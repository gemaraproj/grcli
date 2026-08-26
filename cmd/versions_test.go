// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/revanite-io/grcli/internal/hub"
)

// catalogBodyTwoReleases is the happy-path JSON the hub returns for a
// catalog with two releases. Matches the live shape exercised against
// hub.grc.store during development.
const catalogBodyTwoReleases = `{
  "namespace":"finos-ccc","catalog_id":"ccc.objstor.cn",
  "type":"ControlCatalog","category":"catalog",
  "title":"CCC Object Storage Controls","summary":"...",
  "author_name":"FINOS Common Cloud Controls",
  "latest_version":"v2026.06-rc2",
  "latest_manifest_digest":"sha256:a787ca997ebb5730404294dce11b1c6b987e39ba393e0f81c1400281e92e7a84",
  "releases":[
    {"version":"v2026.06-rc2","manifest_digest":"sha256:a787ca997ebb5730404294dce11b1c6b987e39ba393e0f81c1400281e92e7a84","pushed_at":"2026-05-28T16:26:17.735477Z"},
    {"version":"v2026.06-rc1","manifest_digest":"sha256:951423bb3df92318ca452cc698432b703d7330275fda8023fd4babee1768824e","pushed_at":"2026-05-28T01:41:37.650829Z"}
  ]
}`

