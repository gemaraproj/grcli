// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gemaraproj/grc-store-clientkit/auth"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"

	"github.com/gemaraproj/grcli/internal/registry"
	"github.com/gemaraproj/grcli/internal/source"
)

func TestResolveTarget(t *testing.T) {
	// Mock hub discovery: --url is now the only way to a registry, so the
	// happy-path cases point --url at this server, which advertises
	// registry.example as the discovered host.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"registry_url":"registry.example","hub_url":"https://hub.example","api_version":"v1"}`))
	}))
	defer srv.Close()

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
				flagURL: srv.URL,
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
			name: "flag-repository-overrides-default",
			flags: map[string]any{
				flagURL:        srv.URL,
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
			name:       "missing-url-when-not-dry-run",
			flags:      map[string]any{},
			loaded:     loadedFull,
			wantErrSub: "--url is required",
		},
		{
			name:       "missing-tag",
			flags:      map[string]any{},
			loaded:     loadedNoMetadata,
			wantErrSub: "could not determine tag",
		},
		{
			name:  "missing-repository",
			flags: map[string]any{},
			loaded: &source.Loaded{
				Type:    "Policy",
				Version: "1.0.0", // satisfies the tag check so resolveTarget reaches the repository check
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

// TestResolveTargetURL covers the discovery hook: --url drives a
// discovery call to resolve the registry, and --dry-run skips it. Mock
// hub via httptest.
func TestResolveTargetURL(t *testing.T) {
	loaded := &source.Loaded{
		Type:     "Policy",
		ID:       "my-policy",
		Version:  "1.2.3",
		AuthorID: "my-team",
	}

	t.Run("url drives discovery, keeps the dial scheme, normalizes at composition", func(t *testing.T) {
		// Adversarial response: scheme included AND trailing slash, two
		// real malformations a hub operator can produce by setting
		// HUB_OCI_PUBLIC_URL = "https://registry.grc.store/".
		//
		// registryHost is the oras dial target, so it KEEPS the advertised
		// scheme — newRemoteRepo derives PlainHTTP from it, and stripping
		// http:// here would force HTTPS against a plain-HTTP zot. The
		// bare-host guarantee for cosign / the printed Reference / SLSA
		// provenance is enforced where those are composed, via
		// NormalizeRegistryHost (which also trims the trailing slash).
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"registry_url":"https://discovered.example/","hub_url":"https://hub.example","api_version":"v1"}`))
		}))
		defer srv.Close()
		// Discover() caches per process; unique httptest URLs per subtest
		// sidestep the cache without exposing the package's reset hook.

		v := viper.New()
		v.Set(flagURL, srv.URL)
		v.SetDefault(flagOutput, "grcli-out")

		got, err := resolveTarget(context.Background(), v, loaded)
		require.NoError(t, err)
		require.Equal(t, "https://discovered.example/", got.registryHost,
			"registryHost is the dial target — the advertised scheme must survive for PlainHTTP routing")
		require.Equal(t, "discovered.example", registry.NormalizeRegistryHost(got.registryHost),
			"normalizing the dial target yields the bare host used for cosign and OCI reference composition")
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
}

func TestResolveBearerToken(t *testing.T) {
	t.Run("uses GitHub Actions OIDC when present and no explicit token", func(t *testing.T) {
		tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"value":"gha.workflow.jwt"}`))
		}))
		defer tokenSrv.Close()
		discoSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"registry_url":"https://r","hub_url":"https://h","api_version":"v1","ci_audience":"https://hub.example/ci"}`))
		}))
		defer discoSrv.Close()

		t.Setenv("GITHUB_ACTIONS", "true")
		t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", tokenSrv.URL)
		t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "req-tok")

		v := viper.New()
		v.Set(flagURL, discoSrv.URL)

		got, err := resolveBearerToken(context.Background(), v)
		require.NoError(t, err)
		require.Equal(t, "gha.workflow.jwt", got,
			"in CI with no explicit token, the workflow OIDC token is the credential")
	})

	t.Run("explicit --token wins even inside GitHub Actions", func(t *testing.T) {
		// Point the Actions endpoint at an unreachable URL: if the GHA
		// path were taken it would fail, so a clean explicit return proves
		// the explicit token short-circuits before the GHA branch.
		t.Setenv("GITHUB_ACTIONS", "true")
		t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "http://127.0.0.1:0/should-not-be-called")
		t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "req-tok")

		v := viper.New()
		v.Set(flagToken, "explicit-tok")

		got, err := resolveBearerToken(context.Background(), v)
		require.NoError(t, err)
		require.Equal(t, "explicit-tok", got)
	})
}

// grcli registers --token, so a no-token error must name it. From
// grc-store-clientkit v0.1.1 the flag is listed only when App.TokenFlag is set,
// and dropping it would silently hide a fix the user can apply.
func TestGrcliApp_NoTokenErrorNamesTheFlag(t *testing.T) {
	err := &auth.ErrNoToken{App: grcliApp, Issuer: "https://issuer", CheckedStore: true}
	msg := err.Error()
	for _, want := range []string{"--token", "GRCLI_TOKEN", "grcli login"} {
		if !strings.Contains(msg, want) {
			t.Errorf("no-token error should name %q, got: %s", want, msg)
		}
	}
}
