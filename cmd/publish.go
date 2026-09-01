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

	"github.com/revanite-io/grc-store-protocol/spdx"

	"github.com/gemaraproj/grc-store-clientkit/auth"
	"github.com/revanite-io/grcli/internal/digest"
	"github.com/revanite-io/grcli/internal/hub"
	"github.com/revanite-io/grcli/internal/provenance"
	"github.com/revanite-io/grcli/internal/registry"
	"github.com/revanite-io/grcli/internal/sign"
	"github.com/revanite-io/grcli/internal/source"
)

// Flag names are declared once so the compiler catches typos at every
// viper.Get call site. publish does not expose a tag/version flag —
// the OCI tag is always metadata.version (ADR-0033). unpack and verify
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
registry, optionally signs with cosign, and notifies the hub via
POST /v1/bundles/sync.

Files can be supplied as positional arguments (grcli publish a.yaml
b.yaml) or via -f / --file. The two forms are mutually exclusive —
mixing them is an error so neither silently wins.

Use --dry-run to write the bundle to an OCI image layout on disk
instead of touching any network.

Auth in GitHub Actions: no GitHub secret, no --token, no GRCLI_TOKEN —
when run inside a workflow with permissions: id-token: write, grcli
mints a GitHub Actions OIDC token and presents it as the credential
(ADR-0032 trusted publishing). The repo (owner/repo, optionally pinned
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
	flags.String(flagURL, defaultURL, "grc.store base URL — discovers the registry and is the hub sync target (ADR-0026)")
	flags.String(flagRepository, "", "repository path within the registry (default: <author.id>/<metadata.id>, slugified to [a-z0-9._-])")
	flags.String(flagToken, "", "bearer token for the hub sync call (or GRCLI_TOKEN); leave unset in GitHub Actions — the workflow's OIDC token is used automatically (trusted publishing, no GitHub secret needed)")
	flags.Bool(flagDryRun, false, "skip all network — emit OCI layout to --output instead")
	flags.String(flagOutput, "grcli-out", "directory to write the OCI layout to when --dry-run")
	flags.Bool(flagNoSign, false, "skip cosign signing even when material is available")
	flags.String(flagCosignKey, "", "cosign key file for local signing (or COSIGN_KEY)")
	flags.String(flagLicense, "", "REQUIRED: publication license as an SPDX expression (e.g. Apache-2.0, MIT OR Apache-2.0, LicenseRef-Revanite-Proprietary); stamped as the org.opencontainers.image.licenses OCI annotation. Publish fails before any network call if unset (ADR-0037)")

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
	registryHost string
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

	// Strict license gate (ADR-0037 decision 1, tightening ADR-0036): grcli
	// is the strict end. --license is now REQUIRED. Validate and canonicalize
	// BEFORE any pack/push — including the --dry-run path — so a missing,
	// malformed, or unknown SPDX expression never produces OCI bytes (locally
	// or in the registry).
	canonicalLicense, err := validatePublishLicense(v.GetString(flagLicense))
	if err != nil {
		return err
	}

	if !target.dryRun {
		// Pre-flight: fail BEFORE packing/pushing if we intend to sign but
		// can't — a signing misconfig must not leave unsigned bytes orphaned
		// in the registry. This is a local, instant check (cosign on PATH +
		// key/CI material); --no-sign is the explicit opt-out for an
		// unsigned, unverifiable publish.
		if err := sign.Preflight(ctx, sign.Options{
			Disabled: v.GetBool(flagNoSign),
			KeyPath:  v.GetString(flagCosignKey),
		}); err != nil {
			return err
		}
		// Pre-flight: versions are immutable, so halt BEFORE packing or
		// pushing if the coordinate is already taken (ADR-0031). This is
		// what stops a re-publish from clobbering existing bytes in the
		// registry — the registry would accept the overwrite before the
		// hub's sync-time guard could reject it.
		if err := checkVersionAvailable(ctx, v, target.repository, target.tag); err != nil {
			return err
		}
		// The registry rejects unauthenticated writes. Mint a repo-scoped
		// push token from the hub and export it so both the oras push and
		// the cosign signature push authenticate.
		if err := authenticatePush(ctx, v, target.repository); err != nil {
			return err
		}
	}

	predicate := provenance.Build(provenance.Input{
		ToolVersion:    version,
		StartedOn:      startedOn,
		ArtifactType:   loaded.Type,
		ArtifactID:     loaded.ID,
		ArtifactName:   loaded.Filename,
		ArtifactDigest: digest.Bytes(loaded.Body),
		SourceFiles:    loaded.SourceDigests,
		Registry:       registry.NormalizeRegistryHost(target.registryHost),
		Repository:     target.repository,
		Tag:            target.tag,
	})

	packInput := registry.PackInput{
		Filename:      loaded.Filename,
		ArtifactType:  loaded.Type,
		ArtifactID:    loaded.ID,
		GemaraVersion: loaded.GemaraVersion,
		Body:          loaded.Body,
		Provenance:    predicate,
		License:       canonicalLicense,
	}

	result, err := pushBundle(ctx, target, packInput, cmd.OutOrStdout(), loaded.Type, loaded.ID)
	if err != nil {
		return err
	}
	if target.dryRun {
		return nil
	}

	return signAndNotify(ctx, v, signContext{
		repository:     target.repository,
		tag:            target.tag,
		reference:      result.Reference,
		registryHost:   target.registryHost,
		manifestDigest: result.ManifestDigest,
		plainHTTP:      strings.HasPrefix(target.registryHost, "http://"),
	})
}

