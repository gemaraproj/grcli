// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/revanite-io/grc-store-protocol/identity"

	ckhub "github.com/gemaraproj/grc-store-clientkit/hub"
	"github.com/gemaraproj/grcli/internal/hub"
	"github.com/gemaraproj/grcli/internal/registry"
	"github.com/gemaraproj/grcli/internal/sign"
	"github.com/gemaraproj/grcli/internal/sigverify"
)

// Flag names specific to verify. flagURL / flagRepository /
// flagCosignKey are declared in publish.go; flagVersion in unpack.go.
const (
	flagCertIdentity   = "certificate-identity"
	flagCertOIDCIssuer = "certificate-oidc-issuer"
	// flagTrustedRoot overrides the embedded Sigstore public-good
	// trusted_root.json with one read from disk — for
	// air-gapped deployments or a private Sigstore instance. Env form
	// GRCLI_TRUSTED_ROOT; there is no --flag, only the env / config key, since
	// it is an ops-level override, not a per-invocation knob.
	flagTrustedRoot = "trusted-root"
)

// defaultCertOIDCIssuer is the issuer assumed for keyless verification when
// --certificate-oidc-issuer (or the GRCLI_CERTIFICATE_OIDC_ISSUER env /
// user-global config key of the same name) is not set. Publishing to grc.store
// is a GitHub-Actions OIDC flow, so this is the issuer for ~every publisher;
// GitHub Enterprise / other CI / an OIDC proxy override it. It is
// applied contextually inside keyless mode, NOT as a viper default, so it can't
// disturb key-vs-keyless detection.
const defaultCertOIDCIssuer = "https://token.actions.githubusercontent.com"

func newVerifyCmd(v *viper.Viper) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Verify a remote Gemara bundle's signature",
		Long: `Verifies the Sigstore signature attached to a remote Gemara bundle.
Keyless verification runs IN-PROCESS — no external tools are
required, just the grcli binary. The bundle must already be pushed to a
registry: signatures live at the registry layer as an OCI 1.1 referrer, not in
the bundle bytes, so verifying a local OCI layout from 'publish --dry-run' is
not supported.

Signatures use the Sigstore bundle format (v0.3), attached as an OCI 1.1
referrer. Artifacts signed by an OLDER grcli — the legacy 'sha256-….sig' tag
format — will NOT verify here; re-publish them to re-sign in the bundle format.

The pinned Sigstore public-good trust root is embedded in grcli and refreshed
with each release. For an air-gapped deployment or a private Sigstore instance,
point GRCLI_TRUSTED_ROOT (env or config key 'trusted-root') at a
trusted_root.json on disk.

With NO trust flags, verify runs in zero-flag mode: it fetches
the catalog record from the hub, reads the keyless signer identity the hub
verified and pinned at ingest, and verifies against it — so a consumer needs
no prior knowledge of the publishing workflow. The identity it trusted, and
that it came from the hub record, are printed before verification runs.
This trusts the hub as the identity source; for an independent check, pass
--certificate-identity (or --cosign-key) yourself.

Passing --certificate-identity (keyless verification, paired with publish's
GitHub-Actions OIDC flow) bypasses the hub lookup entirely. The identity is
typically the publishing workflow URL, e.g.
https://github.com/<org>/<repo>/.github/workflows/publish.yml@refs/heads/main.
--certificate-oidc-issuer defaults to https://token.actions.githubusercontent.com
(the GitHub Actions issuer); set it — as a flag, GRCLI_CERTIFICATE_OIDC_ISSUER,
or a user-global config key — only for GitHub Enterprise, another CI provider,
or an OIDC proxy.

Passing --cosign-key (key-based verification, paired with publish's
--cosign-key) selects the one remaining path that shells out to 'cosign' — a
niche publisher-shared-key mode. That path, and ONLY that path, still requires
cosign >= 3.x on PATH.

Examples:
  # Zero-flag: verify against the identity the hub recorded at ingest
  grcli verify --url https://hub.grc.store \
    --repository myorg/my-controls --version 1.0.0

  # Key-based (bypasses the hub lookup)
  grcli verify --url https://hub.grc.store \
    --repository myorg/my-controls --version 1.0.0 \
    --cosign-key /keys/cosign.pub

  # Keyless, asserting the identity yourself (bypasses the hub lookup;
  # issuer defaults to GitHub Actions)
  grcli verify --url https://hub.grc.store \
    --repository myorg/my-controls --version 1.0.0 \
    --certificate-identity https://github.com/myorg/my-controls/.github/workflows/publish.yml@refs/heads/main

  # Keyless with a non-GitHub-Actions issuer
  grcli verify --url https://hub.grc.store \
    --repository myorg/my-controls --version 1.0.0 \
    --certificate-identity   <workflow-identity> \
    --certificate-oidc-issuer https://gitlab.example.com`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runVerify(cmd, v)
		},
	}

	flags := cmd.Flags()
	flags.String(flagURL, defaultURL, "grc.store base URL (discovers the registry)")
	flags.String(flagRepository, "", "repository path within the registry (required)")
	flags.String(flagVersion, "", "artifact version to verify — the metadata.version of the published bundle (required)")
	flags.String(flagCosignKey, "", "cosign public key file (mutually exclusive with keyless flags)")
	flags.String(flagCertIdentity, "", "expected signer identity (e.g., a GHA workflow URL)")
	flags.String(flagCertOIDCIssuer, "", "expected OIDC issuer for keyless verification (default: https://token.actions.githubusercontent.com — override for GitHub Enterprise / other CI)")

	return cmd
}

