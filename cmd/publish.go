// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/revanite-io/grc-store-protocol/discovery"
	"github.com/revanite-io/grc-store-protocol/spdx"

	"github.com/gemaraproj/grc-store-clientkit/auth"
	"github.com/gemaraproj/grc-store-clientkit/bundle"
	"github.com/gemaraproj/grc-store-clientkit/hub"
	"github.com/gemaraproj/grc-store-clientkit/keyless"
	"github.com/gemaraproj/grc-store-clientkit/provenance"
	"github.com/gemaraproj/grcli/internal/digest"
	"github.com/gemaraproj/grcli/internal/sign"
	"github.com/gemaraproj/grcli/internal/source"
)

// Flag names are declared once so the compiler catches typos at every
// viper.Get call site. publish does not expose a tag/version flag —
// the OCI tag is always metadata.version. unpack and verify
// take --version (see flagVersion in unpack.go) to address a published
// bundle.
const (
	flagFile       = "file"
	flagURL        = "url"
	flagRepository = "repository"
	flagToken      = "token"
	flagDryRun     = "dry-run"
	flagOutput     = "output"
	flagNoSign     = "no-sign"
	flagCosignKey  = "cosign-key"
	flagLicense    = "license"
)

func newPublishCmd(v *viper.Viper) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "publish [file...]",
		Short: "Bundle one Gemara artifact with provenance and push it to grc.store",
		Long: `Loads the file(s) describing a single artifact, attaches a SLSA-shaped
provenance record, packs an OCI bundle, pushes it to the configured
registry, signs it, and notifies the hub via POST /v1/bundles/sync.

Files can be supplied as positional arguments (grcli publish a.yaml
b.yaml) or via -f / --file. The two forms are mutually exclusive —
mixing them is an error so neither silently wins.

Use --dry-run to write the bundle to an OCI image layout on disk
instead of touching any network.

Signing is keyless by default: in GitHub Actions the workflow's OIDC
token is used (permissions: id-token: write); elsewhere set
SIGSTORE_ID_TOKEN to an OIDC token with audience "sigstore", or a browser
window opens to sign in to the public-good Sigstore issuer. --cosign-key
signs with a local key via cosign instead; --no-sign publishes unsigned.

Auth in GitHub Actions: no GitHub secret, no --token, no GRCLI_TOKEN —
when run inside a workflow with permissions: id-token: write, grcli
mints a GitHub Actions OIDC token and presents it as the credential
(trusted publishing). The repo (owner/repo, optionally pinned
to a ref) must be registered as a trusted publisher on the hub for the
target namespace; a 403 means that binding is missing — not that you
need to set a secret.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPublish(cmd, v, args)
		},
	}

	flags := cmd.Flags()
	flags.StringSliceP(flagFile, "f", nil, "input file(s) describing one artifact (repeatable; comma-separated also accepted)")
	flags.String(flagURL, defaultURL, "grc.store base URL — discovers the registry and is the hub sync target")
	flags.String(flagRepository, "", "repository path within the registry (default: <author.id>/<metadata.id>, slugified to [a-z0-9._-])")
	flags.String(flagToken, "", "bearer token for the hub sync call (or GRCLI_TOKEN); leave unset in GitHub Actions — the workflow's OIDC token is used automatically (trusted publishing, no GitHub secret needed)")
	flags.Bool(flagDryRun, false, "skip all network — emit OCI layout to --output instead")
	flags.String(flagOutput, "grcli-out", "directory to write the OCI layout to when --dry-run")
	flags.Bool(flagNoSign, false, "publish unsigned (the hub rejects unsigned catalogs at ingest)")
	flags.String(flagCosignKey, "", "cosign key file for key-based signing instead of keyless (or COSIGN_KEY)")
	flags.String(flagLicense, "", "REQUIRED: publication license as an SPDX expression (e.g. Apache-2.0, MIT OR Apache-2.0, LicenseRef-Acme-Proprietary); stamped as the org.opencontainers.image.licenses OCI annotation. Publish fails before any network call if unset")

	// Flags are bound to viper inside RunE (see runPublish) rather than
	// here at construction time. Two subcommands sharing a viper instance
	// (e.g. publish + unpack both defining --output) would otherwise
	// clobber each other's bindings — viper keys are global per instance.
	//
	// COSIGN_KEY is the conventional env name for the cosign key path;
	// override the GRCLI_ prefix so existing cosign users see it picked up.
	_ = v.BindEnv(flagCosignKey, "COSIGN_KEY")

	return cmd
}

// publishTarget holds the resolved push destination after flags, config,
// and artifact metadata defaults are merged.
type publishTarget struct {
	registryHost string // the advertised registry_url, scheme kept
	repository   string
	tag          string
	dryRun       bool
	output       string
}

func runPublish(cmd *cobra.Command, v *viper.Viper, positional []string) error {
	if err := v.BindPFlags(cmd.Flags()); err != nil {
		return fmt.Errorf("binding flags: %w", err)
	}
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	startedOn := time.Now().UTC()

	flagFiles := expandCommas(v.GetStringSlice(flagFile))
	files, err := mergeFileSources(flagFiles, positional)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return errors.New("no input files: pass paths positionally (grcli publish a.yaml) or via --file")
	}

	loaded, err := source.Load(ctx, files)
	if err != nil {
		return err
	}

	target, err := resolveTarget(ctx, v, loaded)
	if err != nil {
		return err
	}

	// Strict license gate: grcli is the strict end. --license is REQUIRED.
	// Validate and canonicalize BEFORE any pack/push — including the
	// --dry-run path — so a missing, malformed, or unknown SPDX expression
	// never produces OCI bytes (locally or in the registry).
	canonicalLicense, err := validatePublishLicense(v.GetString(flagLicense))
	if err != nil {
		return err
	}

	// Every preflight runs BEFORE packing, so a misconfiguration never
	// leaves orphaned bytes in the registry: signing material (the keyless
	// identity is resolved here — in a terminal that may open a browser),
	// the immutable-version check, the hub login, and a push-granting
	// registry token.
	var (
		plan   signPlan
		bearer string
		reg    bundle.Registry
	)
	if !target.dryRun {
		if plan, err = planSigning(ctx, v, cmd.ErrOrStderr()); err != nil {
			return err
		}
		if err := checkVersionAvailable(ctx, v, target.repository, target.tag); err != nil {
			return err
		}
		if bearer, err = resolveBearerToken(ctx, v); err != nil {
			return err
		}
		if reg, err = pushRegistry(ctx, v, bearer, target.repository); err != nil {
			return err
		}
	}

	predicate := provenance.Build(provenance.Input{
		Tool:           "grcli",
		ToolVersion:    version,
		StartedOn:      startedOn,
		ArtifactType:   loaded.Type,
		ArtifactID:     loaded.ID,
		ArtifactName:   loaded.Filename,
		ArtifactDigest: digest.Bytes(loaded.Body),
		SourceFiles:    loaded.SourceDigests,
		Registry:       reg.Host,
		Repository:     target.repository,
		Tag:            target.tag,
	})
	in := bundle.Input{
		Filename:      loaded.Filename,
		ArtifactType:  loaded.Type,
		ArtifactID:    loaded.ID,
		GemaraVersion: loaded.GemaraVersion,
		Body:          loaded.Body,
		Provenance:    predicate,
		License:       canonicalLicense,
	}

	if target.dryRun {
		res, err := bundle.PushLocal(ctx, target.output, target.tag, in)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "dry-run: wrote bundle to %s\n  manifest digest: %s\n  body digest:     %s\n  artifact: %s/%s\n",
			res.Reference, res.ManifestDigest, res.BodyDigest, loaded.Type, loaded.ID)
		return nil
	}

	res, err := reg.Push(ctx, target.repository, target.tag, in)
	if err != nil {
		return fmt.Errorf("push: %w", err)
	}
	fmt.Fprintf(out, "pushed %s\n  manifest digest: %s\n", res.Reference, res.ManifestDigest)

	status, err := plan.apply(ctx, reg, target.repository, res, predicate)
	if err != nil {
		return fmt.Errorf("sign: %w", err)
	}
	fmt.Fprintln(out, status)

	syncResp, err := hub.New(publishHubURL(v), bearer).SyncBundle(ctx, target.repository, target.tag)
	if err != nil {
		return fmt.Errorf("hub sync: %w", err)
	}
	fmt.Fprintf(out, "hub indexed %s:%s — %d artifacts (%d new), types=%s\n",
		syncResp.Repository, syncResp.Tag, syncResp.ArtifactCount, syncResp.NewCount, strings.Join(syncResp.Types, ","))
	return nil
}

// signPlan is the signing decision, made before any bytes move.
type signPlan struct {
	mode    sign.Mode
	keyPath string
	idToken string // Fulcio identity for keyless; never the hub bearer
}

// planSigning resolves how this publish signs: --no-sign, --cosign-key
// (cosign on PATH, version-gated), or keyless (identity resolved now via
// SIGSTORE_ID_TOKEN, GitHub Actions, or an interactive sign-in; a
// non-terminal with neither fails here, not after the push).
func planSigning(ctx context.Context, v *viper.Viper, promptOut io.Writer) (signPlan, error) {
	if v.GetBool(flagNoSign) {
		return signPlan{mode: sign.ModeSkipped}, nil
	}
	if key := v.GetString(flagCosignKey); key != "" {
		if err := sign.KeyPreflight(ctx); err != nil {
			return signPlan{}, err
		}
		return signPlan{mode: sign.ModeKey, keyPath: key}, nil
	}
	tok, err := keyless.Identity(ctx, keyless.PublicGoodAudience, promptOut)
	if err != nil {
		return signPlan{}, fmt.Errorf("keyless signing: %w — or pass --cosign-key for key-based signing, or --no-sign", err)
	}
	return signPlan{mode: sign.ModeKeyless, idToken: tok}, nil
}

// apply signs the pushed bundle per the plan and returns the status line.
func (p signPlan) apply(ctx context.Context, reg bundle.Registry, repository string, res *bundle.Result, predicate provenance.Predicate) (string, error) {
	switch p.mode {
	case sign.ModeSkipped:
		return "signing skipped: --no-sign", nil
	case sign.ModeKey:
		err := sign.SignWithKey(ctx, sign.KeyOptions{
			KeyPath: p.keyPath, Reference: res.Reference, PlainHTTP: reg.PlainHTTP, RegistryToken: reg.Token,
		})
		return "signed (key)", err
	default:
		signer := &keyless.Signer{IDToken: p.idToken}
		_, attested, err := reg.SignAndAttach(ctx, repository, res, signer, predicate)
		if err != nil {
			return "", err
		}
		if !attested {
			return "signed (keyless)", nil
		}
		return "signed (keyless), provenance attested", nil
	}
}

// pushRegistry resolves the registry dial target from hub discovery and a
// push-capable credential: a manual GRC_STORE_REGISTRY_* override (the
// GRCLI_REGISTRY_* names still work for one release), else a repository-
// scoped token minted from the hub with the caller's login. A pull-only
// grant — the caller does not own the namespace — fails here, before
// packing.
func pushRegistry(ctx context.Context, v *viper.Viper, bearer, repository string) (bundle.Registry, error) {
	hubURL := publishHubURL(v)
	d, err := hub.Discover(ctx, hubURL)
	if err != nil {
		return bundle.Registry{}, fmt.Errorf("hub discovery: %w", err)
	}
	host, plainHTTP, err := hub.Registry(d)
	if err != nil {
		return bundle.Registry{}, fmt.Errorf("hub discovery: %w", err)
	}
	reg := bundle.Registry{Host: host, PlainHTTP: plainHTTP}

	if tok := registryTokenOverride(); tok != "" {
		reg.Token = tok
		return reg, nil
	}
	if registryBasicAuthOverride() {
		return reg, nil // clientkit reads GRC_STORE_REGISTRY_USERNAME/PASSWORD itself
	}
	tok, err := hub.New(hubURL, bearer).RegistryToken(ctx, repository, []string{"pull", "push"})
	if err != nil {
		if errors.Is(err, hub.ErrUnauthorized) {
			return bundle.Registry{}, fmt.Errorf("minting a registry push token: %w — %s again", err, grcliApp.LoginHint())
		}
		return bundle.Registry{}, fmt.Errorf("minting a registry push token: %w", err)
	}
	if !tok.GrantsPush() {
		ns, _, _ := strings.Cut(repository, "/")
		return bundle.Registry{}, fmt.Errorf("the hub granted pull-only access to %s — publishing needs ownership of namespace %q (or hub admin); check the namespace on the hub or pass --repository", repository, ns)
	}
	reg.Token = tok.Token
	return reg, nil
}

// registryTokenOverride returns a user-supplied registry bearer, honouring
// the deprecated GRCLI_ spelling with a warning.
func registryTokenOverride() string {
	if t := os.Getenv(bundle.RegistryTokenEnv); t != "" {
		return t
	}
	if t := os.Getenv("GRCLI_REGISTRY_TOKEN"); t != "" {
		fmt.Fprintf(os.Stderr, "warning: GRCLI_REGISTRY_TOKEN is deprecated; use %s\n", bundle.RegistryTokenEnv)
		return t
	}
	return ""
}

// registryBasicAuthOverride reports a user-supplied username/password pair,
// mapping the deprecated GRCLI_ spelling onto the shared names so the
// clientkit registry client picks it up.
func registryBasicAuthOverride() bool {
	if os.Getenv(bundle.RegistryUsernameEnv) != "" && os.Getenv(bundle.RegistryPasswordEnv) != "" {
		return true
	}
	if u, p := os.Getenv("GRCLI_REGISTRY_USERNAME"), os.Getenv("GRCLI_REGISTRY_PASSWORD"); u != "" && p != "" {
		fmt.Fprintf(os.Stderr, "warning: GRCLI_REGISTRY_USERNAME/PASSWORD are deprecated; use %s/%s\n", bundle.RegistryUsernameEnv, bundle.RegistryPasswordEnv)
		_ = os.Setenv(bundle.RegistryUsernameEnv, u)
		_ = os.Setenv(bundle.RegistryPasswordEnv, p)
		return true
	}
	return false
}

// resolveTarget merges --repository/--url/--dry-run with the
// metadata-derived defaults and validates the combination. --url drives
// the registry hostname via the hub's discovery endpoint;
// --dry-run skips discovery since it never touches the network.
//
// The OCI tag is always metadata.version — no override. The hub
// enforces tag == metadata.version as an invariant; a --tag override could only ever produce a 422
// tag_version_mismatch from the syncer, so the flag was removed rather
// than left as a foot-gun.
func resolveTarget(ctx context.Context, v *viper.Viper, loaded *source.Loaded) (publishTarget, error) {
	tag := loaded.Version
	if tag == "" {
		return publishTarget{}, errors.New("could not determine tag — metadata.version is required")
	}
	repository := cmp.Or(v.GetString(flagRepository), defaultRepository(loaded.AuthorID, loaded.ID))
	if repository == "" {
		return publishTarget{}, errors.New("could not determine --repository — set it explicitly or populate metadata.author.id + metadata.id")
	}

	url := v.GetString(flagURL)
	dryRun := v.GetBool(flagDryRun)

	var registryHost string
	if url != "" && !dryRun {
		d, err := hub.Discover(ctx, url)
		if err != nil {
			return publishTarget{}, fmt.Errorf("hub discovery: %w", err)
		}
		registryHost = d.RegistryURL
	}

	target := publishTarget{
		registryHost: registryHost,
		repository:   repository,
		tag:          tag,
		dryRun:       dryRun,
		output:       v.GetString(flagOutput),
	}
	if !target.dryRun && target.registryHost == "" {
		return publishTarget{}, errors.New("--url is required (use --dry-run to skip push)")
	}
	return target, nil
}

// checkVersionAvailable is the publish pre-flight. Versions are
// immutable, so if the target coordinate already exists on the hub, halt
// here — before packing, before any registry write. That prevents a
// re-publish from clobbering the existing bytes in the registry (which
// accepts the overwrite before the hub's sync-time guard can reject it).
// No-op when --repository isn't a plain <namespace>/<id> coordinate; the
// server-side sync guard remains the backstop.
func checkVersionAvailable(ctx context.Context, v *viper.Viper, repository, tag string) error {
	hubBaseURL := publishHubURL(v)
	if hubBaseURL == "" {
		return nil
	}
	ns, cid, ok := strings.Cut(repository, "/")
	if !ok || ns == "" || cid == "" || strings.Contains(cid, "/") {
		return nil
	}
	status, err := hub.New(hubBaseURL, "").VersionExists(ctx, ns, cid, tag)
	if err != nil {
		return fmt.Errorf("checking whether %s:%s already exists: %w", repository, tag, err)
	}
	switch status {
	case hub.VersionPresent:
		return fmt.Errorf("%s:%s already exists — versions are immutable; bump the version (or yank it first)", repository, tag)
	case hub.VersionTombstoned:
		return fmt.Errorf("%s:%s was yanked and cannot be republished — publish a new version", repository, tag)
	default:
		return nil
	}
}

// publishHubURL returns the hub base URL for the publish run (--url).
func publishHubURL(v *viper.Viper) string {
	return v.GetString(flagURL)
}

// resolveBearerToken wraps auth.Resolve with the publish command's glue.
// Resolution order: --token / GRCLI_TOKEN > GitHub Actions OIDC (the
// trusted-publishing bearer, audience from discovery) > stored device-login
// creds. Discovery failures are swallowed — the worst case is that
// auth.Resolve has no store key and returns ErrNoToken with the same
// "run grcli login" hint.
func resolveBearerToken(ctx context.Context, v *viper.Viper) (string, error) {
	in := auth.ResolveInput{
		App:           grcliApp,
		ExplicitToken: v.GetString(flagToken),
		Warn:          os.Stderr,
	}
	url := v.GetString(flagURL)
	var disco *discovery.Document
	if url != "" {
		disco, _ = hub.Discover(ctx, url)
	}
	if in.ExplicitToken == "" {
		tok, inCI, err := hub.CIBearer(ctx, url, disco)
		switch {
		case inCI && err == nil:
			return tok, nil
		case err != nil:
			fmt.Fprintf(os.Stderr, "warning: GitHub Actions OIDC token unavailable, falling back: %v\n", err)
		}
	}
	if disco != nil {
		in.Issuer = disco.OIDCIssuer
		in.ClientID = disco.OIDCCLIClientID
	}
	if store, err := auth.NewDefaultStore(grcliApp); err == nil {
		in.Store = store
	}
	return auth.Resolve(ctx, in)
}

// validatePublishLicense runs the strict SPDX gate for --license (grcli is
// the strict end; the hub is lenient). The flag is REQUIRED: an
// empty/whitespace-only value is an error — distinct from the invalid-value
// message, because a missing flag and a malformed value are different user
// mistakes. The returned string is the canonical SPDX spelling.
func validatePublishLicense(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("a publication license is required: pass --license with an SPDX expression (e.g. Apache-2.0, MIT OR Apache-2.0; see https://spdx.org/licenses) or a LicenseRef-… token for a custom/proprietary license")
	}
	canonical, err := spdx.Canonicalize(raw)
	if err != nil {
		switch {
		case errors.Is(err, spdx.ErrUnknownID):
			return "", fmt.Errorf("invalid --license %q: unknown SPDX id (%w) — check https://spdx.org/licenses or use a LicenseRef- token for a custom license", raw, err)
		case errors.Is(err, spdx.ErrSyntax):
			return "", fmt.Errorf("invalid --license %q: malformed SPDX expression (%w)", raw, err)
		default:
			return "", fmt.Errorf("invalid --license %q: %w", raw, err)
		}
	}
	return canonical, nil
}

// mergeFileSources combines files from -f / --file with files passed as
// positional arguments. The two forms are mutually exclusive.
func mergeFileSources(flagFiles, positional []string) ([]string, error) {
	if len(flagFiles) > 0 && len(positional) > 0 {
		return nil, errors.New("pass input files either positionally or via --file, not both")
	}
	if len(flagFiles) > 0 {
		return flagFiles, nil
	}
	return expandCommas(positional), nil
}

// expandCommas lets users write `-f a.yaml,b.yaml` in addition to
// `-f a.yaml -f b.yaml`.
func expandCommas(in []string) []string {
	out := make([]string, 0, len(in))
	for _, raw := range in {
		for part := range strings.SplitSeq(raw, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

// defaultRepository slugifies <author.id>/<metadata.id> for the
// registry path. Anything outside [a-zA-Z0-9._-] is collapsed to "-".
func defaultRepository(authorID, artifactID string) string {
	if authorID == "" || artifactID == "" {
		return ""
	}
	return slugify(authorID) + "/" + slugify(artifactID)
}

var slugPattern = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func slugify(s string) string {
	s = slugPattern.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-_.")
	return strings.ToLower(s)
}
