// SPDX-License-Identifier: LicenseRef-Revanite-Proprietary

package hub

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestVersionExists_StatusMapping covers the typed status returns —
// 200/404/410 map to the three VersionStatus values, none of them an
// error. This is the publish pre-flight's load-bearing contract.
func TestVersionExists_StatusMapping(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   VersionStatus
	}{
		{"200 → present", http.StatusOK, VersionPresent},
		{"404 → absent", http.StatusNotFound, VersionAbsent},
		{"410 → tombstoned", http.StatusGone, VersionTombstoned},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			got, err := New(srv.URL, "").VersionExists(context.Background(), "ns", "id", "v1")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("VersionExists = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestVersionExists_UnexpectedStatus locks in the unified diagnostic
// shape (URL + status + body snippet). Before this round, the default
// branch dropped the body — operators chasing 5xx on the publish
// pre-flight got no upstream context.
func TestVersionExists_UnexpectedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`upstream timeout from zot`))
	}))
	defer srv.Close()

	_, err := New(srv.URL, "").VersionExists(context.Background(), "ns", "id", "v1")
	if err == nil {
		t.Fatal("expected error on 500, got nil")
	}
	msg := err.Error()
	for _, want := range []string{srv.URL, "500", "upstream timeout from zot"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q must contain %q (URL+status+body shape)", msg, want)
		}
	}
}

// TestSync_HappyPath verifies the decoded SyncResponse round-trips
// alongside the diagnostic-shape changes — guards against accidentally
// breaking the 2xx path while reshaping the error path.
func TestSync_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization header = %q, want Bearer test-token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"repository":"a/b","tag":"1.0.0","manifest_etag":"etag","artifact_count":3,"new_count":1,"types":["Policy"]}`))
	}))
	defer srv.Close()

	resp, err := New(srv.URL, "test-token").Sync(context.Background(), "a/b", "1.0.0")
	if err != nil {
		t.Fatalf("Sync error: %v", err)
	}
	if resp.Repository != "a/b" || resp.Tag != "1.0.0" || resp.ArtifactCount != 3 || resp.NewCount != 1 {
		t.Errorf("Sync response = %+v, want repository=a/b tag=1.0.0 artifact_count=3 new_count=1", resp)
	}
}

// TestSync_UnexpectedStatus is the matching diagnostic-shape test for
// Sync: URL was previously missing from the non-2xx error message.
func TestSync_UnexpectedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`backend unavailable`))
	}))
	defer srv.Close()

	_, err := New(srv.URL, "test-token").Sync(context.Background(), "a/b", "1.0.0")
	if err == nil {
		t.Fatal("expected error on 502, got nil")
	}
	msg := err.Error()
	for _, want := range []string{srv.URL, "502", "backend unavailable"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q must contain %q (URL+status+body shape)", msg, want)
		}
	}
}

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
