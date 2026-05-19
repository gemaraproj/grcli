// SPDX-License-Identifier: LicenseRef-Revanite-Proprietary

package cmd

import (
	"testing"

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
