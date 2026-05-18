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

var (
	cfgFile string
	rootCmd = &cobra.Command{
		Use:           "grcli",
		Short:         "Publish Gemara artifact bundles to grc.store",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version,
	}
)

// Execute is the package entry point called by main().
func Execute() error {
	return rootCmd.Execute()
}

func init() {
	cobra.OnInitialize(initConfig)
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "",
		"config file (default: ./.grcli.yaml, $XDG_CONFIG_HOME/grcli/config.yaml, $HOME/.grcli.yaml)")
	rootCmd.AddCommand(newPublishCmd())
}

func initConfig() {
	if cfgFile != "" {
		viper.SetConfigFile(cfgFile)
	} else {
		viper.SetConfigName(".grcli")
		viper.SetConfigType("yaml")
		viper.AddConfigPath(".")
		if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
			viper.AddConfigPath(filepath.Join(xdg, "grcli"))
		}
		if home, err := os.UserHomeDir(); err == nil {
			viper.AddConfigPath(home)
		}
	}
	viper.SetEnvPrefix("GRCLI")
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_", ".", "_"))
	viper.AutomaticEnv()
	if err := viper.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if !errors.As(err, &notFound) {
			fmt.Fprintln(os.Stderr, "grcli: warning: reading config:", err)
		}
	}
}
