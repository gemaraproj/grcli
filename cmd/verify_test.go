// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"

	"github.com/revanite-io/grcli/internal/sigverify"
)

// fakeCosignVersion puts a cosign on PATH that answers `cosign version[ --json]`
// with the given semver (and no-ops any other invocation). It lets the
// version-gated argv builder be tested deterministically, independent of
// whatever cosign the host happens to have.
func fakeCosignVersion(t *testing.T, version string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = version ]; then printf '{\"gitVersion\":\"" + version + "\"}\\n'; exit 0; fi\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "cosign"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake cosign: %v", err)
	}
	t.Setenv("PATH", dir)
}

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
			// issuer without identity (and no key) is an explicit error:
			// identity is the keyless trigger; a lone issuer has nothing to
			// bind to (ADR-0044). Identity WITHOUT issuer is NOT here — it
			// now succeeds by defaulting the issuer (see TestResolveVerifyPolicy_URL).
			name: "issuer-without-identity",
			args: []string{
				"verify", "--url", "https://hub.example", "--repository", "rep", "--version", "t",
				"--certificate-oidc-issuer", "https://example.com",
			},
			wantSub: "--certificate-oidc-issuer requires --certificate-identity",
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

	// discovery server shared by the issuer-default cases below.
	discovery := func(t *testing.T) *httptest.Server {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"registry_url":"https://discovered.example/","hub_url":"https://hub.example","api_version":"v1"}`))
		}))
		t.Cleanup(srv.Close)
		return srv
	}

	t.Run("keyless without issuer defaults to GitHub Actions", func(t *testing.T) {
		srv := discovery(t)
		v := viper.New()
		v.Set(flagURL, srv.URL)
		v.Set(flagRepository, "team/artifact")
		v.Set(flagVersion, "1.0.0")
		v.Set(flagCertIdentity, "https://github.com/team/repo/.github/workflows/publish.yml@refs/heads/main")
		// flagCertOIDCIssuer deliberately unset

		policy, err := resolveVerifyPolicy(context.Background(), v)
		require.NoError(t, err)
		require.Equal(t, defaultCertOIDCIssuer, policy.issuer,
			"keyless verify with no --certificate-oidc-issuer must default to the GitHub Actions issuer")
		require.Equal(t, defaultCertOIDCIssuer, policy.identityPolicy().Issuer,
			"the defaulted issuer must reach the in-process verifier")
	})

	t.Run("explicit issuer overrides the default", func(t *testing.T) {
		srv := discovery(t)
		v := viper.New()
		v.Set(flagURL, srv.URL)
		v.Set(flagRepository, "team/artifact")
		v.Set(flagVersion, "1.0.0")
		v.Set(flagCertIdentity, "id")
		v.Set(flagCertOIDCIssuer, "https://gitlab.example.com")

		policy, err := resolveVerifyPolicy(context.Background(), v)
		require.NoError(t, err)
		require.Equal(t, "https://gitlab.example.com", policy.issuer,
			"an explicit --certificate-oidc-issuer must override the GitHub Actions default")
	})
}

// TestVerifyPolicy_CosignArgs now covers ONLY key-mode: keyless verification
// moved in-process (ADR-0046), so cosign is the sole remaining shell-out and it
// only ever runs with --key. The keyless trust material is carried by
// identityPolicy() instead (TestVerifyPolicy_IdentityPolicy below).
func TestVerifyPolicy_CosignArgs(t *testing.T) {
	p := verifyPolicy{
		reference: "reg.example/team/artifact:1.0.0",
		keyPath:   "/keys/cosign.pub",
	}

	t.Run("2.6.x band passes --new-bundle-format", func(t *testing.T) {
		fakeCosignVersion(t, "v2.6.3")
		args, err := p.cosignArgs(context.Background())
		require.NoError(t, err)
		require.Equal(t, []string{
			"verify", "--new-bundle-format",
			"--key", "/keys/cosign.pub",
			"reg.example/team/artifact:1.0.0",
		}, args)
	})

	// cosign ≥ 3.0.0 makes the bundle format the default and deprecates the
	// flag; verify must omit it there to match what sign now produces.
	t.Run("3.x omits the deprecated flag", func(t *testing.T) {
		fakeCosignVersion(t, "v3.0.6")
		args, err := p.cosignArgs(context.Background())
		require.NoError(t, err)
		require.Equal(t, []string{
			"verify",
			"--key", "/keys/cosign.pub",
			"reg.example/team/artifact:1.0.0",
		}, args)
	})

	t.Run("cosign too old fails fast", func(t *testing.T) {
		fakeCosignVersion(t, "v2.2.0")
		_, err := p.cosignArgs(context.Background())
		require.Error(t, err)
		require.Contains(t, err.Error(), "2.6.0")
	})
}

// TestVerifyPolicy_IdentityPolicy pins the mapping from the resolved policy to
// the in-process sigstore-go identity pin — the security-critical seam that
// replaced cosign's --certificate-identity / --certificate-identity-regexp +
// --certificate-oidc-issuer flags (ADR-0046 decision 2). The exact-vs-regexp
// choice and the issuer must carry through byte-for-byte.
func TestVerifyPolicy_IdentityPolicy(t *testing.T) {
	t.Run("explicit keyless mode → exact SAN, exact issuer", func(t *testing.T) {
		p := verifyPolicy{
			identity: "https://github.com/team/repo/.github/workflows/publish.yml@refs/heads/main",
			issuer:   "https://token.actions.githubusercontent.com",
		}
		require.Equal(t, sigverify.IdentityPolicy{
			SAN:    "https://github.com/team/repo/.github/workflows/publish.yml@refs/heads/main",
			Issuer: "https://token.actions.githubusercontent.com",
		}, p.identityPolicy())
	})
	t.Run("hub-lookup mode → anchored SAN regexp, exact issuer", func(t *testing.T) {
		p := verifyPolicy{
			identityRegexp: `^https://github\.com/team/repo/\.github/workflows/publish\.yml@`,
			issuer:         "https://token.actions.githubusercontent.com",
		}
		require.Equal(t, sigverify.IdentityPolicy{
			SANRegexp: `^https://github\.com/team/repo/\.github/workflows/publish\.yml@`,
			Issuer:    "https://token.actions.githubusercontent.com",
		}, p.identityPolicy())
	})
}