// signContext carries the push coordinates the sign + notify step needs.
type signContext struct {
	repository     string
	tag            string
	reference      string // <registry>/<repository>:<tag>, bare host
	registryHost   string // scheme-prefixed oras dial target
	manifestDigest string // sha256:… of the just-pushed manifest
	plainHTTP      bool
}

// resolveTarget merges --repository/--url/--dry-run with the
// metadata-derived defaults and validates the combination. --url drives
// the registry hostname via the hub's discovery endpoint (ADR-0026);
// --dry-run skips discovery since it never touches the network.
//
// The OCI tag is always metadata.version — no override. ADR-0033 (in
// grc.store-backend) made tag == metadata.version a hub-enforced
// invariant; a --tag override could only ever produce a 422
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
		// Keep the scheme the hub advertises (http:// for a plain-HTTP
		// dev registry, https:// for prod). registryHost is the oras dial
		// target, and newRemoteRepo derives PlainHTTP from that scheme —
		// stripping it here would force HTTPS against a plain-HTTP zot.
		// Display/provenance/cosign consumers normalize to a bare host at
		// their own call sites (PushResult.Reference, provenance below).
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

// pushBundle either writes the bundle to a local OCI layout (dry-run)
// or pushes it to the configured registry, printing a one-line summary
// in either case.
func pushBundle(ctx context.Context, target publishTarget, in registry.PackInput, out io.Writer, artifactType, artifactID string) (*registry.PushResult, error) {
	if target.dryRun {
		result, err := registry.PushLocal(ctx, target.output, target.tag, in)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(out,
			"dry-run: wrote bundle to %s\n  manifest digest: %s\n  body digest:     %s\n  artifact: %s/%s\n",
			result.Reference, result.ManifestDigest, result.BodyDigest, artifactType, artifactID)
		return result, nil
	}
	result, err := registry.PushRemote(ctx, target.registryHost, target.repository, target.tag, in)
	if err != nil {
		return nil, fmt.Errorf("push: %w", err)
	}
	fmt.Fprintf(out, "pushed %s\n  manifest digest: %s\n", result.Reference, result.ManifestDigest)
	return result, nil
}

