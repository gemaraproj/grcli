// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gemaraproj/go-gemara/bundle"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/gemaraproj/grcli/internal/cache"
	"github.com/gemaraproj/grcli/internal/hub"
	"github.com/gemaraproj/grcli/internal/refs"
	"github.com/gemaraproj/grcli/internal/registry"
	"github.com/gemaraproj/grcli/internal/sigverify"
)

const (
	flagSource = "source"
	// flagVersion is the published artifact's metadata.version, which is
	// also its OCI tag (the hub guarantees they're the same). Shared with
	// verify.go.
	flagVersion = "version"

	// Reference-resolution flags.
	flagWithImports    = "with-imports"
	flagWithReferences = "with-references"
	flagNoCache        = "no-cache"

	// flagNoVerify opts out of the default pre-unpack signature
	// verification. flagCertIdentity / flagCertOIDCIssuer are defined in verify.go
	// and reused here so an unpack can assert an identity instead of trusting the
	// hub-recorded one.
	flagNoVerify = "no-verify"
)

func newUnpackCmd(v *viper.Viper) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "unpack",
		Short: "Extract a Gemara bundle from a local OCI layout or remote registry",
		Long: `Reads a Gemara bundle and writes its artifact files to a directory.
The bundle manifest, including any SLSA-shaped provenance record, is
written alongside as bundle.json.

The source can be a local OCI image layout (--source, the shape produced
by 'grcli publish --dry-run') or a remote registry discovered from the
hub (--url plus --repository). Exactly one of --source / --url must be set.

Verification: a remote (--url) unpack VERIFIES the artifact's
Sigstore signature in-process before writing anything, and fails closed —
an unsigned, mis-signed, or unverifiable artifact is refused and no files
are written. This is the same check as 'grcli verify': zero-flag against
the identity the hub recorded at ingest, or --certificate-identity to
assert the signer yourself and bypass the hub. Pass --no-verify to write
without verifying (INSECURE). A local --source layout has no registry
signature to check, so it is always written without verification.

Caching: a remote (--url) fetch is served from a global on-disk
cache when the same namespace/id/version has been fetched before — a cache
hit for the artifact bytes needs no network. grc.store tags are immutable,
so a hit can never be stale. (Best-effort exception: resolving references
makes short-deadline hub lookups for LICENSE METADATA only — the primary's
license baseline, and a one-time lookup for any cached reference whose
license was never confirmed; failures are reported and never block.) Set
$GRCLI_CACHE to relocate the cache; pass --no-cache to force a fresh pull of
the primary artifact (and references) and persist nothing. --source reads
are local and never cached.

Registry auth flows through the same Docker credential chain and
GRCLI_REGISTRY_USERNAME / GRCLI_REGISTRY_PASSWORD / GRCLI_REGISTRY_TOKEN
overrides as 'grcli publish'.

Resolving references: with --with-imports (the artifact's
'imports') or --with-references (every mapping reference it declares),
grcli also pulls the referenced grc.store artifacts into references/
<category>/<ns>/<id>@<version>, alongside a references/index.json record.
A reference whose host is 'grc.store' resolves against your --url target
(so the same reference works against prod, staging, or a local proxy); a
reference to any other host is reported and skipped. Resolution needs a
hub target, so pass --url. Pulled artifacts are cached globally (set
$GRCLI_CACHE to override the location); --no-cache bypasses the cache.
Note: the verification above covers the PRIMARY artifact; pulled references
(--with-imports / --with-references) are NOT signature-verified yet — that
is a forthcoming follow-up. A license that differs from the primary's is
reported as a warning, not an error.

Examples:
  # From a local 'publish --dry-run' output
  grcli unpack --source ./grcli-out --version 1.0.0

  # From a remote registry (via hub discovery)
  grcli unpack --url https://hub.grc.store \
    --repository myorg/my-controls --version 1.0.0

  # Pull the artifact AND the catalogs it imports
  grcli unpack --url https://hub.grc.store \
    --repository myorg/my-controls --version 1.0.0 --with-imports`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUnpack(cmd, v)
		},
	}

	flags := cmd.Flags()
	flags.String(flagSource, "", "OCI image layout directory (mutually exclusive with --url)")
	flags.String(flagURL, defaultURL, "grc.store base URL (discovers the registry)")
	flags.String(flagRepository, "", "repository path within the registry (requires --url)")
	flags.String(flagVersion, "", "artifact version to unpack — the metadata.version of the published bundle (required)")
	flags.String(flagOutput, "grcli-unpacked", "directory to write extracted files to")
	flags.Bool(flagWithImports, false, "also resolve and pull the artifact's `imports` references from the hub (requires --url)")
	flags.Bool(flagWithReferences, false, "also resolve and pull ALL of the artifact's mapping references from the hub (requires --url); superset of --with-imports")
	flags.Bool(flagNoCache, false, "bypass the local artifact cache for this run (primary + references); set cache-enabled: false in config to disable it durably")
	flags.Bool(flagNoVerify, false, "write without verifying the artifact's signature (INSECURE) — the default verifies and fails closed")
	flags.String(flagCertIdentity, "", "verify against this exact signer identity instead of the hub-recorded one (bypasses the hub lookup)")
	flags.String(flagCertOIDCIssuer, "", "expected OIDC issuer for --certificate-identity (default: https://token.actions.githubusercontent.com)")

	// Bind at RunE time, not here — see comment in newPublishCmd.
	return cmd
}