// hubLookupServer serves both the discovery doc and a catalog detail so
// resolveVerifyPolicy's zero-flag path (discovery → GetCatalog) can run against
// httptest. signerIdentity is written into the catalog record; pass "" to omit
// the field entirely (simulating an artifact that predates hub-side
// verification). onCatalog, when non-nil, fires on each /v1/catalogs hit so a
// test can assert the catalog lookup did (or did NOT) happen.
func hubLookupServer(t *testing.T, signerIdentity string, onCatalog func()) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/.well-known/"):
			_, _ = w.Write([]byte(`{"registry_url":"https://discovered.example/","hub_url":"https://hub.example","api_version":"v1"}`))
		case strings.HasPrefix(r.URL.Path, "/v1/catalogs/"):
			if onCatalog != nil {
				onCatalog()
			}
			if signerIdentity == "" {
				_, _ = w.Write([]byte(`{"namespace":"team","catalog_id":"artifact"}`))
				return
			}
			_, _ = fmt.Fprintf(w, `{"namespace":"team","catalog_id":"artifact","signer_identity":%q}`, signerIdentity)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestResolveVerifyPolicy_HubLookup covers the zero-flag verify-by-coordinate
// path (ADR-0045 decision 8): no trust flags, so the signer identity is read
// from the hub's catalog record and turned into an anchored keyless cosign
// policy.
func TestResolveVerifyPolicy_HubLookup(t *testing.T) {
	baseViper := func(url string) *viper.Viper {
		v := viper.New()
		v.Set(flagURL, url)
		v.Set(flagRepository, "team/artifact")
		v.Set(flagVersion, "1.0.0")
		return v
	}

	t.Run("hub identity becomes an anchored, escaped identity regexp", func(t *testing.T) {
		// A workflow path carrying regexp metacharacters ('.') — the escaping is
		// load-bearing, so it must survive into the compiled matcher.
		const issuer = "https://token.actions.githubusercontent.com"
		const workflowPath = "https://github.com/acme/repo.name/.github/workflows/publish.yml"
		canonical := "keyless:" + issuer + "#" + workflowPath

		srv := hubLookupServer(t, canonical, nil)
		policy, err := resolveVerifyPolicy(context.Background(), baseViper(srv.URL))
		require.NoError(t, err)

		require.Equal(t, issuer, policy.issuer)
		require.Equal(t, canonical, policy.hubIdentity, "the raw hub record must be kept for the visible-trust announcement")
		require.Empty(t, policy.identity, "hub-lookup mode uses the regexp field, never the exact-identity field")

		wantRegexp := "^" + regexp.QuoteMeta(workflowPath) + "@"
		require.Equal(t, wantRegexp, policy.identityRegexp)
		require.True(t, strings.HasPrefix(policy.identityRegexp, "^"), "must anchor at start")
		require.True(t, strings.HasSuffix(policy.identityRegexp, "@"), "must require the SAN's @<ref> boundary")

		// The anchoring + escaping must admit any ref of THIS workflow while
		// refusing a wider or prefixed identity.
		re := regexp.MustCompile(policy.identityRegexp)
		require.True(t, re.MatchString(workflowPath+"@refs/tags/v1.0.0"), "any tag ref of the pinned workflow verifies")
		require.True(t, re.MatchString(workflowPath+"@refs/heads/main"), "any branch ref of the pinned workflow verifies")
		require.False(t, re.MatchString("https://evil.example/"+workflowPath+"@refs/tags/v1"), "^ anchor rejects a prefixed identity")
		require.False(t, re.MatchString(workflowPath+"-sibling/.github/workflows/publish.yml@refs/tags/v1"), "the @ boundary rejects a longer sibling path")
		// The escaped '.' must not act as a wildcard: a look-alike host differing
		// only where a literal '.' sits must not match.
		require.False(t, re.MatchString("https://github.com/acme/repoXname/.github/workflows/publish.yml@refs/tags/v1"), "escaped '.' must be a literal, not a wildcard")

		// The resolved policy feeds the in-process matcher (not cosign) — the
		// anchored regexp becomes the SAN-regexp pin, issuer stays exact.
		require.Equal(t, sigverify.IdentityPolicy{SANRegexp: wantRegexp, Issuer: issuer}, policy.identityPolicy())
		require.Equal(t, "keyless identity from hub record: "+canonical+", issuer "+issuer, policy.modeDescription())
	})

	t.Run("hub record without a signer identity is a clear, actionable error", func(t *testing.T) {
		srv := hubLookupServer(t, "", nil)
		_, err := resolveVerifyPolicy(context.Background(), baseViper(srv.URL))
		require.Error(t, err)
		require.Contains(t, err.Error(), "no recorded signer identity")
		require.Contains(t, err.Error(), "--cosign-key or --certificate-identity", "the error must point at the explicit-flag escape hatch")
	})

	t.Run("malformed hub identity fails loudly", func(t *testing.T) {
		for _, bad := range []string{"not-a-valid-identity", "keyless:issuer-without-hash"} {
			srv := hubLookupServer(t, bad, nil)
			_, err := resolveVerifyPolicy(context.Background(), baseViper(srv.URL))
			require.Error(t, err, "identity %q must be rejected", bad)
			require.Contains(t, err.Error(), "malformed")
		}
	})

	t.Run("unsupported key: scheme is rejected", func(t *testing.T) {
		srv := hubLookupServer(t, "key:sha256:abc123", nil)
		_, err := resolveVerifyPolicy(context.Background(), baseViper(srv.URL))
		require.Error(t, err)
		require.Contains(t, err.Error(), "unsupported scheme")
		require.Contains(t, err.Error(), `"key"`)
	})

	t.Run("explicit --certificate-identity bypasses the hub lookup entirely", func(t *testing.T) {
		called := 0
		srv := hubLookupServer(t, "key:should-never-be-read", func() { called++ })

		v := baseViper(srv.URL)
		v.Set(flagCertIdentity, "https://github.com/team/repo/.github/workflows/publish.yml@refs/heads/main")

		policy, err := resolveVerifyPolicy(context.Background(), v)
		require.NoError(t, err)
		require.Equal(t, 0, called, "the catalog record must not be fetched when the identity is supplied explicitly")
		require.Equal(t, "https://github.com/team/repo/.github/workflows/publish.yml@refs/heads/main", policy.identity)
		require.Empty(t, policy.identityRegexp, "explicit keyless mode uses the exact identity, not a regexp")
		require.Equal(t, defaultCertOIDCIssuer, policy.issuer, "explicit keyless still defaults the issuer (ADR-0044)")
	})

	t.Run("explicit --cosign-key bypasses the hub lookup entirely", func(t *testing.T) {
		called := 0
		srv := hubLookupServer(t, "key:should-never-be-read", func() { called++ })

		v := baseViper(srv.URL)
		v.Set(flagCosignKey, "/keys/cosign.pub")

		policy, err := resolveVerifyPolicy(context.Background(), v)
		require.NoError(t, err)
		require.Equal(t, 0, called, "the catalog record must not be fetched when a key is supplied")
		require.Equal(t, "/keys/cosign.pub", policy.keyPath)
	})
}
