// SPDX-License-Identifier: LicenseRef-Revanite-Proprietary

package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"

	"github.com/revanite-io/grcli/internal/hub"
	"github.com/revanite-io/grcli/internal/source"
)

func TestResolveTarget(t *testing.T) {
	loadedFull := &source.Loaded{
		Type:     "Policy",
		ID:       "my-policy",
		Version:  "1.2.3",
		AuthorID: "my-team",
	}
	loadedNoMetadata := &source.Loaded{
		Type: "Policy",
		ID:   "my-policy",
	}

	tests := []struct {
		name       string
		flags      map[string]any
		loaded     *source.Loaded
		wantErrSub string
		wantTarget publishTarget
	}{
		{
			name: "all-defaults-from-metadata",
			flags: map[string]any{
				flagRegistry: "registry.example",
			},
			loaded: loadedFull,
			wantTarget: publishTarget{
				registryHost: "registry.example",
				repository:   "my-team/my-policy",
				tag:          "1.2.3",
				output:       "grcli-out",
			},
		},
		{
			name: "flag-tag-overrides-metadata-version",
			flags: map[string]any{
				flagRegistry: "registry.example",
				flagTag:      "override",
			},
			loaded: loadedFull,
			wantTarget: publishTarget{
				registryHost: "registry.example",
				repository:   "my-team/my-policy",
				tag:          "override",
				output:       "grcli-out",
			},
		},
		{
			name: "flag-repository-overrides-default",
			flags: map[string]any{
				flagRegistry:   "registry.example",
				flagRepository: "custom/repo",
			},
			loaded: loadedFull,
			wantTarget: publishTarget{
				registryHost: "registry.example",
				repository:   "custom/repo",
				tag:          "1.2.3",
				output:       "grcli-out",
			},
		},
		{
			name: "dry-run-does-not-require-registry",
			flags: map[string]any{
				flagDryRun: true,
				flagOutput: "/tmp/out",
			},
			loaded: loadedFull,
			wantTarget: publishTarget{
				registryHost: "",
				repository:   "my-team/my-policy",
				tag:          "1.2.3",
				dryRun:       true,
				output:       "/tmp/out",
			},
		},
		{
			name:       "missing-registry-when-not-dry-run",
			flags:      map[string]any{},
			loaded:     loadedFull,
			wantErrSub: "--registry or --url is required",
		},
		{
			name: "missing-tag",
			flags: map[string]any{
				flagRegistry: "registry.example",
			},
			loaded:     loadedNoMetadata,
			wantErrSub: "could not determine tag",
		},
		{
			name: "missing-repository",
			flags: map[string]any{
				flagRegistry: "registry.example",
				flagTag:      "1.0.0",
			},
			loaded: &source.Loaded{
				Type: "Policy",
				// no ID, no AuthorID — defaultRepository returns ""
			},
			wantErrSub: "could not determine --repository",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := viper.New()
			for k, val := range tc.flags {
				v.Set(k, val)
			}
			// Output default mirrors the flag default; resolveTarget reads
			// it via viper, so set it unless the test overrode it.
			if _, ok := tc.flags[flagOutput]; !ok {
				v.SetDefault(flagOutput, "grcli-out")
			}

			got, err := resolveTarget(context.Background(), v, tc.loaded)
			if tc.wantErrSub != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErrSub)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantTarget, got)
		})
	}
}

// TestResolveTargetURL covers the ADR-0026 discovery hook: --url alone
// drives a discovery call, --url + --registry is a hard error, --url +
// --hub-url is a hard error. Mock hub via httptest.
func TestResolveTargetURL(t *testing.T) {
	loaded := &source.Loaded{
		Type:     "Policy",
		ID:       "my-policy",
		Version:  "1.2.3",
		AuthorID: "my-team",
	}

	t.Run("url drives discovery when registry is unset", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"registry_url":"https://discovered.example","hub_url":"https://hub.example","api_version":"v1"}`))
		}))
		defer srv.Close()
		// Discover() caches; clear between subtests via the package hook
		// the hub_test.go exposes — but it's internal to that package and
		// not exported here. Use a unique URL per subtest to sidestep the
		// cache instead.

		v := viper.New()
		v.Set(flagURL, srv.URL)
		v.SetDefault(flagOutput, "grcli-out")

		got, err := resolveTarget(context.Background(), v, loaded)
		require.NoError(t, err)
		require.Equal(t, "https://discovered.example", got.registryHost,
			"registry should come from discovery when --registry is unset")
	})

	t.Run("url plus explicit registry is a conflict", func(t *testing.T) {
		v := viper.New()
		v.Set(flagURL, "https://hub.example")
		v.Set(flagRegistry, "explicit.example")
		v.SetDefault(flagOutput, "grcli-out")

		_, err := resolveTarget(context.Background(), v, loaded)
		require.Error(t, err)
		require.Contains(t, err.Error(), "conflicting flags")
		require.Contains(t, err.Error(), "--url and --registry")
	})

	t.Run("url plus explicit hub-url is a conflict", func(t *testing.T) {
		v := viper.New()
		v.Set(flagURL, "https://hub.example")
		v.Set(flagHubURL, "https://other.example")
		v.SetDefault(flagOutput, "grcli-out")

		_, err := resolveTarget(context.Background(), v, loaded)
		require.Error(t, err)
		require.Contains(t, err.Error(), "conflicting flags")
		require.Contains(t, err.Error(), "--url and --hub-url")
	})

	t.Run("explicit registry skips discovery entirely", func(t *testing.T) {
		// httptest server that fails the test if it's hit — explicit
		// --registry must not trigger discovery.
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			t.Error("discovery endpoint was hit despite explicit --registry")
		}))
		defer srv.Close()
		// Don't set --url, set only --registry; discovery server only
		// here to fail the test if accidentally called.
		_ = srv.URL

		v := viper.New()
		v.Set(flagRegistry, "explicit.example")
		v.SetDefault(flagOutput, "grcli-out")

		got, err := resolveTarget(context.Background(), v, loaded)
		require.NoError(t, err)
		require.Equal(t, "explicit.example", got.registryHost)
	})

	t.Run("dry-run with url does not trigger discovery", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			t.Error("discovery endpoint hit during dry-run — should be skipped")
		}))
		defer srv.Close()

		v := viper.New()
		v.Set(flagURL, srv.URL)
		v.Set(flagDryRun, true)
		v.SetDefault(flagOutput, "grcli-out")

		got, err := resolveTarget(context.Background(), v, loaded)
		require.NoError(t, err)
		require.True(t, got.dryRun)
		require.Equal(t, "", got.registryHost, "dry-run should not need a registry")
	})

	// Belt-and-braces: any cache state left over from earlier subtests
	// should not leak into other test files. Force a fresh state if the
	// discover_test exports a reset (it's package-internal).
	_ = hub.Discovery{} // keep the hub import alive in case future tests use it
}