func runUnpack(cmd *cobra.Command, v *viper.Viper) error {
	if err := v.BindPFlags(cmd.Flags()); err != nil {
		return fmt.Errorf("binding flags: %w", err)
	}
	// A bare `grcli unpack --source ...` would otherwise collide with the
	// bake-in --url default; suppress the default so --source alone is not
	// read as "both --source and --url".
	suppressDefaultURLIfExplicit(cmd, v, flagSource)
	ctx := cmd.Context()

	version := v.GetString(flagVersion)
	output := v.GetString(flagOutput)
	out := cmd.OutOrStdout()

	// Capture whether the user supplied an explicit registry credential BEFORE
	// any pull mints and exports one. Reference resolution mints a fresh token
	// per referenced repository (registry tokens are per-namespace), and must
	// only do so when the user hasn't provided their own credential — which is
	// no longer detectable once resolveBundle has exported a primary token.
	userCreds := userSuppliedRegistryCredential()

	// Shared fetch stage (cache-checking pull), identical to `cat`; unpack's
	// last mile is writing the directory.
	unpacked, refLabel, err := resolveBundle(ctx, v, out)
	if err != nil {
		return err
	}

	// Verify the signature BEFORE writing anything. Fail closed:
	// a rejected artifact returns here, so os.MkdirAll/writeBundle never run
	// and the output directory is not created.
	switch planUnpackVerify(v.GetString(flagSource), v.GetBool(flagNoVerify)) {
	case unpackVerify:
		if err := verifyBeforeUnpack(ctx, v, out); err != nil {
			return err
		}
	case unpackSkipSource:
		fmt.Fprintln(out, "  ! --source is a local OCI layout with no registry signature to verify; writing WITHOUT verification")
	case unpackSkipNoVerify:
		fmt.Fprintln(out, "  ! WARNING: --no-verify set — writing WITHOUT signature verification; the artifact's provenance is NOT checked")
	}

	if err := os.MkdirAll(output, 0o755); err != nil {
		return fmt.Errorf("creating output dir: %w", err)
	}

	fmt.Fprintf(out, "unpacked %s:%s → %s (%d files, %d imports)\n",
		refLabel, version, output, len(unpacked.Files), len(unpacked.Imports))
	if err := writeBundle(unpacked, output, out); err != nil {
		return err
	}

	if mode, want := referenceMode(v); want {
		return resolveReferences(ctx, v, mode, unpacked, output, userCreds, out)
	}
	return nil
}

// unpackVerifyPlan is how unpack handles signature verification for one
// invocation, decided from the flags before any network work.
type unpackVerifyPlan int

const (
	unpackVerify       unpackVerifyPlan = iota // verify before writing; fail closed
	unpackSkipSource                           // --source: no registry referrer exists to verify against
	unpackSkipNoVerify                         // --no-verify: caller opted out
)

