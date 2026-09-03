// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gemaraproj/go-gemara/bundle"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"

	"github.com/gemaraproj/grcli/internal/cache"
)

func tempCache(t *testing.T) *cache.Cache {
	t.Helper()
	t.Setenv("GRCLI_CACHE", t.TempDir())
	c, err := cache.Open()
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	return c
}

// TestPutBundleRoundtrip is the core Phase 2 guarantee: a bundle cached on a
// miss and reconstructed on a hit yields byte-identical files and a
// byte-identical bundle.json — so unpack/cat output can't drift between a
// fresh pull and a cache hit.
func TestPutBundleRoundtrip(t *testing.T) {
	c := tempCache(t)
	orig := &bundle.Bundle{
		Files: []bundle.File{
			{Name: "controls.yaml", Type: "ControlCatalog", Data: []byte("id: acme\n")},
			{Name: "mappings.yaml", Type: "Mapping", Data: []byte("maps: []\n")},
		},
		Manifest: bundle.Manifest{
			BundleVersion: "1",
			GemaraVersion: "0.5.0",
			Revision:      "abc",
			// Exercise the nested shapes real publishes populate — a
			// map[string]any (like the SLSA provenance predicate) and an
			// Artifacts slice — so the JSON decode→re-encode round trip is
			// tested against the fields most likely to drift, not just strings.
			Metadata: map[string]any{
				"provenance": map[string]any{
					"builder":   "grcli",
					"buildType": "https://example/slsa",
					"count":     3,
				},
			},
			Artifacts: []bundle.Artifact{
				{Name: "controls.yaml", Type: "ControlCatalog", ID: "acme", Role: "primary"},
			},
		},
		Etag: "sha256:deadbeef",
	}
	var out bytes.Buffer
	putBundle(c, "hub.grc.store", "acme", "x", "1.0.0", orig, &out)
	if out.Len() != 0 {
		t.Fatalf("unexpected putBundle output: %q", out.String())
	}

	e, found, err := c.Get("hub.grc.store", "acme", "x", "1.0.0")
	if err != nil || !found {
		t.Fatalf("Get: found=%v err=%v", found, err)
	}
	got, err := bundleFromEntry(e)
	if err != nil {
		t.Fatalf("bundleFromEntry: %v", err)
	}

	if len(got.Files) != len(orig.Files) {
		t.Fatalf("got %d files, want %d", len(got.Files), len(orig.Files))
	}
	for i := range orig.Files {
		if got.Files[i].Name != orig.Files[i].Name || !bytes.Equal(got.Files[i].Data, orig.Files[i].Data) {
			t.Errorf("file %d = {%q,%q}, want {%q,%q}", i,
				got.Files[i].Name, got.Files[i].Data, orig.Files[i].Name, orig.Files[i].Data)
		}
	}
	if got.Etag != orig.Etag {
		t.Errorf("Etag = %q, want %q", got.Etag, orig.Etag)
	}
	// bundle.json bytes writeBundle would emit must match exactly.
	wantMF, _ := json.MarshalIndent(orig.Manifest, "", "  ")
	gotMF, _ := json.MarshalIndent(got.Manifest, "", "  ")
	if !bytes.Equal(wantMF, gotMF) {
		t.Errorf("manifest bytes drifted:\n got: %s\nwant: %s", gotMF, wantMF)
	}
}

func TestPutBundleSkipsWhenImportsPresent(t *testing.T) {
	c := tempCache(t)
	b := &bundle.Bundle{
		Files:   []bundle.File{{Name: "controls.yaml", Data: []byte("id: acme\n")}},
		Imports: []bundle.File{{Name: "dep.yaml", Data: []byte("id: dep\n")}},
	}
	var out bytes.Buffer
	putBundle(c, "hub.grc.store", "acme", "x", "1.0.0", b, &out)

	if _, found, _ := c.Get("hub.grc.store", "acme", "x", "1.0.0"); found {
		t.Error("bundle with imports should not have been cached (would drop imports on hit)")
	}
	if !bytes.Contains(out.Bytes(), []byte("carries imports")) {
		t.Errorf("expected an imports-not-cached notice, got %q", out.String())
	}
}

