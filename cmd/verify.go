// SPDX-License-Identifier: LicenseRef-Revanite-Proprietary

package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
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
	flags.String(flagRegistry, "", "OCI registry hostname (required)")
	flags.String(flagRepository, "", "repository path within the registry (required)")
	flags.String(flagTag, "", "OCI tag to verify (required)")
	flags.String(flagCosignKey, "", "cosign public key file (mutually exclusive with keyless flags)")
	flags.String(flagCertIdentity, "", "expected signer identity (e.g., a GHA workflow URL)")
	flags.String(flagCertOIDCIssuer, "", "expected OIDC issuer (e.g., https://token.actions.githubusercontent.com)")

	return cmd
}

func runVerify(cmd *cobra.Command, v *viper.Viper) error {
	if err := v.BindPFlags(cmd.Flags()); err != nil {
		return fmt.Errorf("binding flags: %w", err)
	}
	ctx := cmd.Context()

	policy, err := resolveVerifyPolicy(v)
	if err != nil {
		return err
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
	identity  string // populated for keyless verification
	issuer    string // populated for keyless verification
}

func (p verifyPolicy) modeDescription() string {
	if p.keyPath != "" {
		return "key=" + p.keyPath
	}
	return "keyless identity=" + p.identity + " issuer=" + p.issuer
}

func (p verifyPolicy) cosignArgs() []string {
	args := []string{"verify"}
	if p.keyPath != "" {
		args = append(args, "--key", p.keyPath)
	} else {
		args = append(args, "--certificate-identity", p.identity, "--certificate-oidc-issuer", p.issuer)
	}
	return append(args, p.reference)
}

func resolveVerifyPolicy(v *viper.Viper) (verifyPolicy, error) {
	registryHost := v.GetString(flagRegistry)
	repository := v.GetString(flagRepository)
	tag := v.GetString(flagTag)
	keyPath := v.GetString(flagCosignKey)
	identity := v.GetString(flagCertIdentity)
	issuer := v.GetString(flagCertOIDCIssuer)

	switch {
	case registryHost == "":
		return verifyPolicy{}, errors.New("--registry is required")
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