// planUnpackVerify decides whether unpack verifies, and if not, why. A local
// --source layout has no registry signature referrer, so it cannot be verified
// (this wins even if --no-verify is also set — the reason is just more
// specific); an explicit --no-verify opts out; otherwise unpack verifies and
// fails closed on an unsigned/mis-signed artifact.
func planUnpackVerify(source string, noVerify bool) unpackVerifyPlan {
	switch {
	case source != "":
		return unpackSkipSource
	case noVerify:
		return unpackSkipNoVerify
	default:
		return unpackVerify
	}
}

// verifyBeforeUnpack verifies the artifact's Sigstore signature in-process
// (the same path verify uses) BEFORE any content is written. It fails closed:
// an unsigned, mis-signed, or otherwise unverifiable artifact returns an error
// and unpack writes nothing. It reuses verify's exact policy resolution, so
// unpack and `grcli verify` apply identical trust — zero-flag against the
// hub-recorded identity, or an explicit --certificate-identity the caller
// asserts (bypassing the hub lookup).
func verifyBeforeUnpack(ctx context.Context, v *viper.Viper, out io.Writer) error {
	policy, err := resolveVerifyPolicy(ctx, v)
	if err != nil {
		return fmt.Errorf("preparing verification: %w", err)
	}
	// Mint a pull token for the signature fetch. resolveBundle may have served
	// the content from cache without minting one, so never assume it's exported.
	policy.registryToken, err = ensureRegistryToken(ctx, v.GetString(flagURL), "", v.GetString(flagRepository), []string{"pull"})
	if err != nil {
		return fmt.Errorf("fetching registry pull token: %w", err)
	}
	fmt.Fprintf(out, "verifying signature (%s)\n", policy.modeDescription())
	verifier, err := newSigstoreVerifier(v)
	if err != nil {
		return fmt.Errorf("initializing verifier: %w", err)
	}
	bundleJSON, artifactDigest, err := registry.FetchSignatureBundle(ctx, policy.registryHost, policy.repository, policy.version)
	if err != nil {
		return fmt.Errorf("discovering signature: %w", err)
	}
	res, err := verifier.Verify(ctx, bundleJSON, artifactDigest, policy.identityPolicy())
	if errors.Is(err, sigverify.ErrUnsigned) {
		return fmt.Errorf("%s:%s has no signature in the registry — refusing to unpack unverified content "+
			"(re-run with --no-verify to override)", policy.repository, policy.version)
	}
	if err != nil {
		return fmt.Errorf("signature verification failed — refusing to unpack: %w", err)
	}
	fmt.Fprintf(out, "verified: %s\n", res.Identity)
	return nil
}

// referenceMode reads the --with-references / --with-imports flags.
// --with-references is the superset, so it wins when both are set.
func referenceMode(v *viper.Viper) (refs.Mode, bool) {
	switch {
	case v.GetBool(flagWithReferences):
		return refs.AllReferences, true
	case v.GetBool(flagWithImports):
		return refs.ImportsOnly, true
	default:
		return 0, false
	}
}

// writeBundle writes the bundle's primary files, any imports (under an
// imports/ subdir to avoid collisions), and the bundle manifest as
// bundle.json. Filenames are path-cleaned and rejected if they try to
// escape the output directory.
func writeBundle(b *bundle.Bundle, dir string, out io.Writer) error {
	for _, file := range b.Files {
		name, err := safeWriteFile(dir, file.Name, file.Data)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "  - %s\n", name)
	}
	if len(b.Imports) > 0 {
		importsDir := filepath.Join(dir, "imports")
		if err := os.MkdirAll(importsDir, 0o755); err != nil {
			return fmt.Errorf("creating imports dir: %w", err)
		}
		for _, file := range b.Imports {
			name, err := safeWriteFile(importsDir, file.Name, file.Data)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "  - imports/%s\n", name)
		}
	}
	if !b.Manifest.Empty() {
		manifestBytes, err := json.MarshalIndent(b.Manifest, "", "  ")
		if err != nil {
			return fmt.Errorf("encoding manifest: %w", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "bundle.json"), manifestBytes, 0o644); err != nil {
			return fmt.Errorf("writing manifest: %w", err)
		}
		fmt.Fprintln(out, "  - bundle.json (bundle manifest)")
	}
	return nil
}

