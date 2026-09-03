// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGetCatalog_TypedSentinels confirms the wrapping shape: a real
// errors.Is check, not just a string match. Documents the contract
// versions.go relies on for clean user-facing error messages.
func TestGetCatalog_TypedSentinels(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   error
	}{
		{"404 → ErrCatalogNotFound", http.StatusNotFound, ErrCatalogNotFound},
		{"410 → ErrCatalogTombstoned", http.StatusGone, ErrCatalogTombstoned},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			_, err := New(srv.URL, "").GetCatalog(context.Background(), "ns", "id")
			if err == nil {
				t.Fatalf("expected error on %d, got nil", tc.status)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("errors.Is(%v, %v) = false", err, tc.want)
			}
		})
	}
}

// TestGetVersionBody covers the reference-resolution body fetch:
// the 200 path returns the body and the manifest-digest header, and the
// typed 404/410 sentinels surface for absent/yanked versions.
func TestGetVersionBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/versions/9.9.9"):
			w.WriteHeader(http.StatusNotFound)
		case strings.HasSuffix(r.URL.Path, "/versions/0.0.0"):
			w.WriteHeader(http.StatusGone)
		default:
			w.Header().Set("X-Gemara-Manifest-Digest", "sha256:deadbeef")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"metadata":{"id":"x"}}`))
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "")

	body, digest, err := c.GetVersionBody(context.Background(), "acme", "x", "1.0.0")
	if err != nil {
		t.Fatalf("GetVersionBody: %v", err)
	}
	if string(body) != `{"metadata":{"id":"x"}}` {
		t.Errorf("body = %q", body)
	}
	if digest != "sha256:deadbeef" {
		t.Errorf("digest = %q, want sha256:deadbeef", digest)
	}

	if _, _, err := c.GetVersionBody(context.Background(), "acme", "x", "9.9.9"); !errors.Is(err, ErrCatalogNotFound) {
		t.Errorf("absent version: err = %v, want ErrCatalogNotFound", err)
	}
	if _, _, err := c.GetVersionBody(context.Background(), "acme", "x", "0.0.0"); !errors.Is(err, ErrCatalogTombstoned) {
		t.Errorf("yanked version: err = %v, want ErrCatalogTombstoned", err)
	}
}

// TestReleaseFor_License confirms the per-version license decodes off
// releases[] and ReleaseFor selects the matching version.
func TestReleaseFor_License(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"namespace":"acme","catalog_id":"x","releases":[
			{"version":"1.0.0","manifest_digest":"sha256:a","license":"Apache-2.0"},
			{"version":"2.0.0","manifest_digest":"sha256:b","license":"MIT"}]}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "")

	cat, err := c.GetCatalog(context.Background(), "acme", "x")
	if err != nil {
		t.Fatalf("GetCatalog: %v", err)
	}
	if rel := cat.ReleaseFor("2.0.0"); rel == nil || rel.License != "MIT" {
		t.Errorf("ReleaseFor(2.0.0) license = %v, want MIT", rel)
	}
	if cat.ReleaseFor("3.0.0") != nil {
		t.Error("ReleaseFor(3.0.0) should be nil")
	}
}
