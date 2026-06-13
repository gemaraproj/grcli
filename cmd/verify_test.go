// SPDX-License-Identifier: LicenseRef-Revanite-Proprietary

package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
)

// Happy-path verify tests would need a real signed registry image and a
// usable cosign trust root — too much external state for a unit test
// suite. Flag-validation paths are well covered here; the cosign-shellout
// branch is one line and exercised manually.
func TestVerify_FlagValidation(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantSub string
	}{
		{
			// Pass --url="" to defeat the bake-in default — otherwise the
			// default would supply a registry source via discovery and this
			// test's "no registry source" premise wouldn't be reachable.
			name:    "missing-url",
			args:    []string{"verify", "--repository", "r", "--version", "t", "--cosign-key", "k", "--url", ""},
			wantSub: "--url is required",
		},
		{
			// A bogus --url is fine: flag validation runs before any hub
			// round-trip, so these cases never dial the host.
			name:    "missing-repository",
			args:    []string{"verify", "--url", "https://hub.example", "--version", "t", "--cosign-key", "k"},
			wantSub: "--repository is required",
		},
		{
			name:    "missing-version",
			args:    []string{"verify", "--url", "https://hub.example", "--repository", "rep", "--cosign-key", "k"},
			wantSub: "--version is required",
		},
		{
			name:    "no-trust-material",
			args:    []string{"verify", "--url", "https://hub.example", "--repository", "rep", "--version", "t"},
			wantSub: "either --cosign-key or --certificate-identity is required",
		},
		{
			name: "both-key-and-keyless",
			args: []string{
				"verify", "--url", "https://hub.example", "--repository", "rep", "--version", "t",
				"--cosign-key", "k",
				"--certificate-identity", "id",
				"--certificate-oidc-issuer", "https://example.com",
			},
			wantSub: "mutually exclusive",
		},
		{
			name: "keyless-missing-issuer",
			args: []string{
				"verify", "--url", "https://hub.example", "--repository", "rep", "--version", "t",
				"--certificate-identity", "id",
			},
			wantSub: "requires both --certificate-identity and --certificate-oidc-issuer",
		},
		{
			name: "keyless-missing-identity",
			args: []string{
				"verify", "--url", "https://hub.example", "--repository", "rep", "--version", "t",
				"--certificate-oidc-issuer", "https://example.com",
			},
			wantSub: "requires both --certificate-identity and --certificate-oidc-issuer",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolatedWorkdir(t)
			out, err := runRootExpectErr(t, tc.args...)
			require.Error(t, err, "expected error, got: %s", out)
			require.Contains(t, err.Error(), tc.wantSub)
		})
	}
}

// TestResolveVerifyPolicy_URL covers the ADR-0026 --url path through the
// verify command. Catches the BLOCKER from the post-ship QA pass: when
// --url drives discovery, the registry_url advertised by the hub carries
// a scheme (https://...), which cosign rejects as an invalid OCI image
// reference unless grcli strips it before composing <host>/<repo>:<tag>.
func TestResolveVerifyPolicy_URL(t *testing.T) {
	t.Run("url discovery yields a bare-host cosign reference", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"registry_url":"https://discovered.example/","hub_url":"https://hub.example","api_version":"v1"}`))
		}))
		defer srv.Close()

		v := viper.New()
		v.Set(flagURL, srv.URL)
		v.Set(flagRepository, "team/artifact")
		v.Set(flagVersion, "1.0.0")
		v.Set(flagCosignKey, "/keys/cosign.pub")

		policy, err := resolveVerifyPolicy(context.Background(), v)
		require.NoError(t, err)
		require.Equal(t, "discovered.example/team/artifact:1.0.0", policy.reference,
			"cosign reference must be bare-host/repo:tag; a https:// prefix would cause cosign to reject the reference")
	})
}

func TestVerifyPolicy_CosignArgs(t *testing.T) {
	t.Run("key-mode", func(t *testing.T) {
		p := verifyPolicy{
			reference: "reg.example/team/artifact:1.0.0",
			keyPath:   "/keys/cosign.pub",
		}
		require.Equal(t, []string{
			"verify", "--new-bundle-format",
			"--key", "/keys/cosign.pub",
			"reg.example/team/artifact:1.0.0",
		}, p.cosignArgs())
	})
	t.Run("keyless-mode", func(t *testing.T) {
		p := verifyPolicy{
			reference: "reg.example/team/artifact:1.0.0",
			identity:  "https://github.com/team/repo/.github/workflows/publish.yml@refs/heads/main",
			issuer:    "https://token.actions.githubusercontent.com",
		}
		require.Equal(t, []string{
			"verify", "--new-bundle-format",
			"--certificate-identity", "https://github.com/team/repo/.github/workflows/publish.yml@refs/heads/main",
			"--certificate-oidc-issuer", "https://token.actions.githubusercontent.com",
			"reg.example/team/artifact:1.0.0",
		}, p.cosignArgs())
	})
}