func runVerify(cmd *cobra.Command, v *viper.Viper) error {
	if err := v.BindPFlags(cmd.Flags()); err != nil {
		return fmt.Errorf("binding flags: %w", err)
	}
	ctx := cmd.Context()

	policy, err := resolveVerifyPolicy(ctx, v)
	if err != nil {
		return err
	}

	// The signature lives in the bearer-auth registry. Mint an
	// anonymous pull token from the hub (when --url is set and no override is
	// present) and export it via GRCLI_REGISTRY_TOKEN. The in-process oras fetch
	// reads it through the Docker credential chain (internal/registry), and
	// key-mode cosign — the one remaining subprocess — gets it as an explicit
	// flag, since the subprocess can't read the environment token.
	policy.registryToken, err = ensureRegistryToken(ctx, v.GetString(flagURL), "", v.GetString(flagRepository), []string{"pull"})
	if err != nil {
		return fmt.Errorf("fetching registry pull token: %w", err)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "verifying %s (%s)\n", policy.reference, policy.modeDescription())

	// Key-based verification is the ONLY path that still shells out to
	// cosign: a niche publisher-shared-key mode the hub doesn't
	// pin yet. The cosign prerequisite now applies exclusively here.
	if policy.keyPath != "" {
		if _, err := exec.LookPath("cosign"); err != nil {
			return errors.New("cosign binary not found on PATH — required only for --cosign-key (key-based) verification; " +
				"install from https://docs.sigstore.dev/cosign/installation/ (keyless verification needs no external tools)")
		}
		args, err := policy.cosignArgs(ctx)
		if err != nil {
			return err
		}
		return runCosignVerify(ctx, args, out)
	}

	// Keyless verification (explicit --certificate-identity and zero-flag
	// hub-lookup) runs in-process against real Sigstore — no cosign.
	return runKeylessVerify(ctx, v, policy, out)
}

// runKeylessVerify performs in-process keyless verification: it
// builds a sigstore-go verifier over the pinned (or GRCLI_TRUSTED_ROOT-override)
// trust root, discovers the signature bundle as an OCI referrer of the artifact
// manifest, and verifies it against both the artifact digest and the pinned
// signer identity (exact SAN for explicit mode, anchored SAN regexp for
// hub-lookup mode). The pre-verify announcement has already printed WHO/why is
// trusted; on success it prints the verified identity as confirmation.
func runKeylessVerify(ctx context.Context, v *viper.Viper, policy verifyPolicy, out io.Writer) error {
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
		return fmt.Errorf("%s has no signature attached in the registry — nothing to verify (was it published with --no-sign?)", policy.reference)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "verified: %s\n", res.Identity)
	return nil
}

