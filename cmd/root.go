// SPDX-License-Identifier: LicenseRef-Revanite-Proprietary

// Package cmd wires the grcli cobra/viper CLI.
package cmd

import (
	"fmt"
	"io"
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
		PersistentPreRunE: func(c *cobra.Command, _ []string) error {
			return loadConfig(v, cfgFile, c.ErrOrStderr())
		},
	}
	cmd.PersistentFlags().StringVar(&cfgFile, "config", "",
		"config file (default: ./.grcli.yaml merged over $XDG_CONFIG_HOME/grcli/config.yaml, or ~/.config/grcli/config.yaml)")

	cmd.AddCommand(newPublishCmd(v))
	cmd.AddCommand(newUnpackCmd(v))
	cmd.AddCommand(newCatCmd(v))
	cmd.AddCommand(newValidateCmd(v))
	cmd.AddCommand(newVerifyCmd(v))
	cmd.AddCommand(newVersionsCmd(v))
	cmd.AddCommand(newLoginCmd(v))
	cmd.AddCommand(newLogoutCmd(v))
	return cmd
}

// flagCacheEnabled is the config key (ADR-0043) that durably turns the artifact
// cache off (equivalent to passing --no-cache on every command). Default true.
// It is a FLAT key, not nested `cache.enabled`, on purpose: the $GRCLI_CACHE
// location env var (ADR-0039) shadows the whole `cache.*` namespace under
// viper's AutomaticEnv, which would mask a nested key's default and file value
// whenever $GRCLI_CACHE is set. The env form is GRCLI_CACHE_ENABLED.
const flagCacheEnabled = "cache-enabled"

// loadConfig wires the GRCLI_* env prefix and layers config files (ADR-0043).
// Precedence, highest first: explicit flag > GRCLI_* env > per-project
// ./.grcli.yaml > user-global $XDG_CONFIG_HOME/grcli/config.yaml > built-in
// default. The user-global file is read first as the base, then the project
// file is MERGED on top, so a personal preference holds unless a project (or
// env/flag) overrides it. --config <file> selects a single file and bypasses
// the search. A missing file is not an error; any other read error is a warning
// (on warn, the command's stderr) so the command still runs on env + flags.
func loadConfig(v *viper.Viper, cfgFile string, warn io.Writer) error {
	v.SetConfigType("yaml")
	v.SetEnvPrefix("GRCLI")
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_", ".", "_"))
	v.AutomaticEnv()
	v.SetDefault(flagCacheEnabled, true)

	if cfgFile != "" {
		v.SetConfigFile(cfgFile)
		if err := v.ReadInConfig(); err != nil {
			fmt.Fprintln(warn, "grcli: warning: reading config:", err)
		}
		return nil
	}

	// Base layer: the user-global file.
	if g := userGlobalConfigPath(); g != "" {
		warnIgnoredLegacyConfig(g, warn)
		if fileExists(g) {
			v.SetConfigFile(g)
			if err := v.ReadInConfig(); err != nil {
				fmt.Fprintln(warn, "grcli: warning: reading user config:", err)
			}
		}
	}
	// Override layer: the per-project file, merged on top.
	if fileExists(projectConfigFile) {
		v.SetConfigFile(projectConfigFile)
		if err := v.MergeInConfig(); err != nil {
			fmt.Fprintln(warn, "grcli: warning: reading project config:", err)
		}
	}
	return nil
}

// projectConfigFile is the per-project (repo-local) config, read from the
// current directory.
const projectConfigFile = ".grcli.yaml"

// warnIgnoredLegacyConfig warns when a config file from a retired pre-ADR-0043
// search location exists and would have been the ACTIVE config under the old
// first-match search (no project file, no new global file) — silence there
// would mean e.g. a hub URL quietly reverting to the prod default. Retired
// locations: `.grcli.yaml` inside the XDG grcli dir (the old search used the
// config name ".grcli" for every path) and the home-root `~/.grcli.yaml`.
func warnIgnoredLegacyConfig(globalPath string, w io.Writer) {
	if fileExists(projectConfigFile) || fileExists(globalPath) {
		return // old and new behavior read the same (or a newer) file; no surprise
	}
	var legacies []string
	// The old search only looked inside $XDG_CONFIG_HOME/grcli when XDG was
	// set; with XDG unset, ~/.config/grcli was never a search path, so a
	// dotfile there was never functional and gets no warning.
	if os.Getenv("XDG_CONFIG_HOME") != "" {
		legacies = append(legacies, filepath.Join(filepath.Dir(globalPath), ".grcli.yaml"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		legacies = append(legacies, filepath.Join(home, ".grcli.yaml"))
	}
	for _, legacy := range legacies {
		if fileExists(legacy) {
			fmt.Fprintf(w, "grcli: warning: ignoring legacy config %s — move it to %s\n", legacy, globalPath)
			return
		}
	}
}

// userGlobalConfigPath is the per-user config file: $XDG_CONFIG_HOME/grcli/
// config.yaml, falling back to ~/.config/grcli/config.yaml. Empty if the home
// directory can't be resolved.
func userGlobalConfigPath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "grcli", "config.yaml")
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