// signAndNotify runs the optional cosign step and the hub sync call.
// Status lines go to os.Stdout rather than a passed-in writer because
// the cosign subprocess inside sign.Sign writes to os.Stdout/os.Stderr
// directly; routing grcli's own status lines through a different writer
// would create a misleading "I control the output" contract.
func signAndNotify(ctx context.Context, v *viper.Viper, sc signContext) error {
	signResult, err := sign.Sign(ctx, sign.Options{
		Disabled:       v.GetBool(flagNoSign),
		KeyPath:        v.GetString(flagCosignKey),
		Reference:      sc.reference,
		PlainHTTP:      sc.plainHTTP,
		RegistryHost:   sc.registryHost,
		Repository:     sc.repository,
		ManifestDigest: sc.manifestDigest,
	})
	if err != nil {
		return fmt.Errorf("sign: %w", err)
	}
	if signResult.Mode == sign.ModeSkipped {
		fmt.Fprintf(os.Stdout, "signing skipped: %s\n", signResult.Reason)
	} else {
		fmt.Fprintf(os.Stdout, "signed (%s)\n", signResult.Mode)
	}

	hubURL := publishHubURL(v)
	if hubURL == "" {
		fmt.Fprintln(os.Stdout, "skipping hub sync: --url not set")
		return nil
	}
	token, err := resolveBearerToken(ctx, v)
	if err != nil {
		return err
	}
	syncResp, err := hub.New(hubURL, token).Sync(ctx, sc.repository, sc.tag)
	if err != nil {
		return fmt.Errorf("hub sync: %w", err)
	}
	fmt.Fprintf(os.Stdout,
		"hub indexed %s:%s — %d artifacts (%d new), types=%s\n",
		syncResp.Repository, syncResp.Tag,
		syncResp.ArtifactCount, syncResp.NewCount,
		strings.Join(syncResp.Types, ","),
	)
	return nil
}

// checkVersionAvailable is the publish pre-flight. Versions are immutable
// (ADR-0031), so if the target coordinate already exists on the hub, halt
// here — before packing, before any registry write. That prevents a
// re-publish from clobbering the existing bytes in the registry (which
// accepts the overwrite before the hub's sync-time guard can reject it).
// No-op when there's no hub URL to ask (--url explicitly cleared) or when
// --repository isn't a plain <namespace>/<id> coordinate; in those cases
// the server-side sync guard remains the backstop.
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

// ciAudience returns the audience grcli requests on its GitHub Actions
// OIDC token. The hub advertises its expected CI audience via discovery
// (ci_audience); prefer that so the token grcli mints and the value the
// hub validates can't drift (a trailing slash or a stale env var would
// otherwise produce an opaque 401). Falls back to the hub URL when
// discovery omits it (an older hub, or one with CI publishing off).
func ciAudience(ctx context.Context, v *viper.Viper) string {
	if url := v.GetString(flagURL); url != "" {
		if d, err := hub.Discover(ctx, url); err == nil && d.CIAudience != "" {
			return d.CIAudience
		}
	}
	return publishHubURL(v)
}

// publishHubURL returns the hub base URL for the publish run (--url).
// Empty means --url was explicitly cleared, so there's no hub to sync
// with or mint a registry token from.
func publishHubURL(v *viper.Viper) string {
	return v.GetString(flagURL)
}

// authenticatePush exports a registry push token (GRCLI_REGISTRY_TOKEN)
// so the oras push and the cosign signature push authenticate to the
// bearer-auth registry (ADR-0031). The hub grants push only to a
// namespace owner or admin, so a push needs a hub login: when no explicit
// registry credential override is present, we resolve the login token and
// surface a clear `grcli login` hint if it's missing. No-op when there's
// no hub URL (--url explicitly cleared) or when a manual GRCLI_REGISTRY_*
// override is set.
func authenticatePush(ctx context.Context, v *viper.Viper, repository string) error {
	hubBaseURL := publishHubURL(v)
	if hubBaseURL == "" {
		return nil
	}
	if os.Getenv("GRCLI_REGISTRY_TOKEN") != "" ||
		(os.Getenv("GRCLI_REGISTRY_USERNAME") != "" && os.Getenv("GRCLI_REGISTRY_PASSWORD") != "") {
		return nil
	}
	login, err := resolveBearerToken(ctx, v)
	if err != nil {
		return fmt.Errorf("registry push needs a hub login to mint a push token: %w", err)
	}
	if _, err := ensureRegistryToken(ctx, hubBaseURL, login, repository, []string{"pull", "push"}); err != nil {
		return fmt.Errorf("fetching registry push token: %w", err)
	}
	return nil
}