// newSigstoreVerifier builds the in-process verifier, honoring the
// GRCLI_TRUSTED_ROOT override. A zero timeout selects the
// package default. Both constructors require SCTs — the production posture is
// never relaxed off the embedded/override root.
func newSigstoreVerifier(v *viper.Viper) (*sigverify.Verifier, error) {
	if path := v.GetString(flagTrustedRoot); path != "" {
		return sigverify.NewVerifierFromFile(path, 0)
	}
	return sigverify.NewVerifier(0)
}

// verifyPolicy bundles the resolved registry coordinates with the trust
// material used to verify the signature.
type verifyPolicy struct {
	reference string // <registry>/<repository>:<tag> — bare-host, for display + cosign
	// registryHost / repository / version are the split coordinates the
	// in-process keyless fetch needs (internal/registry.FetchSignatureBundle
	// resolves the tag, discovers the signature referrer). registryHost keeps
	// any http(s):// scheme the hub advertised so plain-HTTP local registries
	// propagate to oras (newRemoteRepo strips the scheme + sets PlainHTTP).
	registryHost string
	repository   string
	version      string
	keyPath      string // populated for key-based verification
	// identity is the exact keyless signer identity for --certificate-identity
	// (explicit-flag keyless mode). Empty in key mode and in hub-lookup mode.
	identity string
	// identityRegexp is the anchored regexp for --certificate-identity-regexp,
	// populated only in hub-lookup mode (the ref-stripped pin admits any git
	// ref but nothing wider than the exact workflow path). Empty otherwise.
	identityRegexp string
	issuer         string // populated for keyless verification (both modes)
	// hubIdentity is the canonical identity string the hub recorded, kept for
	// the pre-verify announcement so trust in the hub is visible, never silent.
	// Non-empty only in hub-lookup mode.
	hubIdentity   string
	registryToken string // Distribution pull token for the bearer-auth registry
	plainHTTP     bool   // registry speaks plain HTTP (local dev) — pass cosign --allow-http-registry
}

func (p verifyPolicy) modeDescription() string {
	switch {
	case p.keyPath != "":
		return "key=" + p.keyPath
	case p.identityRegexp != "":
		// Hub-lookup mode: name the identity AND that the hub is its source, so
		// the consumer sees exactly what they're trusting and where it came from.
		return "keyless identity from hub record: " + p.hubIdentity + ", issuer " + p.issuer
	default:
		return "keyless identity=" + p.identity + " issuer=" + p.issuer
	}
}

// cosignArgs builds the argv for the ONLY remaining cosign shell-out:
// --cosign-key (key-based) verification. The keyless
// paths verify in-process and never reach here. grcli signs with the Sigstore
// bundle format (bundle-as-OCI-referrer), so cosign must expect it too — the
// bundle-format flags come from the SAME version-gated helper the sign side
// uses (sign.BundleFormatArgs), so sign and verify can't silently drift on
// either the format OR the cosign version band.
func (p verifyPolicy) cosignArgs(ctx context.Context) ([]string, error) {
	bundleArgs, err := sign.BundleFormatArgs(ctx)
	if err != nil {
		return nil, err
	}
	args := append([]string{"verify"}, bundleArgs...)
	// cosign verify pulls the signature from the registry, which now
	// requires a bearer token. Unlike the in-process oras path, the
	// cosign subprocess can't read GRCLI_REGISTRY_TOKEN, so pass it
	// explicitly when we minted one.
	if p.registryToken != "" {
		args = append(args, "--registry-token", p.registryToken)
	}
	if p.plainHTTP {
		args = append(args, "--allow-http-registry")
	}
	args = append(args, "--key", p.keyPath)
	return append(args, p.reference), nil
}

