// SPDX-License-Identifier: LicenseRef-Revanite-Proprietary

// Package cmd wires the grcli cobra/viper CLI.
package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// version is overwritten at build time via -ldflags.
var version = "dev"

// Execute is the package entry point called by main(). It builds a fresh
// command tree and viper instance for each invocation, which keeps tests
// from leaking state through package-level singletons.
func Execute() error {
	return newRootCmd().Execute()
}

// newRootCmd assembles the root command and the viper instance shared
// with its subcommands. The viper instance is populated by the root's
// PersistentPreRunE so subcommands see config + env values before their
// RunE fires.
func newRootCmd() *cobra.Command {
	v := viper.New()
	var cfgFile string

	cmd := &cobra.Command{
		Use:               "grcli",
		Short:             "Validate, publish, unpack, and verify Gemara artifact bundles against grc.store",
		SilenceUsage:      true,
		SilenceErrors:     true,
		Version:           version,
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
		// Cobra does NOT chain PersistentPreRunE: if a subcommand defines
		// its own, this one is silently skipped. If you add a subcommand
		// with its own PersistentPreRunE, call loadConfig from there too
		// (or refactor to a withConfig wrapper around RunE).
		PersistentPreRunE: func(*cobra.Command, []string) error {
			return loadConfig(v, cfgFile)
		},
	}
	cmd.PersistentFlags().StringVar(&cfgFile, "config", "",
		"config file (default: ./.grcli.yaml, $XDG_CONFIG_HOME/grcli/config.yaml, $HOME/.grcli.yaml)")

	cmd.AddCommand(newPublishCmd(v))
	cmd.AddCommand(newUnpackCmd(v))
	cmd.AddCommand(newValidateCmd(v))
	cmd.AddCommand(newVerifyCmd(v))
	cmd.AddCommand(newVersionsCmd(v))
	cmd.AddCommand(newLoginCmd(v))
	cmd.AddCommand(newLogoutCmd(v))
	return cmd
}

// loadConfig points viper at the right config file paths, wires up the
// GRCLI_* env prefix, and reads the config file if one is present.
// A missing default config file is not an error; any other read error
// is surfaced as a warning so the command can still run on env + flags.
func loadConfig(v *viper.Viper, cfgFile string) error {
	if cfgFile != "" {
		v.SetConfigFile(cfgFile)
	} else {
		v.SetConfigName(".grcli")
		v.SetConfigType("yaml")
		v.AddConfigPath(".")
		if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
			v.AddConfigPath(filepath.Join(xdg, "grcli"))
		}
		if home, err := os.UserHomeDir(); err == nil {
			v.AddConfigPath(home)
		}
	}
	v.SetEnvPrefix("GRCLI")
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_", ".", "_"))
	v.AutomaticEnv()

	err := v.ReadInConfig()
	if err == nil {
		return nil
	}
	var notFound viper.ConfigFileNotFoundError
	if errors.As(err, &notFound) {
		return nil
	}
	fmt.Fprintln(os.Stderr, "grcli: warning: reading config:", err)
	return nil
}