// safeWriteFile writes data to dir/name, rejecting names that would
// escape dir via "..", absolute paths, or other traversal tricks.
// Returns the path-cleaned name (relative to dir) on success.
func safeWriteFile(dir, name string, data []byte) (string, error) {
	if name == "" {
		return "", errors.New("bundle file has empty name")
	}
	clean := filepath.Clean(name)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe bundle file name %q", name)
	}
	path := filepath.Join(dir, clean)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return clean, nil
}

// refIndexEntry is one row of references/index.json — a record of a resolved
// reference's provenance, written so a consumer (or a later verify-on-pull
// pass) knows exactly what was pulled and from where.
type refIndexEntry struct {
	Category       string `json:"category"`
	Namespace      string `json:"namespace"`
	CatalogID      string `json:"catalog_id"`
	Version        string `json:"version"`
	SourceURL      string `json:"source_url"`
	ManifestDigest string `json:"manifest_digest,omitempty"`
	ContentDigest  string `json:"content_digest"`
	License        string `json:"license,omitempty"`
	Verified       bool   `json:"verified"`
	Path           string `json:"path"`
}

// resolveReferences walks the unpacked artifact's mapping references and pulls
// the ones that point at the targeted hub into references/<category>/ alongside
// the primary. It is best-effort: an unrecognized host, a not-found,
// or a fetch error is reported and skipped, never fatal.
func resolveReferences(ctx context.Context, v *viper.Viper, mode refs.Mode, b *bundle.Bundle, output string, userCreds bool, out io.Writer) error {
	url := v.GetString(flagURL)
	repository := v.GetString(flagRepository)
	version := v.GetString(flagVersion)

	// Gather selected references across the primary file(s).
	var selected []refs.Selected
	for _, f := range b.Files {
		a, err := refs.Scan(f.Data)
		if err != nil {
			fmt.Fprintf(out, "  ! could not read references in %s: %v\n", f.Name, err)
			continue
		}
		for _, n := range a.Notes {
			fmt.Fprintf(out, "  note: %s\n", n)
		}
		selected = append(selected, a.Select(mode)...)
	}
	if len(selected) == 0 {
		fmt.Fprintln(out, "no resolvable references declared in this artifact")
		return nil
	}

	// Resolution needs a hub target. The local (--source) path has no --url.
	if url == "" {
		fmt.Fprintf(out, "%d reference(s) declared, but resolution needs a hub target — re-run with --url\n", len(selected))
		return nil
	}
	targetHost := hostOf(url)
	if targetHost == "" {
		return fmt.Errorf("could not determine host from --url %q", url)
	}
	client := hub.New(url, "")

	// References pulled from the registry need the registry
	// host, discovered lazily on the FIRST cache miss so a fully-cached run stays
	// offline. Memoized: at most one discovery per unpack, and a failure only
	// skips the references that actually need a pull, not the cached ones.
	var (
		regHost string
		regErr  error
		regDone bool
	)
	resolveRegistryHost := func() (string, error) {
		if !regDone {
			regDone = true
			d, derr := hub.Discover(ctx, url)
			if derr != nil {
				regErr = fmt.Errorf("registry discovery: %w", derr)
			} else {
				regHost = d.RegistryURL
			}
		}
		return regHost, regErr
	}

	// The primary's own coordinate (for the self-reference guard and the
	// license-mismatch baseline). Best-effort: a non-<ns>/<id> --repository
	// just disables these niceties.
	primaryNS, primaryID := splitRepository(repository)
	primaryLicense := primaryLicenseBestEffort(ctx, client, primaryNS, primaryID, version)

	var c *cache.Cache
	if cachingEnabled(v) {
		cc, err := cache.Open()
		if err != nil {
			fmt.Fprintf(out, "  ! cache unavailable, fetching without it: %v\n", err)
		} else {
			c = cc
		}
	}

	seen := make(map[string]bool)
	var index []refIndexEntry
	pulled, skipped := 0, 0

	for _, s := range selected {
		ns, id, ok, reason := refs.Recognize(s.URL, targetHost)
		if !ok {
			fmt.Fprintf(out, "  - skip [%s] %s: %s\n", s.Category, s.URL, reason)
			skipped++
			continue
		}
		coord := fmt.Sprintf("%s/%s@%s", ns, id, s.Version)
		if seen[coord] {
			continue
		}
		seen[coord] = true
		if ns == primaryNS && id == primaryID && s.Version == version {
			continue // the artifact references itself; already unpacked
		}

		entry, err := fetchReference(ctx, fetchRefArgs{
			client: client, cache: c, registryHost: resolveRegistryHost, hubURL: url,
			host: targetHost, ns: ns, id: id, version: s.Version, sourceURL: s.URL,
			userCreds: userCreds,
		}, out)
		if err != nil {
			fmt.Fprintf(out, "  - skip [%s] %s: %v\n", s.Category, coord, err)
			skipped++
			continue
		}
		if primaryLicense != "" && entry.License != "" && primaryLicense != entry.License {
			fmt.Fprintf(out, "  ! license: %s is %s but the primary is %s — review before reuse\n",
				coord, entry.License, primaryLicense)
		}
		if len(entry.Files) == 0 {
			fmt.Fprintf(out, "  - skip [%s] %s: reference bundle has no files\n", s.Category, coord)
			skipped++
			continue
		}

		// A reference is a full bundle, written to its own directory (like the
		// primary unpack): the artifact file(s) plus bundle.json.
		refDir := filepath.Join("references", s.Category, ns, fmt.Sprintf("%s@%s", id, s.Version))
		if err := writeReference(output, refDir, entry, out); err != nil {
			fmt.Fprintf(out, "  - skip [%s] %s: %v\n", s.Category, coord, err)
			skipped++
			continue
		}
		index = append(index, refIndexEntry{
			Category:       s.Category,
			Namespace:      ns,
			CatalogID:      id,
			Version:        s.Version,
			SourceURL:      s.URL,
			ManifestDigest: entry.ManifestDigest,
			ContentDigest:  referenceContentDigest(entry),
			License:        entry.License,
			Verified:       entry.Verified,
			Path:           refDir,
		})
		pulled++
	}

	if len(index) > 0 {
		indexBytes, err := json.MarshalIndent(index, "", "  ")
		if err != nil {
			return fmt.Errorf("encoding references index: %w", err)
		}
		if err := os.WriteFile(filepath.Join(output, "references", "index.json"), indexBytes, 0o644); err != nil {
			return fmt.Errorf("writing references index: %w", err)
		}
		fmt.Fprintln(out, "  - references/index.json")
	}
	fmt.Fprintf(out, "resolved %d reference(s), skipped %d\n", pulled, skipped)
	return nil
}