// identityPolicy translates the resolved keyless trust material into the
// in-process sigstore-go identity pin. Explicit mode carries an exact SAN
// (p.identity); hub-lookup mode carries the anchored SAN regexp
// (p.identityRegexp) — exactly one is set. The issuer is always exact. These are
// the same fields cosignArgs used to hand cosign, so the identity semantics are
// byte-identical to the old --certificate-identity / --certificate-identity-regexp
// + --certificate-oidc-issuer arguments.
func (p verifyPolicy) identityPolicy() sigverify.IdentityPolicy {
	return sigverify.IdentityPolicy{
		SAN:       p.identity,
		SANRegexp: p.identityRegexp,
		Issuer:    p.issuer,
	}
}

func resolveVerifyPolicy(ctx context.Context, v *viper.Viper) (verifyPolicy, error) {
	url := v.GetString(flagURL)
	repository := v.GetString(flagRepository)
	version := v.GetString(flagVersion)
	keyPath := v.GetString(flagCosignKey)
	identity := v.GetString(flagCertIdentity)
	issuer := v.GetString(flagCertOIDCIssuer)

	// Validate the cheap flag combinations before the network round-trip,
	// so a missing --repository/--version or bad trust material fails fast
	// without a hub call.
	switch {
	case url == "":
		return verifyPolicy{}, errors.New("--url is required")
	case repository == "":
		return verifyPolicy{}, errors.New("--repository is required")
	case version == "":
		return verifyPolicy{}, errors.New("--version is required")
	}

	// Keyless mode is keyed on --certificate-identity ALONE, never the issuer:
	// the issuer carries a default (defaultCertOIDCIssuer), so letting it
	// trigger keyless mode would make every invocation look keyless and break
	// --cosign-key detection.
	keyMode := keyPath != ""
	keylessMode := identity != ""
	issuerSet := issuer != ""
	switch {
	case keyMode && (keylessMode || issuerSet):
		return verifyPolicy{}, errors.New("--cosign-key is mutually exclusive with --certificate-identity / --certificate-oidc-issuer")
	case issuerSet && !keylessMode:
		return verifyPolicy{}, errors.New("--certificate-oidc-issuer requires --certificate-identity")
	}
	// With no key and no identity we're in zero-flag mode:
	// the signer identity comes from the hub's catalog record, not the flags.
	// (A lone --certificate-oidc-issuer is already rejected above, so this is
	// exactly "no trust material at all".)
	hubLookupMode := !keyMode && !keylessMode
	// Keyless with no explicit issuer defaults to GitHub Actions.
	// This runs AFTER mode resolution, and cosign still checks issuer == this
	// value, so a wrong default can only cause a false rejection, never a
	// false acceptance. Hub-lookup mode carries its own issuer from the record.
	if keylessMode && issuer == "" {
		issuer = defaultCertOIDCIssuer
	}

	d, err := ckhub.Discover(ctx, url)
	if err != nil {
		return verifyPolicy{}, fmt.Errorf("hub discovery: %w", err)
	}
	registryHost := d.RegistryURL
	// The discovered registry value may carry an http(s):// scheme. Record
	// whether it's plain HTTP (so cosign gets --allow-http-registry for a
	// local dev zot), then normalize to a bare host — cosign rejects a
	// reference that includes a scheme.
	plainHTTP := strings.HasPrefix(registryHost, "http://")
	rawRegistry := registryHost // keeps the scheme for the in-process oras fetch
	registryHost = registry.NormalizeRegistryHost(registryHost)
	if registryHost == "" {
		return verifyPolicy{}, errors.New("hub discovery returned no registry URL")
	}

	policy := verifyPolicy{
		reference:    fmt.Sprintf("%s/%s:%s", registryHost, repository, version),
		registryHost: rawRegistry,
		repository:   repository,
		version:      version,
		keyPath:      keyPath,
		identity:     identity,
		issuer:       issuer,
		plainHTTP:    plainHTTP,
	}

	if hubLookupMode {
		if err := resolveHubIdentity(ctx, url, repository, &policy); err != nil {
			return verifyPolicy{}, err
		}
	}
	return policy, nil
}

