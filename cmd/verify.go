// SPDX-License-Identifier: LicenseRef-Revanite-Proprietary

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

	"github.com/revanite-io/grcli/internal/hub"
	"github.com/revanite-io/grcli/internal/registry"
	"github.com/revanite-io/grcli/internal/sign"
)

// Flag names specific to verify. flagURL / flagRepository /
// flagCosignKey are declared in publish.go; flagVersion in unpack.go.
const (
	flagCertIdentity   = "certificate-identity"
	flagCertOIDCIssuer = "certificate-oidc-issuer"
)

// defaultCertOIDCIssuer is the issuer assumed for keyless verification when
// --certificate-oidc-issuer (or the GRCLI_CERTIFICATE_OIDC_ISSUER env /
// user-global config key of the same name) is not set. Publishing to grc.store
// is a GitHub-Actions OIDC flow, so this is the issuer for ~every publisher;
// GitHub Enterprise / other CI / an OIDC proxy override it (ADR-0044). It is
// applied contextually inside keyless mode, NOT as a viper default, so it can't
// disturb key-vs-keyless detection.
const defaultCertOIDCIssuer = "https://token.actions.githubusercontent.com"

func newVerifyCmd(v *viper.Viper) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Verify a remote Gemara bundle's cosign signature",
		Long: `Verifies the cosign signature attached to a remote Gemara bundle by
shelling out to 'cosign verify'. The bundle must already be pushed to a
registry — cosign signatures live at the registry layer, not in the
bundle bytes, so verifying a local OCI layout from 'publish --dry-run'
is not supported.

Signatures use the Sigstore bundle format (cosign's --new-bundle-format),
attached as an OCI 1.1 referrer, which this command always requests.
Artifacts signed by an OLDER grcli — the legacy 'sha256-….sig' tag format —
will NOT verify here; re-publish them to re-sign in the bundle format.
Requires cosign >= 3.x on PATH.

With NO trust flags, verify runs in zero-flag mode (ADR-0045): it fetches
the catalog record from the hub, reads the keyless signer identity the hub
verified and pinned at ingest, and verifies against it — so a consumer needs
no prior knowledge of the publishing workflow. The identity it trusted, and
that it came from the hub record, are printed before verification runs.
This trusts the hub as the identity source; for an independent check, pass
--certificate-identity (or --cosign-key) yourself.

Passing --cosign-key (key-based verification, paired with publish's
--cosign-key) or --certificate-identity (keyless verification, paired with
publish's GitHub-Actions OIDC flow) bypasses the hub lookup entirely. For
keyless, the identity is typically the publishing workflow URL, e.g.
https://github.com/<org>/<repo>/.github/workflows/publish.yml@refs/heads/main.
--certificate-oidc-issuer defaults to https://token.actions.githubusercontent.com
(the GitHub Actions issuer); set it — as a flag, GRCLI_CERTIFICATE_OIDC_ISSUER,
or a user-global config key — only for GitHub Enterprise, another CI provider,
or an OIDC proxy.

Requires 'cosign' on PATH.

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

	// ADR-0031: cosign verify pulls the signature from the bearer-auth
	// registry. Mint an anonymous pull token from the hub (when --url is
	// set and no override is present) and pass it to cosign explicitly —
	// the subprocess can't read GRCLI_REGISTRY_TOKEN from the environment.
	policy.registryToken, err = ensureRegistryToken(ctx, v.GetString(flagURL), "", v.GetString(flagRepository), []string{"pull"})
	if err != nil {
		return fmt.Errorf("fetching registry pull token: %w", err)
	}

	if _, err := exec.LookPath("cosign"); err != nil {
		return errors.New("cosign binary not found on PATH — install from https://docs.sigstore.dev/cosign/installation/")
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "verifying %s (%s)\n", policy.reference, policy.modeDescription())
	return runCosignVerify(ctx, policy.cosignArgs(), out)
}

// verifyPolicy bundles the resolved registry coordinates with the trust
// material used to verify the signature.
type verifyPolicy struct {
	reference string // <registry>/<repository>:<tag>
	keyPath   string // populated for key-based verification
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
	// Non-empty only in hub-lookup mode (ADR-0045 decision 8).
	hubIdentity   string
	registryToken string // Distribution pull token for the bearer-auth registry (ADR-0031)
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

func (p verifyPolicy) cosignArgs() []string {
	// grcli signs with the Sigstore bundle format (bundle-as-OCI-referrer), so
	// verification must expect it too. A bundle signature does not verify against
	// the legacy `.sig` path; the two are a matched producer/consumer pair. The
	// flag string is the SAME exported constant the sign side uses, so they can't
	// silently drift (sign.FlagNewBundleFormat, ADR-0035).
	args := []string{"verify", sign.FlagNewBundleFormat}
	// cosign verify pulls the signature from the registry, which now
	// requires a bearer token (ADR-0031). Unlike the oras path, the
	// cosign subprocess can't read GRCLI_REGISTRY_TOKEN, so pass it
	// explicitly when we minted one.
	if p.registryToken != "" {
		args = append(args, "--registry-token", p.registryToken)
	}
	if p.plainHTTP {
		args = append(args, "--allow-http-registry")
	}
	switch {
	case p.keyPath != "":
		args = append(args, "--key", p.keyPath)
	case p.identityRegexp != "":
		// Hub-lookup mode: the pin is ref-stripped, so match the workflow path
		// under any ref via a regexp anchored to that exact path (ADR-0045).
		args = append(args, "--certificate-identity-regexp", p.identityRegexp, "--certificate-oidc-issuer", p.issuer)
	default:
		args = append(args, "--certificate-identity", p.identity, "--certificate-oidc-issuer", p.issuer)
	}
	return append(args, p.reference)
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
	// With no key and no identity we're in zero-flag mode (ADR-0045 decision 8):
	// the signer identity comes from the hub's catalog record, not the flags.
	// (A lone --certificate-oidc-issuer is already rejected above, so this is
	// exactly "no trust material at all".)
	hubLookupMode := !keyMode && !keylessMode
	// Keyless with no explicit issuer defaults to GitHub Actions (ADR-0044).
	// This runs AFTER mode resolution, and cosign still checks issuer == this
	// value, so a wrong default can only cause a false rejection, never a
	// false acceptance. Hub-lookup mode carries its own issuer from the record.
	if keylessMode && issuer == "" {
		issuer = defaultCertOIDCIssuer
	}

	d, err := hub.Discover(ctx, url)
	if err != nil {
		return verifyPolicy{}, fmt.Errorf("hub discovery: %w", err)
	}
	registryHost := d.RegistryURL
	// The discovered registry value may carry an http(s):// scheme. Record
	// whether it's plain HTTP (so cosign gets --allow-http-registry for a
	// local dev zot), then normalize to a bare host — cosign rejects a
	// reference that includes a scheme.
	plainHTTP := strings.HasPrefix(registryHost, "http://")
	registryHost = registry.NormalizeRegistryHost(registryHost)
	if registryHost == "" {
		return verifyPolicy{}, errors.New("hub discovery returned no registry URL")
	}

	policy := verifyPolicy{
		reference: fmt.Sprintf("%s/%s:%s", registryHost, repository, version),
		keyPath:   keyPath,
		identity:  identity,
		issuer:    issuer,
		plainHTTP: plainHTTP,
	}

	if hubLookupMode {
		if err := resolveHubIdentity(ctx, url, repository, &policy); err != nil {
			return verifyPolicy{}, err
		}
	}
	return policy, nil
}

// resolveHubIdentity fills the keyless trust material on policy from the hub's
// recorded signer identity for the catalog coordinate (ADR-0045 decision 8).
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

// parseKeylessIdentity splits a hub-recorded canonical signer identity —
// "keyless:<oidc-issuer>#<workflow-path>", ref-stripped
// (grc-store-protocol/identity) — into its issuer and workflow path. It rejects
// unknown schemes (e.g. the defined-but-unwired "key:sha256:<fpr>") and
// malformed values so a garbled record fails loudly rather than producing a
// bogus cosign policy.
func parseKeylessIdentity(canonical string) (issuer, workflowPath string, err error) {
	rest, ok := strings.CutPrefix(canonical, identity.KeylessScheme)
	if !ok {
		scheme, _, hasScheme := strings.Cut(canonical, ":")
		if hasScheme {
			return "", "", fmt.Errorf("hub signer identity %q uses unsupported scheme %q — only keyless identities can be verified without explicit trust flags; pass --cosign-key or --certificate-identity", canonical, scheme)
		}
		return "", "", fmt.Errorf("hub signer identity %q is malformed (expected \"keyless:<issuer>#<workflow-path>\")", canonical)
	}
	issuer, workflowPath, ok = strings.Cut(rest, "#")
	if !ok || issuer == "" || workflowPath == "" {
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