// fetchRefArgs bundles the inputs to fetchReference (a positional list would be
// error-prone at this width).
type fetchRefArgs struct {
	client *hub.Client
	cache  *cache.Cache
	// registryHost lazily resolves the OCI registry host, so a cache hit never
	// triggers discovery (offline-capable). Called only on a cache miss.
	registryHost func() (string, error)
	hubURL       string
	host         string // cache host key (the hub host)
	ns, id       string
	version      string
	sourceURL    string
	userCreds    bool
}

// fetchReference returns a reference as a full bundle, from the cache when
// present and uncorrupted, otherwise by pulling the whole bundle from the
// registry and, unless --no-cache, caching it. The
// per-version license is read from the hub for the license-mismatch warning and
// recorded on the entry. Verification is deferred, so the
// entry is recorded as unverified.
func fetchReference(ctx context.Context, a fetchRefArgs, out io.Writer) (*cache.Entry, error) {
	if a.cache != nil {
		e, found, err := a.cache.Get(a.host, a.ns, a.id, a.version)
		if err != nil {
			fmt.Fprintf(out, "  ! cache: %v (re-fetching)\n", err)
		} else if found {
			// An entry can lack a license: a primary fetch caches with license
			// "" (it makes no catalog lookup), and a reference cached during a
			// hub outage recorded "" too. Heal on hit — look the license up
			// live and upgrade the entry in place — but at most once: a
			// SUCCESSFUL lookup sets LicenseChecked even when the catalog
			// genuinely records no license, so a license-less coordinate does
			// not pay a hub call on every future hit. Only a FAILED lookup
			// leaves LicenseChecked unset for a retry on the next run.
			if e.License == "" && !e.LicenseChecked {
				if license, ok := referenceLicense(ctx, a.client, a.ns, a.id, a.version, out); ok {
					e.License = license
					e.LicenseChecked = true
					if perr := a.cache.Put(a.host, a.ns, a.id, a.version, *e); perr != nil {
						fmt.Fprintf(out, "  ! cache write failed (continuing): %v\n", perr)
					}
				}
			}
			return e, nil
		}
	}

	// License is best-effort hub metadata for the mismatch warning; a lookup
	// failure doesn't block the pull (the bundle stands on its own) and does
	// not poison the cache permanently — the hit path above retries unchecked
	// entries (once per run) until a lookup succeeds.
	license, licenseChecked := referenceLicense(ctx, a.client, a.ns, a.id, a.version, out)

	registryHost, err := a.registryHost()
	if err != nil {
		return nil, err
	}
	repo := a.ns + "/" + a.id
	if err := mintRefPullToken(ctx, a.hubURL, repo, a.userCreds); err != nil {
		return nil, fmt.Errorf("fetching registry pull token: %w", err)
	}
	b, err := registry.UnpackRemote(ctx, registryHost, repo, a.version)
	if err != nil {
		return nil, err
	}
	// Reference resolution is direct-only: if the referenced bundle
	// carries its own transitive imports, we neither materialize nor cache them
	// (the v2 entry stores Files + manifest only). Say so rather than dropping
	// them silently.
	if len(b.Imports) > 0 {
		noteDroppedReferenceImports(out, a.ns, a.id, a.version, len(b.Imports))
	}
	e, err := entryFromBundle(b, license, a.sourceURL)
	if err != nil {
		return nil, err
	}
	e.LicenseChecked = licenseChecked
	// Persist for reuse, unless the bundle carries the dormant Imports slot the
	// v2 entry format can't represent (see putBundle) — then serve, don't cache.
	if a.cache != nil && len(b.Imports) == 0 {
		if err := a.cache.Put(a.host, a.ns, a.id, a.version, e); err != nil {
			fmt.Fprintf(out, "  ! cache write failed (continuing): %v\n", err)
		}
	}
	return &e, nil
}

