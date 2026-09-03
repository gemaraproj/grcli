// SPDX-License-Identifier: Apache-2.0

// Package cmd wires the grcli cobra/viper CLI.
package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gemaraproj/grc-store-clientkit/auth"
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
		"config file (default: $XDG_CONFIG_HOME/grcli/config.yaml, or ~/.config/grcli/config.yaml)")

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

// flagCacheEnabled is the config key that durably turns the artifact
// cache off (equivalent to passing --no-cache on every command). Default true.
// It is a FLAT key, not nested `cache.enabled`, on purpose: the $GRCLI_CACHE
// location env var shadows the whole `cache.*` namespace under
// viper's AutomaticEnv, which would mask a nested key's default and file value
// whenever $GRCLI_CACHE is set. The env form is GRCLI_CACHE_ENABLED.
const flagCacheEnabled = "cache-enabled"

// grcliApp identifies this CLI to grc-store-clientkit: it picks the credential
// file under ${XDG_DATA_HOME:-~/.local/share}/grcli and names grcli (not pvtr,
// the other consumer of that module) in every "run `grcli login`" hint.
//
// GRCLI_TOKEN is named here for those messages; viper's GRCLI env prefix
// already merges the variable into the --token flag, so it reaches Resolve as
// an explicit token before the module's own environment lookup runs.
// TokenFlag is set because grcli registers --token (see publish.go): from
// clientkit v0.1.1 the flag is named in no-token errors only when the tool
// declares it, so omitting this would drop a fix the user can actually apply.
var grcliApp = auth.App{Name: "grcli", TokenEnv: "GRCLI_TOKEN", TokenFlag: "--token"}

// loadConfig wires the GRCLI_* env prefix and reads the single user-global
// config file. Precedence, highest first:
// explicit flag > GRCLI_* env > user-global $XDG_CONFIG_HOME/grcli/config.yaml
// (fallback ~/.config/grcli/config.yaml) > built-in default. There is NO
// per-project layer: a repo-local ./.grcli.yaml is deliberately not read
// — a committed config file steering a publish/verify tool is a
// footgun — so a present one earns a migration warning instead. --config
// <file> selects a single file and bypasses the search. A missing file is not
// an error; any other read error is a warning (on the command's stderr) so the
// command still runs on env + flags.
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

	g := userGlobalConfigPath()
	if g == "" {
		return nil // home dir unresolved — run on env + flags only
	}
	warnIgnoredConfig(g, warn)
	if fileExists(g) {
		v.SetConfigFile(g)
		if err := v.ReadInConfig(); err != nil {
			fmt.Fprintln(warn, "grcli: warning: reading user config:", err)
		}
	}
	return nil
}

// projectConfigFile is the repo-local config path. grcli no longer reads
// it; the constant remains so warnIgnoredConfig can nudge anyone
// migrating from the per-project layer to the user-global file.
const projectConfigFile = ".grcli.yaml"

// warnIgnoredConfig warns about config files sitting at locations grcli no
// longer reads, so a settings file isn't silently ignored after a layout
// change. Retired locations: the per-project ./.grcli.yaml and the
// legacy dotfiles (~/.grcli.yaml and $XDG_CONFIG_HOME/grcli/.grcli.yaml).
// The only blessed location is the user-global config.yaml (globalPath).
func warnIgnoredConfig(globalPath string, w io.Writer) {
	// Each candidate carries a display path (friendly, e.g. relative
	// ./.grcli.yaml) and an absolute path used only for dedup — running grcli
	// from $HOME makes ./.grcli.yaml and ~/.grcli.yaml the same file, which
	// must warn once, not twice.
	type candidate struct{ display, abs string }
	candidates := []candidate{}
	if abs, err := filepath.Abs(projectConfigFile); err == nil {
		candidates = append(candidates, candidate{projectConfigFile, abs})
	}
	// The pre-0043 search only looked inside $XDG_CONFIG_HOME/grcli when XDG
	// was set; with XDG unset, ~/.config/grcli was never a search path.
	if os.Getenv("XDG_CONFIG_HOME") != "" {
		p := filepath.Join(filepath.Dir(globalPath), ".grcli.yaml")
		candidates = append(candidates, candidate{p, p})
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".grcli.yaml")
		candidates = append(candidates, candidate{p, p})
	}
	seen := map[string]bool{}
	for _, c := range candidates {
		if seen[c.abs] || !fileExists(c.abs) {
			continue
		}
		seen[c.abs] = true
		fmt.Fprintf(w, "grcli: warning: ignoring config %s — grcli reads only %s; move your settings there\n", c.display, globalPath)
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