// resolveHubIdentity fills the keyless trust material on policy from the hub's
// recorded signer identity for the catalog coordinate.
// The hub is trusted only as the *identity* source here — cosign still performs
// the Sigstore verification against it — and runVerify prints what was used and
// that it came from the hub before verifying, so the trust is never silent.
func resolveHubIdentity(ctx context.Context, url, repository string, policy *verifyPolicy) error {
	ns, id, ok := strings.Cut(repository, "/")
	if !ok || ns == "" || id == "" || strings.Contains(id, "/") {
		return fmt.Errorf("expected --repository as <namespace>/<catalog-id>, got %q", repository)
	}

	catalog, err := hub.New(url, "").GetCatalog(ctx, ns, id)
	if err != nil {
		return err
	}
	if catalog.SignerIdentity == "" {
		return fmt.Errorf("hub has no recorded signer identity for %s/%s — the artifact predates hub-side signature verification, or this hub does not serve signer identity; pass --cosign-key or --certificate-identity to verify explicitly", ns, id)
	}

	issuer, workflowPath, err := parseKeylessIdentity(catalog.SignerIdentity)
	if err != nil {
		return err
	}
	policy.issuer = issuer
	policy.hubIdentity = catalog.SignerIdentity
	// The pin is ref-stripped, so admit any git ref by matching the exact
	// workflow path followed by cosign's SAN '@<ref>' suffix. QuoteMeta and the
	// '^...@' anchor are load-bearing: they must never widen beyond this one
	// workflow path (e.g. a longer sibling path or an org-wide match).
	policy.identityRegexp = "^" + regexp.QuoteMeta(workflowPath) + "@"
	return nil
}

// parseKeylessIdentity splits a hub-recorded canonical signer identity into
// its issuer and workflow path by delegating to identity.ParseKeyless — the
// format owner's inverse of CanonicalKeylessIdentity, so producer and parser
// cannot drift — and maps its typed sentinels to actionable grcli messages. It
// rejects unknown schemes (e.g. the defined-but-unwired "key:sha256:<fpr>") and
// malformed values so a garbled record fails loudly rather than producing a
// bogus verification policy.
func parseKeylessIdentity(canonical string) (issuer, workflowPath string, err error) {
	issuer, workflowPath, err = identity.ParseKeyless(canonical)
	switch {
	case errors.Is(err, identity.ErrUnknownScheme):
		if scheme, _, hasScheme := strings.Cut(canonical, ":"); hasScheme && scheme != "" {
			return "", "", fmt.Errorf("hub signer identity %q uses unsupported scheme %q — only keyless identities can be verified without explicit trust flags; pass --cosign-key or --certificate-identity", canonical, scheme)
		}
		return "", "", fmt.Errorf("hub signer identity %q is malformed (expected \"keyless:<issuer>#<workflow-path>\")", canonical)
	case errors.Is(err, identity.ErrMissingSeparator):
		return "", "", fmt.Errorf("hub signer identity %q is malformed (expected \"keyless:<issuer>#<workflow-path>\")", canonical)
	case err != nil:
		return "", "", fmt.Errorf("hub signer identity %q: %w", canonical, err)
	}
	// ParseKeyless owns the format split; grcli additionally rejects empty
	// halves — a pin with no issuer or no path cannot drive a cosign policy.
	if issuer == "" || workflowPath == "" {
		return "", "", fmt.Errorf("hub signer identity %q is malformed (expected \"keyless:<issuer>#<workflow-path>\")", canonical)
	}
	return issuer, workflowPath, nil
}

func runCosignVerify(ctx context.Context, args []string, out io.Writer) error {
	cosignCmd := exec.CommandContext(ctx, "cosign", args...)
	cosignCmd.Stdout = out
	cosignCmd.Stderr = out
	cosignCmd.Stdin = os.Stdin
	if err := cosignCmd.Run(); err != nil {
		return fmt.Errorf("cosign verify failed: %w", err)
	}
	return nil
}
