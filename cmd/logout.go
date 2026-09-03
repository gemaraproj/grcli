// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/gemaraproj/grc-store-clientkit/auth"
	"github.com/gemaraproj/grc-store-clientkit/hub"
)

func newLogoutCmd(v *viper.Viper) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logout",
		Short: "Forget stored credentials for a grc.store hub",
		Long: `Removes the stored access + refresh tokens for the hub at --url.
The hub itself is not contacted — logout is purely local. Other hubs
the user has logged into are untouched.

Pass --issuer to remove credentials by issuer URL when you know it
directly (e.g. the hub is unreachable and you can't run discovery).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLogout(cmd, v)
		},
	}
	flags := cmd.Flags()
	flags.String(flagURL, defaultURL, "grc.store base URL (discovers the issuer)")
	flags.String("issuer", "", "OIDC issuer URL to forget credentials for (alternative to --url)")
	return cmd
}

func runLogout(cmd *cobra.Command, v *viper.Viper) error {
	if err := v.BindPFlags(cmd.Flags()); err != nil {
		return fmt.Errorf("binding flags: %w", err)
	}
	suppressDefaultURLIfExplicit(cmd, v, "issuer")
	out := cmd.OutOrStdout()

	issuer := v.GetString("issuer")
	url := v.GetString(flagURL)
	switch {
	case issuer != "" && url != "":
		// Can only happen when both flags are explicitly set — the
		// suppress helper above clears the URL default in the --issuer-
		// only case. So treating this as a real conflict is correct.
		return fmt.Errorf("pass either --url or --issuer, not both")
	case issuer == "" && url == "":
		return fmt.Errorf("--url or --issuer is required")
	case url != "":
		disc, err := hub.Discover(cmd.Context(), url)
		if err != nil {
			return fmt.Errorf("hub discovery: %w (pass --issuer directly if the hub is unreachable)", err)
		}
		if disc.OIDCIssuer == "" {
			return fmt.Errorf("hub at %s does not advertise an OIDC issuer; pass --issuer directly", url)
		}
		issuer = disc.OIDCIssuer
	}

	store, err := auth.NewDefaultStore(grcliApp)
	if err != nil {
		return err
	}
	if err := store.Delete(issuer); err != nil {
		return fmt.Errorf("removing stored credentials: %w", err)
	}
	fmt.Fprintf(out, "✓ Forgot credentials for %s\n", issuer)
	return nil
}
