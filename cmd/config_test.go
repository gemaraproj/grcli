// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gemaraproj/go-gemara/bundle"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
)

// writeGlobalConfig writes the user-global config.yaml under the (isolated)
// XDG_CONFIG_HOME. isolatedWorkdir must have been called first.
func writeGlobalConfig(t *testing.T, content string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "grcli")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(content), 0o644))
}

func writeProjectConfig(t *testing.T, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(projectConfigFile, []byte(content), 0o644))
}

func loadedViper(t *testing.T) *viper.Viper {
	t.Helper()
	v := viper.New()
	require.NoError(t, loadConfig(v, "", io.Discard))
	return v
}

func TestConfig_DefaultCacheEnabled(t *testing.T) {
	isolatedWorkdir(t)
	require.True(t, cachingEnabled(loadedViper(t)), "caching is on by default with no config")
}

func TestConfig_GlobalDisablesCache(t *testing.T) {
	isolatedWorkdir(t)
	writeGlobalConfig(t, "cache-enabled: false\n")
	require.False(t, cachingEnabled(loadedViper(t)), "user-global cache-enabled:false must disable caching")
}

// TestConfig_ProjectFileIgnored: a repo-local ./.grcli.yaml is no longer
// read, so it cannot override the user-global file — the global setting
// stands even when a project file says otherwise.
func TestConfig_ProjectFileIgnored(t *testing.T) {
	isolatedWorkdir(t)
	writeGlobalConfig(t, "cache-enabled: false\n")
	writeProjectConfig(t, "cache-enabled: true\n")
	require.False(t, cachingEnabled(loadedViper(t)),
		"a project .grcli.yaml must be ignored — user-global cache-enabled:false stands")
}

func TestConfig_EnvOverridesGlobal(t *testing.T) {
	isolatedWorkdir(t)
	writeGlobalConfig(t, "cache-enabled: false\n")
	t.Setenv("GRCLI_CACHE_ENABLED", "true")
	require.True(t, cachingEnabled(loadedViper(t)), "GRCLI_* env must override the config file")
}

// TestConfig_GlobalFromHomeFallbackWhenXDGUnset covers the branch most users
// actually hit: XDG_CONFIG_HOME unset, so the user-global file is read from
// ~/.config/grcli/config.yaml. Every other test goes through isolatedWorkdir,
// which always sets XDG_CONFIG_HOME and hides this path.
func TestConfig_GlobalFromHomeFallbackWhenXDGUnset(t *testing.T) {
	home := t.TempDir()
	t.Chdir(t.TempDir()) // empty cwd → no project file
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "") // force the ~/.config fallback
	t.Setenv("GRCLI_CACHE_ENABLED", "")
	dir := filepath.Join(home, ".config", "grcli")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("cache-enabled: false\n"), 0o644))

	require.False(t, cachingEnabled(loadedViper(t)),
		"must read ~/.config/grcli/config.yaml when XDG_CONFIG_HOME is unset")
}

func TestConfig_ExplicitFileBypassesSearch(t *testing.T) {
	isolatedWorkdir(t)
	// The user-global file says disable; an explicit --config file says enable.
	// The explicit file must win and the global file must not be read.
	writeGlobalConfig(t, "cache-enabled: false\n")
	explicit := filepath.Join(t.TempDir(), "custom.yaml")
	require.NoError(t, os.WriteFile(explicit, []byte("cache-enabled: true\n"), 0o644))
	v := viper.New()
	require.NoError(t, loadConfig(v, explicit, io.Discard))
	require.True(t, cachingEnabled(v))
}

// TestWarnIgnoredConfig: warn whenever a config file sits at a location grcli
// no longer reads — the per-project ./.grcli.yaml or the legacy
// home/XDG dotfiles — and stay silent when there are none. Unlike the old
// legacy check, a present project file warns even when a user-global file
// exists, because the project file no longer merges over it.
func TestWarnIgnoredConfig(t *testing.T) {
	warned := func(t *testing.T) string {
		t.Helper()
		var buf bytes.Buffer
		warnIgnoredConfig(userGlobalConfigPath(), &buf)
		return buf.String()
	}

	t.Run("project ./.grcli.yaml present: warns even with a global file", func(t *testing.T) {
		isolatedWorkdir(t)
		writeGlobalConfig(t, "cache-enabled: true\n")
		writeProjectConfig(t, "url: x\n")
		out := warned(t)
		require.Contains(t, out, "ignoring config")
		require.Contains(t, out, ".grcli.yaml")
	})

	t.Run("legacy XDG dotfile present: warns", func(t *testing.T) {
		isolatedWorkdir(t)
		dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "grcli")
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".grcli.yaml"), []byte("url: x\n"), 0o644))
		require.Contains(t, warned(t), "ignoring config")
	})

	t.Run("legacy home dotfile present: warns", func(t *testing.T) {
		home := isolatedWorkdir(t)
		t.Chdir(t.TempDir()) // cwd must differ from HOME, or ~/.grcli.yaml doubles as the project file
		require.NoError(t, os.WriteFile(filepath.Join(home, ".grcli.yaml"), []byte("url: x\n"), 0o644))
		require.Contains(t, warned(t), "ignoring config")
	})

	t.Run("no ignored files: silent", func(t *testing.T) {
		isolatedWorkdir(t)
		require.Empty(t, warned(t))
	})
}

// TestConfig_EndToEnd_IgnoredWarningReachesStderr drives the REAL command path
// (root PersistentPreRunE → loadConfig → warnIgnoredConfig → the command's
// stderr): with a per-project ./.grcli.yaml seeded, any command run must
// surface the migration warning. Guards the call wiring, which the direct-
// helper tests above cannot (deleting the loadConfig call site would not fail
// them).
func TestConfig_EndToEnd_IgnoredWarningReachesStderr(t *testing.T) {
	isolatedWorkdir(t)
	writeProjectConfig(t, "url: x\n")

	// `cat` without --version fails in RunE — AFTER PersistentPreRunE has run
	// loadConfig — so the warning must already be on stderr.
	stdout, stderr, err := executeRootSplit("cat", "--source", "irrelevant")
	require.Error(t, err)
	require.Contains(t, stderr, "ignoring config", "loadConfig must emit the migration warning on the command's stderr")
	require.Empty(t, stdout)
}

// TestConfig_EndToEnd_CacheDisabledSkipsWarmEntry proves the config toggle wires
// through the command: with a warm cache entry but cache-enabled:false in the
// user-global config, unpack must bypass the cache and attempt (and fail
// offline) a network fetch.
func TestConfig_EndToEnd_CacheDisabledSkipsWarmEntry(t *testing.T) {
	c := tempCache(t)  // sets GRCLI_CACHE
	isolatedWorkdir(t) // sets XDG_CONFIG_HOME (after GRCLI_CACHE; both live)
	writeGlobalConfig(t, "cache-enabled: false\n")
	const url = "https://hub.invalid.test"
	seed := &bundle.Bundle{Source: bundle.File{Name: "controls.yaml", Data: []byte("id: from-cache\n")}}
	putBundle(c, hostOf(url), "acme", "controls", "1.0.0", seed, io.Discard)

	_, err := runRootExpectErr(t, "unpack", "--url", url, "--repository", "acme/controls",
		"--version", "1.0.0", "--output", filepath.Join(t.TempDir(), "out"))
	require.Error(t, err, "cache-enabled:false must bypass the warm entry and fail on the offline fetch")
}
