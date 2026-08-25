// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDiscover(t *testing.T) {
	t.Run("happy path returns parsed discovery doc", func(t *testing.T) {
		resetDiscoveryCacheForTest()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/.well-known/grc-store-configuration" {
				t.Errorf("requested path = %q, want /.well-known/grc-store-configuration", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"registry_url":"https://registry.example","hub_url":"https://hub.example","api_version":"v1"}`))
		}))
		defer srv.Close()

		d, err := Discover(context.Background(), srv.URL)
		if err != nil {
			t.Fatalf("Discover error: %v", err)
		}
		if d.RegistryURL != "https://registry.example" {
			t.Errorf("RegistryURL = %q, want https://registry.example", d.RegistryURL)
		}
		if d.HubURL != "https://hub.example" {
			t.Errorf("HubURL = %q, want https://hub.example", d.HubURL)
		}
		if d.APIVersion != "v1" {
			t.Errorf("APIVersion = %q, want v1", d.APIVersion)
		}
	})

	t.Run("cache hit avoids a second HTTP call", func(t *testing.T) {
		resetDiscoveryCacheForTest()
		var hits int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt32(&hits, 1)
			_, _ = w.Write([]byte(`{"registry_url":"https://r","hub_url":"https://h","api_version":"v1"}`))
		}))
		defer srv.Close()

		for i := 0; i < 3; i++ {
			if _, err := Discover(context.Background(), srv.URL); err != nil {
				t.Fatalf("call %d error: %v", i, err)
			}
		}
		if got := atomic.LoadInt32(&hits); got != 1 {
			t.Errorf("HTTP hits = %d, want 1 (cache should have served calls 2 and 3)", got)
		}
	})

	t.Run("trailing slash on base URL is normalized to cache hit", func(t *testing.T) {
		resetDiscoveryCacheForTest()
		var hits int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt32(&hits, 1)
			_, _ = w.Write([]byte(`{"registry_url":"https://r","hub_url":"https://h","api_version":"v1"}`))
		}))
		defer srv.Close()

		if _, err := Discover(context.Background(), srv.URL); err != nil {
			t.Fatalf("first call: %v", err)
		}
		if _, err := Discover(context.Background(), srv.URL+"/"); err != nil {
			t.Fatalf("trailing-slash call: %v", err)
		}
		if got := atomic.LoadInt32(&hits); got != 1 {
			t.Errorf("HTTP hits = %d, want 1 (trailing slash should hit cache)", got)
		}
	})

	t.Run("missing registry_url is a loud error", func(t *testing.T) {
		resetDiscoveryCacheForTest()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"hub_url":"https://h","api_version":"v1"}`))
		}))
		defer srv.Close()

		_, err := Discover(context.Background(), srv.URL)
		if err == nil {
			t.Fatal("expected error for missing registry_url, got nil")
		}
		if !strings.Contains(err.Error(), "registry_url") {
			t.Errorf("error = %q, want it to mention registry_url", err.Error())
		}
		if !strings.Contains(err.Error(), srv.URL) {
			t.Errorf("error = %q, want it to name the hub URL we called", err.Error())
		}
	})

	t.Run("malformed JSON surfaces a parse error with body excerpt", func(t *testing.T) {
		resetDiscoveryCacheForTest()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`not json {{{`))
		}))
		defer srv.Close()

		_, err := Discover(context.Background(), srv.URL)
		if err == nil {
			t.Fatal("expected error for malformed JSON, got nil")
		}
		if !strings.Contains(err.Error(), srv.URL) {
			t.Errorf("error = %q, want it to name the hub URL", err.Error())
		}
	})

	t.Run("404 surfaces the status code and a useful message", func(t *testing.T) {
		resetDiscoveryCacheForTest()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`not found`))
		}))
		defer srv.Close()

		_, err := Discover(context.Background(), srv.URL)
		if err == nil {
			t.Fatal("expected error for 404, got nil")
		}
		if !strings.Contains(err.Error(), "404") {
			t.Errorf("error = %q, want it to include 404", err.Error())
		}
	})

	t.Run("500 surfaces the status code", func(t *testing.T) {
		resetDiscoveryCacheForTest()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"registry_url_unconfigured"}`))
		}))
		defer srv.Close()

		_, err := Discover(context.Background(), srv.URL)
		if err == nil {
			t.Fatal("expected error for 500, got nil")
		}
		if !strings.Contains(err.Error(), "500") {
			t.Errorf("error = %q, want it to include 500", err.Error())
		}
	})

	t.Run("empty base URL fails before hitting the network", func(t *testing.T) {
		resetDiscoveryCacheForTest()
		_, err := Discover(context.Background(), "")
		if err == nil {
			t.Fatal("expected error for empty base URL, got nil")
		}
	})

	t.Run("decodes ci_audience for trusted publishing", func(t *testing.T) {
		resetDiscoveryCacheForTest()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"registry_url":"https://r","hub_url":"https://h","api_version":"v1","ci_audience":"https://hub.example/ci"}`))
		}))
		defer srv.Close()

		d, err := Discover(context.Background(), srv.URL)
		if err != nil {
			t.Fatalf("Discover error: %v", err)
		}
		if d.CIAudience != "https://hub.example/ci" {
			t.Errorf("CIAudience = %q, want https://hub.example/ci", d.CIAudience)
		}
	})

	t.Run("ci_audience absent leaves the field empty", func(t *testing.T) {
		resetDiscoveryCacheForTest()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"registry_url":"https://r","hub_url":"https://h","api_version":"v1"}`))
		}))
		defer srv.Close()

		d, err := Discover(context.Background(), srv.URL)
		if err != nil {
			t.Fatalf("Discover error: %v", err)
		}
		if d.CIAudience != "" {
			t.Errorf("CIAudience = %q, want empty when not advertised", d.CIAudience)
		}
	})
}