// referenceLicense looks up a reference's per-version publication license from
// the hub for the license-mismatch warning and references/index.json. ok=false
// means the LOOKUP failed (license unknown — reported, since a silently-missing
// license suppresses the mismatch warning); ok=true with license "" means the
// catalog genuinely records none for that version. The call is best-effort
// metadata, so it gets a short deadline: it must never stall a resolution that
// is otherwise served from cache.
func referenceLicense(ctx context.Context, client *hub.Client, ns, id, version string, out io.Writer) (license string, ok bool) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cat, err := client.GetCatalog(ctx, ns, id)
	if err != nil {
		fmt.Fprintf(out, "  ! license lookup failed for %s/%s@%s (mismatch warning unavailable): %v\n",
			ns, id, version, err)
		return "", false
	}
	if rel := cat.ReleaseFor(version); rel != nil {
		return rel.License, true
	}
	return "", true
}

// noteDroppedReferenceImports warns that a referenced bundle carries its own
// transitive imports, which grcli does not materialize: reference resolution is
// direct-only, and the v2 cache stores Files + manifest only.
func noteDroppedReferenceImports(out io.Writer, ns, id, version string, n int) {
	fmt.Fprintf(out, "  ! %s/%s@%s carries %d transitive import(s) — not materialized (direct-only resolution)\n",
		ns, id, version, n)
}

// writeReference writes a reference bundle's files (and bundle.json, when
// present) into refDir under output.
func writeReference(output, refDir string, e *cache.Entry, out io.Writer) error {
	// File names come from the REMOTE bundle. safeWriteFile only guards escape
	// from the output root, so a name with ../ segments could climb out of
	// refDir and overwrite the primary's files. Validate EVERY name before
	// writing ANY, so a hostile name later in the list can't leave earlier
	// files orphaned on disk when the reference is rejected.
	rels := make([]string, len(e.Files))
	for i, f := range e.Files {
		rel, err := refRelPath(refDir, f.Name)
		if err != nil {
			return err
		}
		rels[i] = rel
	}
	for i, f := range e.Files {
		written, err := safeWriteFile(output, rels[i], f.Data)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "  - %s\n", written)
	}
	if len(e.Manifest) > 0 {
		written, err := safeWriteFile(output, filepath.Join(refDir, "bundle.json"), e.Manifest)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "  - %s\n", written)
	}
	return nil
}

