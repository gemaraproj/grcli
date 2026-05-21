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
)

// Flag names specific to verify. flagRegistry / flagRepository / flagTag
// / flagCosignKey are declared in publish.go.
const (
	flagCertIdentity   = "certificate-identity"
	flagCertOIDCIssuer = "certificate-oidc-issuer"
)

func newVerifyCmd(v *viper.Viper) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Verify a remote Gemara bundle's cosign signature",
		Long: `Verifies the cosign signature attached to a remote Gemara bundle by
shelling out to 'cosign verify'. The bundle must already be pushed to a
registry — cosign signatures live at the registry layer, not in the
bundle bytes, so verifying a local OCI layout from 'publish --dry-run'
is not supported.

You must specify either --cosign-key (key-based verification, paired
with publish's --cosign-key) or both --certificate-identity and
--certificate-oidc-issuer (keyless verification, paired with publish's
GitHub-Actions OIDC flow). For keyless, the identity is typically the
publishing workflow URL, e.g.
https://github.com/<org>/<repo>/.github/workflows/publish.yml@refs/heads/main,
and the issuer is https://token.actions.githubusercontent.com.

The verification policy a publisher should register with grc.store is
exactly this pair: a public key, or an (identity, issuer) tuple. (The
grc.store registration UI for this is not yet shipped; for now, share
the policy out-of-band with anyone who needs to verify your bundles.)

Requires 'cosign' on PATH.

Examples:
  # Key-based
  grcli verify --registry registry.grc.store \
    --repository myorg/my-controls --tag 1.0.0 \
    --cosign-key /keys/cosign.pub

  # Keyless (GitHub Actions OIDC)
  grcli verify --registry registry.grc.store \
    --repository myorg/my-controls --tag 1.0.0 \
    --certificate-identity   https://github.com/myorg/my-controls/.github/workflows/publish.yml@refs/heads/main \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runVerify(cmd, v)
		},
	}

	flags := cmd.Flags()
	flags.String(flagURL, defaultURL, "grc.store base URL (discovers the registry; replaces --registry)")
	flags.String(flagRegistry, "", "OCI registry hostname (required if --url is not set)")
	flags.String(flagRepository, "", "repository path within the registry (required)")
	flags.String(flagTag, "", "OCI tag to verify (required)")
	flags.String(flagCosignKey, "", "cosign public key file (mutually exclusive with keyless flags)")
	flags.String(flagCertIdentity, "", "expected signer identity (e.g., a GHA workflow URL)")
	flags.String(flagCertOIDCIssuer, "", "expected OIDC issuer (e.g., https://token.actions.githubusercontent.com)")
	// Deprecated: kept functional for one release cycle (ADR-0026).
	_ = flags.MarkDeprecated(flagRegistry, "use --url to discover the registry from the hub")

	return cmd
}

func runVerify(cmd *cobra.Command, v *viper.Viper) error {
	if err := v.BindPFlags(cmd.Flags()); err != nil {
		return fmt.Errorf("binding flags: %w", err)
	}
	suppressDefaultURLIfExplicit(cmd, v, flagRegistry)
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
	args := []string{"verify"}
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
	registryHost := v.GetString(flagRegistry)
	url := v.GetString(flagURL)
	repository := v.GetString(flagRepository)
	tag := v.GetString(flagTag)
	keyPath := v.GetString(flagCosignKey)
	identity := v.GetString(flagCertIdentity)
	issuer := v.GetString(flagCertOIDCIssuer)

	if url != "" && registryHost != "" {
		return verifyPolicy{}, errors.New("conflicting flags: --url and --registry; pick one")
	}
	if registryHost == "" && url != "" {
		d, err := hub.Discover(ctx, url)
		if err != nil {
			return verifyPolicy{}, fmt.Errorf("hub discovery: %w", err)
		}
		registryHost = d.RegistryURL
	}
	// The registry value (discovered or --registry) may carry an http(s)://
	// scheme. Record whether it's plain HTTP (so cosign gets
	// --allow-http-registry for a local dev zot), then normalize to a bare
	// host — cosign rejects a reference that includes a scheme.
	plainHTTP := strings.HasPrefix(registryHost, "http://")
	registryHost = registry.NormalizeRegistryHost(registryHost)

	switch {
	case registryHost == "":
		return verifyPolicy{}, errors.New("--registry or --url is required")
	case repository == "":
		return verifyPolicy{}, errors.New("--repository is required")
	case tag == "":
		return verifyPolicy{}, errors.New("--tag is required")
	}

	keyMode := keyPath != ""
	keylessMode := identity != "" || issuer != ""
	switch {
	case !keyMode && !keylessMode:
		return verifyPolicy{}, errors.New("either --cosign-key or --certificate-identity is required")
	case keyMode && keylessMode:
		return verifyPolicy{}, errors.New("--cosign-key is mutually exclusive with --certificate-identity / --certificate-oidc-issuer")
	case keylessMode && (identity == "" || issuer == ""):
		return verifyPolicy{}, errors.New("keyless verification requires both --certificate-identity and --certificate-oidc-issuer")
	}

	return verifyPolicy{
		reference: fmt.Sprintf("%s/%s:%s", registryHost, repository, tag),
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
