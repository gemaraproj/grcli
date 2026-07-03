// SPDX-License-Identifier: LicenseRef-Revanite-Proprietary

package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

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

You must specify either --cosign-key (key-based verification, paired
with publish's --cosign-key) or --certificate-identity (keyless
verification, paired with publish's GitHub-Actions OIDC flow). For
keyless, the identity is typically the publishing workflow URL, e.g.
https://github.com/<org>/<repo>/.github/workflows/publish.yml@refs/heads/main.
--certificate-oidc-issuer defaults to https://token.actions.githubusercontent.com
(the GitHub Actions issuer); set it — as a flag, GRCLI_CERTIFICATE_OIDC_ISSUER,
or a user-global config key — only for GitHub Enterprise, another CI provider,
or an OIDC proxy.

The verification policy a publisher should register with grc.store is
exactly this pair: a public key, or an (identity, issuer) tuple. (The
grc.store registration UI for this is not yet shipped; for now, share
the policy out-of-band with anyone who needs to verify your bundles.)

Requires 'cosign' on PATH.

Examples:
  # Key-based
  grcli verify --url https://hub.grc.store \
    --repository myorg/my-controls --version 1.0.0 \
    --cosign-key /keys/cosign.pub

  # Keyless (GitHub Actions OIDC — issuer defaults to GitHub Actions)
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
	reference     string // <registry>/<repository>:<tag>
	keyPath       string // populated for key-based verification
	identity      string // populated for keyless verification
	issuer        string // populated for keyless verification
	registryToken string // Distribution pull token for the bearer-auth registry (ADR-0031)
	plainHTTP     bool   // registry speaks plain HTTP (local dev) — pass cosign --allow-http-registry
}

func (p verifyPolicy) modeDescription() string {
	if p.keyPath != "" {
		return "key=" + p.keyPath
	}
	return "keyless identity=" + p.identity + " issuer=" + p.issuer
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
	if p.keyPath != "" {
		args = append(args, "--key", p.keyPath)
	} else {
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
	case !keyMode && !keylessMode && !issuerSet:
		return verifyPolicy{}, errors.New("either --cosign-key or --certificate-identity is required")
	case keyMode && (keylessMode || issuerSet):
		return verifyPolicy{}, errors.New("--cosign-key is mutually exclusive with --certificate-identity / --certificate-oidc-issuer")
	case issuerSet && !keylessMode:
		return verifyPolicy{}, errors.New("--certificate-oidc-issuer requires --certificate-identity")
	}
	// Keyless with no explicit issuer defaults to GitHub Actions (ADR-0044).
	// This runs AFTER mode resolution, and cosign still checks issuer == this
	// value, so a wrong default can only cause a false rejection, never a
	// false acceptance.
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

	return verifyPolicy{
		reference: fmt.Sprintf("%s/%s:%s", registryHost, repository, version),
		keyPath:   keyPath,
		identity:  identity,
		issuer:    issuer,
		plainHTTP: plainHTTP,
	}, nil
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