// refRelPath joins a reference bundle's file name onto the reference's own
// directory, rejecting any name whose cleaned path escapes (or resolves to)
// that directory — the name is remote-controlled, and without this check a
// ../-laden name could overwrite the primary's unpacked files elsewhere in
// the output tree.
func refRelPath(refDir, name string) (string, error) {
	if name == "" {
		return "", errors.New("reference file has empty name")
	}
	joined := filepath.Clean(filepath.Join(refDir, name))
	if joined == refDir || !strings.HasPrefix(joined, refDir+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe reference file name %q", name)
	}
	return joined, nil
}

// referenceContentDigest is the index's stable content identifier for a
// reference: the digest of its bundle.json manifest when present (the single
// document that captures the whole bundle), else the sole file's digest, else —
// for a multi-file bundle with no manifest — the digest of the ordered
// (name, per-file digest) list, so the index never records an empty
// content_digest (a later verify pass needs something to check against). The
// OCI identity is recorded separately as ManifestDigest.
func referenceContentDigest(e *cache.Entry) string {
	switch {
	case len(e.Manifest) > 0:
		return cache.Digest(e.Manifest)
	case len(e.Files) == 1:
		return cache.Digest(e.Files[0].Data)
	case len(e.Files) > 1:
		var b strings.Builder
		for _, f := range e.Files {
			b.WriteString(f.Name)
			b.WriteByte(0)
			b.WriteString(cache.Digest(f.Data))
			b.WriteByte('\n')
		}
		return cache.Digest([]byte(b.String()))
	default:
		return ""
	}
}

// mintRefPullToken exports a pull token scoped to repo for the next registry
// pull. It bypasses ensureRegistryToken's "already set" short-circuit because
// GRCLI_REGISTRY_TOKEN may hold a token scoped to a DIFFERENT repo (the
// primary's, or a prior reference's) pulled earlier in this run. When the user
// supplied their own credential, that wins and we touch nothing.
func mintRefPullToken(ctx context.Context, hubURL, repo string, userCreds bool) error {
	if userCreds || hubURL == "" {
		return nil
	}
	tok, err := hub.FetchRegistryToken(ctx, hubURL, "", repo, []string{"pull"})
	if err != nil {
		return err
	}
	if tok != "" {
		_ = os.Setenv("GRCLI_REGISTRY_TOKEN", tok)
	}
	return nil
}

// userSuppliedRegistryCredential reports whether the user set an explicit
// registry credential via env, captured before any pull mints its own token.
func userSuppliedRegistryCredential() bool {
	if os.Getenv("GRCLI_REGISTRY_TOKEN") != "" {
		return true
	}
	return os.Getenv("GRCLI_REGISTRY_USERNAME") != "" && os.Getenv("GRCLI_REGISTRY_PASSWORD") != ""
}

// primaryLicenseBestEffort returns the primary artifact's publication license
// for the mismatch warning, or "" if it can't be determined (no hub baseline,
// then no warnings are emitted). Like referenceLicense, it is best-effort
// metadata on a short deadline: it runs on every reference resolution — even a
// fully cache-served one — and must never stall it.
func primaryLicenseBestEffort(ctx context.Context, client *hub.Client, ns, id, version string) string {
	if ns == "" || id == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cat, err := client.GetCatalog(ctx, ns, id)
	if err != nil {
		return ""
	}
	if rel := cat.ReleaseFor(version); rel != nil {
		return rel.License
	}
	return ""
}

// splitRepository splits an <ns>/<id> repository path into its parts. A path
// that isn't exactly two segments yields empty strings (disabling the
// self-reference guard and license baseline rather than guessing).
func splitRepository(repository string) (ns, id string) {
	parts := strings.Split(strings.Trim(repository, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", ""
	}
	return parts[0], parts[1]
}

// hostOf returns the host of a hub base URL, tolerating a missing scheme.
func hostOf(rawURL string) string {
	u, err := neturl.Parse(rawURL)
	if err == nil && u.Host != "" {
		return u.Host
	}
	// Scheme-less value (e.g. "hub.grc.store" or "hub.grc.store/x"): take the
	// first path segment as the host.
	return strings.Split(strings.TrimRight(rawURL, "/"), "/")[0]
}