// resolveBearerToken wraps auth.Resolve with the publish command's
// glue: pulls --token / GRCLI_TOKEN (merged by viper), re-runs hub
// discovery to learn the OIDC issuer + client_id when --url is set
// (cached after resolveTarget's earlier call, so this is a map lookup),
// and instantiates the default credential store. Discovery failures
// here are swallowed — the worst case is that auth.Resolve has no
// store-key to look up and falls back to ErrNoToken, which prints the
// same "run grcli login" hint a caller would already need.
func resolveBearerToken(ctx context.Context, v *viper.Viper) (string, error) {
	in := auth.ResolveInput{
		App:           grcliApp,
		ExplicitToken: v.GetString(flagToken),
		Warn:          os.Stderr,
	}
	// Resolution order (ADR-0028): --token / GRCLI_TOKEN (captured above)
	// > GitHub Actions OIDC > stored device-login creds. The CI step:
	// when no explicit token is set and we're in a GHA job, fetch the
	// workflow's OIDC token and present it directly — the hub validates it
	// (ADR-0032) and maps the repo to its trusted-publisher namespace. No
	// secret, no login. The audience comes from the hub's discovery doc
	// (ci_audience), falling back to the hub URL, so it always matches the
	// hub's HUB_CI_OIDC_AUDIENCE. On any failure we fall through to the
	// normal stored-credential path rather than hard-failing.
	if in.ExplicitToken == "" && auth.InGitHubActions() {
		if tok, err := auth.FetchGitHubActionsToken(ctx, ciAudience(ctx, v)); err == nil && tok != "" {
			return tok, nil
		} else if err != nil {
			fmt.Fprintf(os.Stderr, "warning: GitHub Actions OIDC token unavailable, falling back: %v\n", err)
		}
	}
	if url := v.GetString(flagURL); url != "" {
		if d, err := hub.Discover(ctx, url); err == nil {
			in.Issuer = d.OIDCIssuer
			in.ClientID = d.OIDCCLIClientID
		}
	}
	if store, err := auth.NewDefaultStore(grcliApp); err == nil {
		in.Store = store
	}
	return auth.Resolve(ctx, in)
}

// validatePublishLicense runs the strict SPDX gate for --license (ADR-0037
// decision 1, tightening ADR-0036: grcli is the strict end). The flag is now
// REQUIRED: an empty/whitespace-only value is an error — distinct from the
// invalid-value message, because a missing flag and a malformed value are
// different user mistakes. A supplied value must be a well-formed SPDX
// expression whose every leaf id is known to the bundled SPDX list; the
// returned string is the canonical SPDX spelling, used from here on. The two
// invalid-value failure modes are distinguished so the publisher knows whether
// they have a grammar error or a typo'd/unknown id.
func validatePublishLicense(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("a publication license is required: pass --license with an SPDX expression (e.g. Apache-2.0, MIT OR Apache-2.0; see https://spdx.org/licenses) or a LicenseRef-… token for a custom/proprietary license (ADR-0037)")
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
// positional arguments. The two forms are mutually exclusive: mixing
// them silently would let one form override or shadow the other on
// scripted runs where both might be set unintentionally (e.g. a
// .grcli.yaml config sets file: while the caller also types one in).
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
// `-f a.yaml -f b.yaml`. Cobra's StringSliceP splits commas at the
// flag layer, but viper.GetStringSlice does not when the underlying
// source is a config file, so we re-split defensively.
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
