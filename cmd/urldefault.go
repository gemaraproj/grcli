// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// defaultURL is the bake-in target for grcli's `--url` flag. Until
// grcli grows private-hub adopters, the public grc.store API is the
// right default — most users running this tool today want it pointed
// at hub.grc.store, and asking them to remember the URL every time
// helps nobody. Override per invocation with `--url <other>` or per
// shell with `GRCLI_URL`. The discovery endpoint that backs `--url`
// is served by the hub at this URL, not the frontend at
// grc.store/ — the frontend Worker does not proxy /.well-known/ through.
const defaultURL = "https://hub.grc.store"

// suppressDefaultURLIfExplicit nulls out the bake-in `--url` value in
// viper when the user has explicitly set one of the listed flags. This
// keeps a subcommand's "--url is mutually exclusive with X" branch from
// spuriously firing on the default. With this in place, `grcli unpack
// --source ./layout` keeps working unchanged — the explicit --source
// signals "local mode, ignore the default --url" and the conflict branch
// does not see two competing sources.
//
// No-op when the user passed --url explicitly: in that case both flags
// are explicit and the conflict really IS a conflict, fire as before.
//
// Pass the flag names that are mutually exclusive with --url for the
// given subcommand: unpack uses --source.
func suppressDefaultURLIfExplicit(cmd *cobra.Command, v *viper.Viper, conflictsWith ...string) {
	if cmd.Flags().Changed(flagURL) {
		return
	}
	for _, name := range conflictsWith {
		if cmd.Flags().Changed(name) {
			v.Set(flagURL, "")
			return
		}
	}
}