// TestResolveBundle_CacheHitSkipsNetwork proves the core Phase 2 property: a
// primary-artifact cache hit is served with NO network. The --url points at a
// non-resolvable host with no server, so if resolveBundle attempted hub
// discovery or a registry pull the command would error — a passing run proves
// the cached bytes were served offline.
func TestResolveBundle_CacheHitSkipsNetwork(t *testing.T) {
	c := tempCache(t) // sets GRCLI_CACHE for both this Put and the in-process command
	workdir := isolatedWorkdir(t)
	const url = "https://hub.invalid.test" // no server; must never be contacted

	// Seed the cache at exactly the coordinate resolveBundle will compute.
	seed := &bundle.Bundle{
		Files:    []bundle.File{{Name: "controls.yaml", Data: []byte("id: from-cache\n")}},
		Manifest: bundle.Manifest{BundleVersion: "1", GemaraVersion: "0.5.0"},
		Etag:     "sha256:abc",
	}
	putBundle(c, hostOf(url), "acme", "controls", "1.0.0", seed, io.Discard)

	unpacked := filepath.Join(workdir, "unpacked")
	// --no-verify: this test isolates the cache layer's offline property. Since
	// ADR-0048 a default unpack verifies, which DOES contact the hub/registry —
	// so "cache hit needs no network" now holds only when verification is off.
	out := runRoot(t, "unpack", "--url", url, "--repository", "acme/controls",
		"--version", "1.0.0", "--no-verify", "--output", unpacked)
	// The label is the hub coordinate — the SAME label a registry miss prints —
	// so repeated runs read identically whether served from cache or network.
	require.Contains(t, out, "unpacked hub.invalid.test/acme/controls:1.0.0")

	got, err := os.ReadFile(filepath.Join(unpacked, "controls.yaml"))
	require.NoError(t, err)
	require.Equal(t, "id: from-cache\n", string(got), "content must come from the cache, not the network")
	// The cached manifest is materialized too.
	require.FileExists(t, filepath.Join(unpacked, "bundle.json"))
}

// TestResolveBundle_NoCacheBypassesHit confirms --no-cache ignores a warm cache
// entry: with no server reachable, the fresh-pull path must fail (proving the
// cache was skipped rather than served).
func TestResolveBundle_NoCacheBypassesHit(t *testing.T) {
	c := tempCache(t)
	workdir := isolatedWorkdir(t)
	const url = "https://hub.invalid.test"
	seed := &bundle.Bundle{Files: []bundle.File{{Name: "controls.yaml", Data: []byte("id: from-cache\n")}}}
	putBundle(c, hostOf(url), "acme", "controls", "1.0.0", seed, io.Discard)

	_, err := runRootExpectErr(t, "unpack", "--url", url, "--repository", "acme/controls",
		"--version", "1.0.0", "--no-cache", "--output", filepath.Join(workdir, "unpacked"))
	require.Error(t, err, "--no-cache must bypass the warm entry and attempt (and fail) a network fetch")
}

func TestBundleFromEntryNoManifest(t *testing.T) {
	e := &cache.Entry{Files: []cache.File{{Name: "body.json", Data: []byte(`{"a":1}`)}}}
	b, err := bundleFromEntry(e)
	if err != nil {
		t.Fatalf("bundleFromEntry: %v", err)
	}
	if !b.Manifest.Empty() {
		t.Errorf("manifest should be empty, got %+v", b.Manifest)
	}
	if len(b.Files) != 1 || string(b.Files[0].Data) != `{"a":1}` {
		t.Errorf("files = %+v", b.Files)
	}
}

func TestBundleFromEntryRejectsBadManifest(t *testing.T) {
	e := &cache.Entry{
		Files:    []cache.File{{Name: "controls.yaml", Data: []byte("ok")}},
		Manifest: []byte("{not json"),
	}
	if _, err := bundleFromEntry(e); err == nil {
		t.Error("expected error decoding a corrupt cached manifest")
	}
}

func TestCachingEnabled(t *testing.T) {
	v := viper.New()
	v.SetDefault(flagCacheEnabled, true)
	v.SetDefault(flagNoCache, false)
	if !cachingEnabled(v) {
		t.Error("caching should be enabled by default")
	}
	v.Set(flagNoCache, true)
	if cachingEnabled(v) {
		t.Error("--no-cache should disable caching")
	}
	// cache-enabled:false disables caching even without --no-cache.
	v.Set(flagNoCache, false)
	v.Set(flagCacheEnabled, false)
	if cachingEnabled(v) {
		t.Error("cache-enabled:false should disable caching")
	}
}
