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
			name:    "missing-registry",
			args:    []string{"verify", "--repository", "r", "--tag", "t", "--cosign-key", "k"},
			wantSub: "--registry or --url is required",
		},
		{
			name: "url-plus-registry-conflict",
			args: []string{
				"verify",
				"--url", "https://hub.example",
				"--registry", "explicit.example",
				"--repository", "r", "--tag", "t", "--cosign-key", "k",
			},
			wantSub: "conflicting flags: --url and --registry",
		},
		{
			name:    "missing-repository",
			args:    []string{"verify", "--registry", "r", "--tag", "t", "--cosign-key", "k"},
			wantSub: "--repository is required",
		},
		{
			name:    "missing-tag",
			args:    []string{"verify", "--registry", "r", "--repository", "rep", "--cosign-key", "k"},
			wantSub: "--tag is required",
		},
		{
			name:    "no-trust-material",
			args:    []string{"verify", "--registry", "r", "--repository", "rep", "--tag", "t"},
			wantSub: "either --cosign-key or --certificate-identity is required",
		},
		{
			name: "both-key-and-keyless",
			args: []string{
				"verify", "--registry", "r", "--repository", "rep", "--tag", "t",
				"--cosign-key", "k",
				"--certificate-identity", "id",
				"--certificate-oidc-issuer", "https://example.com",
			},
			wantSub: "mutually exclusive",
		},
		{
			name: "keyless-missing-issuer",
			args: []string{
				"verify", "--registry", "r", "--repository", "rep", "--tag", "t",
				"--certificate-identity", "id",
			},
			wantSub: "requires both --certificate-identity and --certificate-oidc-issuer",
		},
		{
			name: "keyless-missing-identity",
			args: []string{
				"verify", "--registry", "r", "--repository", "rep", "--tag", "t",
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
		v.Set(flagTag, "1.0.0")
		v.Set(flagCosignKey, "/keys/cosign.pub")

		policy, err := resolveVerifyPolicy(context.Background(), v)
		require.NoError(t, err)
		require.Equal(t, "discovered.example/team/artifact:1.0.0", policy.reference,
			"cosign reference must be bare-host/repo:tag; a https:// prefix would cause cosign to reject the reference")
	})

	t.Run("url plus explicit registry is a conflict at policy resolution", func(t *testing.T) {
		v := viper.New()
		v.Set(flagURL, "https://hub.example")
		v.Set(flagRegistry, "explicit.example")
		v.Set(flagRepository, "team/artifact")
		v.Set(flagTag, "1.0.0")
		v.Set(flagCosignKey, "/keys/cosign.pub")

		_, err := resolveVerifyPolicy(context.Background(), v)
		require.Error(t, err)
		require.Contains(t, err.Error(), "conflicting flags: --url and --registry")
	})
}

func TestVerifyPolicy_CosignArgs(t *testing.T) {
	t.Run("key-mode", func(t *testing.T) {
		p := verifyPolicy{
			reference: "reg.example/team/artifact:1.0.0",
			keyPath:   "/keys/cosign.pub",
		}
		require.Equal(t, []string{
			"verify",
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
			"verify",
			"--certificate-identity", "https://github.com/team/repo/.github/workflows/publish.yml@refs/heads/main",
			"--certificate-oidc-issuer", "https://token.actions.githubusercontent.com",
			"reg.example/team/artifact:1.0.0",
		}, p.cosignArgs())
	})
}
