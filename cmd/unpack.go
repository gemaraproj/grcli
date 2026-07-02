// SPDX-License-Identifier: LicenseRef-Revanite-Proprietary

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

	"github.com/gemaraproj/go-gemara/bundle"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/revanite-io/grcli/internal/cache"
	"github.com/revanite-io/grcli/internal/hub"
	"github.com/revanite-io/grcli/internal/refs"
	"github.com/revanite-io/grcli/internal/registry"
)

const (
	flagSource = "source"
	// flagVersion is the published artifact's metadata.version, which is
	// also its OCI tag (ADR-0033 guarantees they're the same). Shared with
	// verify.go.
	flagVersion = "version"

	// Reference-resolution flags (ADR-0039).
	flagWithImports    = "with-imports"
	flagWithReferences = "with-references"
	flagNoCache        = "no-cache"
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

Registry auth flows through the same Docker credential chain and
GRCLI_REGISTRY_USERNAME / GRCLI_REGISTRY_PASSWORD / GRCLI_REGISTRY_TOKEN
overrides as 'grcli publish'.

Resolving references (ADR-0039): with --with-imports (the artifact's
'imports') or --with-references (every mapping reference it declares),
grcli also pulls the referenced grc.store artifacts into references/
<category>/<ns>/<id>@<version>, alongside a references/index.json record.
A reference whose host is 'grc.store' resolves against your --url target
(so the same reference works against prod, staging, or a local proxy); a
reference to any other host is reported and skipped. Resolution needs a
hub target, so pass --url. Pulled artifacts are cached globally (set
$GRCLI_CACHE to override the location); --no-cache bypasses the cache.
Note: in this release pulled references are NOT signature-verified yet —
that is a forthcoming follow-up. A license that differs from the primary's
is reported as a warning, not an error.

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
	flags.Bool(flagNoCache, false, "bypass the local artifact cache when resolving references (fresh fetch, nothing persisted)")

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

	source := v.GetString(flagSource)
	url := v.GetString(flagURL)
	repository := v.GetString(flagRepository)
	version := v.GetString(flagVersion)
	output := v.GetString(flagOutput)

	if version == "" {
		return errors.New("--version is required")
	}
	switch {
	case source == "" && url == "":
		return errors.New("either --source or --url is required")
	case source != "" && url != "":
		return errors.New("--source is mutually exclusive with --url")
	}

	var (
		unpacked *bundle.Bundle
		refLabel string
		err      error
	)
	if source != "" {
		unpacked, err = registry.UnpackLocal(ctx, source, version)
		refLabel = source
	} else {
		if repository == "" {
			return errors.New("--repository is required when --url is set")
		}
		d, derr := hub.Discover(ctx, url)
		if derr != nil {
			return fmt.Errorf("hub discovery: %w", derr)
		}
		// Keep the advertised scheme: registryHost is the oras dial
		// target and newRemoteRepo derives PlainHTTP from it, so stripping
		// http:// here would force HTTPS against a plain-HTTP zot. The
		// display label below normalizes to a bare host.
		registryHost := d.RegistryURL
		// ADR-0031: the registry requires a token even for reads. Reads
		// are public, so mint an anonymous pull token from the hub and
		// export it for the oras pull.
		if _, terr := ensureRegistryToken(ctx, url, "", repository, []string{"pull"}); terr != nil {
			return fmt.Errorf("fetching registry pull token: %w", terr)
		}
		unpacked, err = registry.UnpackRemote(ctx, registryHost, repository, version)
		refLabel = registry.NormalizeRegistryHost(registryHost) + "/" + repository
	}
	if err != nil {
		return err
	}

	if err := os.MkdirAll(output, 0o755); err != nil {
		return fmt.Errorf("creating output dir: %w", err)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "unpacked %s:%s → %s (%d files, %d imports)\n",
		refLabel, version, output, len(unpacked.Files), len(unpacked.Imports))
	if err := writeBundle(unpacked, output, out); err != nil {
		return err
	}

	if mode, want := referenceMode(v); want {
		return resolveReferences(ctx, v, mode, unpacked, output, out)
	}
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
// the primary (ADR-0039). It is best-effort: an unrecognized host, a not-found,
// or a fetch error is reported and skipped, never fatal.
func resolveReferences(ctx context.Context, v *viper.Viper, mode refs.Mode, b *bundle.Bundle, output string, out io.Writer) error {
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

	// The primary's own coordinate (for the self-reference guard and the
	// license-mismatch baseline). Best-effort: a non-<ns>/<id> --repository
	// just disables these niceties.
	primaryNS, primaryID := splitRepository(repository)
	primaryLicense := primaryLicenseBestEffort(ctx, client, primaryNS, primaryID, version)

	var c *cache.Cache
	if !v.GetBool(flagNoCache) {
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

		entry, err := fetchReference(ctx, client, c, targetHost, ns, id, s.Version, s.URL, out)
		if err != nil {
			fmt.Fprintf(out, "  - skip [%s] %s: %v\n", s.Category, coord, err)
			skipped++
			continue
		}
		if primaryLicense != "" && entry.License != "" && primaryLicense != entry.License {
			fmt.Fprintf(out, "  ! license: %s is %s but the primary is %s — review before reuse\n",
				coord, entry.License, primaryLicense)
		}

		// The hub serves a reference as a single JSON body, cached as a
		// one-file bundle (Phase 4 will pull full multi-file bundles).
		if len(entry.Files) == 0 {
			fmt.Fprintf(out, "  - skip [%s] %s: empty reference body\n", s.Category, coord)
			skipped++
			continue
		}
		body := entry.Files[0].Data
		rel := filepath.Join("references", s.Category, ns, fmt.Sprintf("%s@%s.json", id, s.Version))
		written, err := safeWriteFile(output, rel, body)
		if err != nil {
			fmt.Fprintf(out, "  - skip [%s] %s: %v\n", s.Category, coord, err)
			skipped++
			continue
		}
		fmt.Fprintf(out, "  - %s\n", written)
		index = append(index, refIndexEntry{
			Category:       s.Category,
			Namespace:      ns,
			CatalogID:      id,
			Version:        s.Version,
			SourceURL:      s.URL,
			ManifestDigest: entry.ManifestDigest,
			ContentDigest:  cache.Digest(body),
			License:        entry.License,
			Verified:       entry.Verified,
			Path:           written,
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

// fetchReference returns a reference's bytes, from the cache when present and
// uncorrupted, otherwise by fetching from the hub and (unless --no-cache)
// caching the result. Verification is deferred (ADR-0039 amendment), so the
// entry is recorded as unverified.
func fetchReference(ctx context.Context, client *hub.Client, c *cache.Cache, host, ns, id, version, sourceURL string, out io.Writer) (*cache.Entry, error) {
	if c != nil {
		e, found, err := c.Get(host, ns, id, version)
		if err != nil {
			fmt.Fprintf(out, "  ! cache: %v (re-fetching)\n", err)
		} else if found {
			return e, nil
		}
	}

	cat, err := client.GetCatalog(ctx, ns, id)
	if err != nil {
		return nil, fmt.Errorf("hub lookup: %w", err)
	}
	license := ""
	if rel := cat.ReleaseFor(version); rel != nil {
		license = rel.License
	}
	body, manifestDigest, err := client.GetVersionBody(ctx, ns, id, version)
	if err != nil {
		return nil, err
	}
	// The hub serves the artifact body as a single JSON document; cache it as a
	// one-file bundle with no manifest (Phase 4 will pull full bundles).
	e := &cache.Entry{
		Files:          []cache.File{{Name: id + ".json", Data: body}},
		ManifestDigest: manifestDigest,
		License:        license,
		SourceURL:      sourceURL,
		Verified:       false, // verify-on-pull is deferred
	}
	if c != nil {
		if err := c.Put(host, ns, id, version, *e); err != nil {
			fmt.Fprintf(out, "  ! cache write failed (continuing): %v\n", err)
		}
	}
	return e, nil
}

// primaryLicenseBestEffort returns the primary artifact's publication license
// for the mismatch warning, or "" if it can't be determined (no hub baseline,
// then no warnings are emitted).
func primaryLicenseBestEffort(ctx context.Context, client *hub.Client, ns, id, version string) string {
	if ns == "" || id == "" {
		return ""
	}
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
