// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/gemaraproj/grc-store-clientkit/auth"
	"github.com/gemaraproj/grc-store-clientkit/hub"
)

func newLoginCmd(v *viper.Viper) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Sign in to grc.store via OIDC device-authorization flow",
		Long: `Asks the hub at --url for its OIDC coordinates, runs the OAuth 2.0
Device Authorization Grant (RFC 8628), and stores the resulting
access + refresh tokens at ${XDG_DATA_HOME:-~/.local/share}/grcli/credentials.json
(0600 perms). Subsequent grcli publish calls auto-pick up the stored
token — no --token / GRCLI_TOKEN needed unless you want to override.

The login flow prints a verification URL and a short user code. Open
the URL in any browser you can reach (does NOT have to be the same
machine), enter the code, sign in, approve the request. grcli polls
the token endpoint and reports completion.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLogin(cmd, v)
		},
	}
	flags := cmd.Flags()
	flags.String(flagURL, defaultURL, "grc.store base URL")
	return cmd
}

func runLogin(cmd *cobra.Command, v *viper.Viper) error {
	if err := v.BindPFlags(cmd.Flags()); err != nil {
		return fmt.Errorf("binding flags: %w", err)
	}
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	url := v.GetString(flagURL)
	if url == "" {
		// Defensive — defaultURL is non-empty in production, but
		// belt-and-braces for any caller that hand-clears the value.
		return fmt.Errorf("--url is required")
	}

	fmt.Fprintf(out, "Discovering %s ...\n", url)
	disc, err := hub.Discover(ctx, url)
	if err != nil {
		return fmt.Errorf("hub discovery: %w", err)
	}
	if disc.OIDCIssuer == "" || disc.OIDCCLIClientID == "" {
		return fmt.Errorf("hub at %s does not advertise OIDC login — its discovery document has no oidc_issuer / oidc_cli_client_id fields, so `grcli login` has nothing to drive a device-grant flow against. Until the hub supports interactive login, you can still publish by passing a token via --token or GRCLI_TOKEN", url)
	}

	meta, err := auth.FetchOIDCMetadata(ctx, disc.OIDCIssuer)
	if err != nil {
		return err
	}

	da, err := auth.StartDeviceFlow(ctx, meta, disc.OIDCCLIClientID)
	if err != nil {
		return err
	}
	printDeviceInstructions(out, da)

	creds, err := auth.PollForToken(ctx, meta, disc.OIDCCLIClientID, da)
	if err != nil {
		return err
	}

	store, err := auth.NewDefaultStore(grcliApp)
	if err != nil {
		return err
	}
	if err := store.Put(creds); err != nil {
		return fmt.Errorf("saving credentials: %w", err)
	}
	fmt.Fprintf(out, "\n✓ Signed in to %s\n  Token stored at %s (expires %s)\n",
		disc.OIDCIssuer, store.Path, creds.ExpiresAt.Format(time.RFC3339))
	return nil
}

// printDeviceInstructions writes the user-facing block — the
// verification URL and the user code, with both the bare URL and the
// complete URL (when Keycloak provides one) so users on machines with
// a clipboard can paste the latter and skip typing the code.
func printDeviceInstructions(out io.Writer, da *auth.DeviceAuthorization) {
	fmt.Fprintln(out)
	if da.VerificationURIComplete != "" {
		fmt.Fprintf(out, "Open this URL in any browser to authorize:\n  %s\n", da.VerificationURIComplete)
		fmt.Fprintf(out, "Or visit %s and enter code:  %s\n", da.VerificationURI, da.UserCode)
	} else {
		fmt.Fprintf(out, "Visit %s and enter code:  %s\n", da.VerificationURI, da.UserCode)
	}
	if da.ExpiresIn > 0 {
		fmt.Fprintf(out, "(code expires in %s)\n", (time.Duration(da.ExpiresIn) * time.Second).Truncate(time.Second))
	}
	fmt.Fprintln(out, "Waiting for authorization...")
}