// fakeHub serves the canned response/status for /v1/catalogs/{ns}/{id}
// and rejects unexpected paths so a routing typo trips the test.
func fakeHub(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/catalogs/") {
			t.Errorf("unexpected hub path: %s", r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.WriteHeader(status)
		if body != "" {
			_, _ = w.Write([]byte(body))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestVersions_HappyPath(t *testing.T) {
	srv := fakeHub(t, http.StatusOK, catalogBodyTwoReleases)
	isolatedWorkdir(t)

	out := runRoot(t, "versions", "finos-ccc/ccc.objstor.cn", "--url", srv.URL)

	require.Contains(t, out, "VERSION")
	require.Contains(t, out, "v2026.06-rc2 (latest)")
	require.Contains(t, out, "v2026.06-rc1")
	require.NotContains(t, out, "v2026.06-rc1 (latest)",
		"only the latest_version row should carry the (latest) marker")

	// Digests are truncated to the docker/oras short form.
	require.Contains(t, out, "sha256:a787ca997ebb")
	require.Contains(t, out, "sha256:951423bb3df9")
	require.NotContains(t, out, "a787ca997ebb5730404294dce11b1c6b987e39ba393e0f81c1400281e92e7a84",
		"full sha256 digest should not appear in the default table")
}

func TestVersions_Latest_PrintsOnlyVersion(t *testing.T) {
	srv := fakeHub(t, http.StatusOK, catalogBodyTwoReleases)
	isolatedWorkdir(t)

	out := runRoot(t, "versions", "finos-ccc/ccc.objstor.cn", "--url", srv.URL, "--latest")

	require.Equal(t, "v2026.06-rc2\n", out,
		"--latest must emit exactly the version string plus a trailing newline for pipeline use")
}

func TestVersions_ReleasesAlias(t *testing.T) {
	srv := fakeHub(t, http.StatusOK, catalogBodyTwoReleases)
	isolatedWorkdir(t)

	out := runRoot(t, "releases", "finos-ccc/ccc.objstor.cn", "--url", srv.URL)
	require.Contains(t, out, "v2026.06-rc2 (latest)",
		"`grcli releases` must alias to `grcli versions`")
}

func TestVersions_NotFound(t *testing.T) {
	srv := fakeHub(t, http.StatusNotFound, `{"error":"not found"}`)
	isolatedWorkdir(t)

	_, err := runRootExpectErr(t, "versions", "does-not-exist/nope", "--url", srv.URL)
	require.Error(t, err)
	require.True(t, errors.Is(err, hub.ErrCatalogNotFound),
		"404 must surface as ErrCatalogNotFound, got %v", err)
	require.Contains(t, err.Error(), "does-not-exist/nope")
	// Assert on the actual leak surface, not just the httptest URL:
	// "http" catches both http:// and https:// hub URLs, and the
	// production default URL catches a regression where the message
	// embeds defaultURL even when --url overrides it.
	require.NotContains(t, err.Error(), "http",
		"user-facing 404 error must not embed any URL")
	require.NotContains(t, err.Error(), defaultURL,
		"user-facing 404 error must not embed the default hub URL")
}

func TestVersions_Tombstoned(t *testing.T) {
	srv := fakeHub(t, http.StatusGone, `{"error":"yanked"}`)
	isolatedWorkdir(t)

	_, err := runRootExpectErr(t, "versions", "finos-ccc/ccc.objstor.cn", "--url", srv.URL)
	require.Error(t, err)
	require.True(t, errors.Is(err, hub.ErrCatalogTombstoned),
		"410 must surface as ErrCatalogTombstoned, got %v", err)
	require.Contains(t, err.Error(), "yanked")
	require.NotContains(t, err.Error(), "http",
		"user-facing 410 error must not embed any URL")
	require.NotContains(t, err.Error(), defaultURL,
		"user-facing 410 error must not embed the default hub URL")
}

func TestVersions_EmptyReleases_Errors(t *testing.T) {
	const body = `{"namespace":"a","catalog_id":"b","latest_version":"","releases":[]}`
	srv := fakeHub(t, http.StatusOK, body)
	isolatedWorkdir(t)

	_, err := runRootExpectErr(t, "versions", "a/b", "--url", srv.URL)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no releases")
}

func TestVersions_Latest_EmptyLatest_SilentExit(t *testing.T) {
	const body = `{"namespace":"a","catalog_id":"b","latest_version":"","releases":[]}`
	srv := fakeHub(t, http.StatusOK, body)
	isolatedWorkdir(t)

	out := runRoot(t, "versions", "a/b", "--url", srv.URL, "--latest")
	require.Equal(t, "", out,
		"--latest on a catalog with no latest_version must exit 0 with empty stdout so xargs pipelines stay clean")
}

// TestVersions_UnexpectedStatus locks in the diagnostic shape of the
// default branch in GetCatalog: a 5xx or other unmodeled status SHOULD
// embed the hub URL in the error so an operator can see which hub
// returned the unexpected status. Pairs with the 404/410 tests above,
// which assert URL ABSENCE for hub-modeled outcomes — together they
// document the contract: known statuses redact, unknown statuses leak
// for debuggability.
func TestVersions_UnexpectedStatus(t *testing.T) {
	srv := fakeHub(t, http.StatusInternalServerError, `{"error":"boom"}`)
	isolatedWorkdir(t)

	_, err := runRootExpectErr(t, "versions", "a/b", "--url", srv.URL)
	require.Error(t, err)
	require.Contains(t, err.Error(), "500")
	require.Contains(t, err.Error(), srv.URL,
		"unmodeled status codes should embed the hub URL for operator debugging")
}

func TestVersions_EmptyBody200(t *testing.T) {
	srv := fakeHub(t, http.StatusOK, "")
	isolatedWorkdir(t)

	_, err := runRootExpectErr(t, "versions", "a/b", "--url", srv.URL)
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty body",
		"a 200 with empty body must be surfaced explicitly, not as a JSON-decode error")
}

func TestVersions_MalformedJSON(t *testing.T) {
	srv := fakeHub(t, http.StatusOK, `{ not valid json`)
	isolatedWorkdir(t)

	_, err := runRootExpectErr(t, "versions", "a/b", "--url", srv.URL)
	require.Error(t, err)
	require.Contains(t, err.Error(), "decoding catalog response")
}

func TestVersions_BadCoordinate(t *testing.T) {
	// No HTTP server: arg parsing must reject these before any dial.
	cases := []struct {
		name  string
		coord string
	}{
		{"no-slash", "finos-ccc"},
		{"empty-id", "finos-ccc/"},
		{"empty-ns", "/ccc.objstor.cn"},
		{"three-segments", "finos-ccc/ccc.objstor.cn/extra"},
		{"only-slash", "/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolatedWorkdir(t)
			_, err := runRootExpectErr(t, "versions", tc.coord, "--url", "https://hub.example")
			require.Error(t, err)
			require.Contains(t, err.Error(), "expected <namespace>/<catalog-id>")
		})
	}
}

// TestShortDigest documents the truncation contract. The sha256 case
// is exercised end-to-end by TestVersions_HappyPath; this table covers
// the boundary, fall-through, and unrecognized-algorithm branches so a
// future change to digestDisplayLen or the prefix check can't silently
// regress the non-sha256 path.
func TestShortDigest(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "sha256-full-64-truncates-to-12",
			in:   "sha256:a787ca997ebb5730404294dce11b1c6b987e39ba393e0f81c1400281e92e7a84",
			want: "sha256:a787ca997ebb",
		},
		{
			name: "sha256-exactly-12-hex-unchanged",
			in:   "sha256:a787ca997ebb",
			want: "sha256:a787ca997ebb",
		},
		{
			name: "sha256-shorter-than-12-unchanged",
			in:   "sha256:abc",
			want: "sha256:abc",
		},
		{
			name: "sha512-unrecognized-prefix-passes-through",
			in:   "sha512:b1946ac92492d2347c6235b4d2611184c1bfa4ce4eaa4d4d0a9d4f5e8c8b1234",
			want: "sha512:b1946ac92492d2347c6235b4d2611184c1bfa4ce4eaa4d4d0a9d4f5e8c8b1234",
		},
		{
			name: "empty-string-unchanged",
			in:   "",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, shortDigest(tc.in))
		})
	}
}

func TestVersions_MissingURL(t *testing.T) {
	isolatedWorkdir(t)
	_, err := runRootExpectErr(t, "versions", "a/b", "--url", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "--url is required")
}

func TestVersions_MissingLatestField_OmitsLatestMarker(t *testing.T) {
	// latest_version absent → no row should be marked (latest), but all
	// releases must still print. Guards against a regression where the
	// marker logic defaults to the first row when the field is empty.
	const body = `{
      "namespace":"a","catalog_id":"b",
      "releases":[
        {"version":"1.0.0","manifest_digest":"sha256:111111111111111111111111111111111111111111111111111111111111aaaa","pushed_at":"2026-01-01T00:00:00Z"},
        {"version":"0.9.0","manifest_digest":"sha256:222222222222222222222222222222222222222222222222222222222222bbbb","pushed_at":"2025-12-01T00:00:00Z"}
      ]
    }`
	srv := fakeHub(t, http.StatusOK, body)
	isolatedWorkdir(t)

	out := runRoot(t, "versions", "a/b", "--url", srv.URL)
	require.Contains(t, out, "1.0.0")
	require.Contains(t, out, "0.9.0")
	require.NotContains(t, out, "(latest)",
		"no row should be marked (latest) when the hub omits latest_version")
}
