// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gemaraproj/go-gemara/bundle"
	"github.com/stretchr/testify/require"

	"github.com/gemaraproj/grcli/internal/cache"
)

// refBearingCatalogYAML is a ControlCatalog whose `imports` resolves reference
// "base" -> https://grc.store/acme/baseline@2.1.0 (the grc.store placeholder
// host rewrites to the --url target).
const refBearingCatalogYAML = `metadata:
  id: my-catalog
  type: ControlCatalog
  gemara-version: "0.5.0"
  description: a test catalog
  author:
    id: acme
    name: Acme
  mapping-references:
    - id: base
      title: Base Catalog
      version: "2.1.0"
      url: https://grc.store/acme/baseline
imports:
  - reference-id: base
`

// fakeDiscoveryHub serves the well-known discovery doc (advertising registryURL)
// and answers the primary's best-effort license lookup with a quiet 404.
// Discovery is now lazy — only a cache MISS triggers it — so an all-cache-hit
// run never calls it, and registryURL is only reached on an actual pull.
func fakeDiscoveryHub(t *testing.T, registryURL string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/.well-known/grc-store-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{"registry_url": registryURL})
		case strings.HasPrefix(r.URL.Path, "/v1/catalogs/"):
			// The primary's license-baseline lookup (best-effort); a 404 just
			// means "no baseline", which suppresses mismatch warnings.
			http.Error(w, "not found", http.StatusNotFound)
		default:
			t.Errorf("unexpected hub path: %s", r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestUnpack_References_FromCache exercises the whole reference-resolution path
// offline: both the primary and its imported reference are pre-seeded in the
// cache, so unpack --with-imports resolves the reference with no registry pull,
// writing it as a directory (files + bundle.json) plus references/index.json.
func TestUnpack_References_FromCache(t *testing.T) {
	c := tempCache(t)
	workdir := isolatedWorkdir(t)
	srv := fakeDiscoveryHub(t, "https://oci.invalid.test") // never pulled from on a hit
	host := hostOf(srv.URL)

	// Primary: a cache hit whose content declares the import.
	primary := &bundle.Bundle{
		Files:    []bundle.File{{Name: "catalog.yaml", Data: []byte(refBearingCatalogYAML)}},
		Manifest: bundle.Manifest{BundleVersion: "1", GemaraVersion: "0.5.0"},
	}
	putBundle(c, host, "myorg", "mycat", "1.0.0", primary, io.Discard)

	// Reference (acme/baseline@2.1.0): also a cache hit — a full bundle.
	ref := &bundle.Bundle{
		Files:    []bundle.File{{Name: "baseline.yaml", Data: []byte("id: baseline\n")}},
		Manifest: bundle.Manifest{BundleVersion: "1", GemaraVersion: "0.5.0"},
		Etag:     "sha256:refetag",
	}
	refEntry, err := entryFromBundle(ref, "Apache-2.0", "https://grc.store/acme/baseline")
	require.NoError(t, err)
	require.NoError(t, c.Put(host, "acme", "baseline", "2.1.0", refEntry))

	output := filepath.Join(workdir, "unpacked")
	out := runRoot(t, "unpack", "--url", srv.URL, "--repository", "myorg/mycat",
		"--version", "1.0.0", "--with-imports", "--no-verify", "--output", output)
	require.Contains(t, out, "resolved 1 reference(s), skipped 0")

	refDir := filepath.Join(output, "references", "imports", "acme", "baseline@2.1.0")
	gotFile, err := os.ReadFile(filepath.Join(refDir, "baseline.yaml"))
	require.NoError(t, err)
	require.Equal(t, "id: baseline\n", string(gotFile), "reference file must be materialized from cache")
	require.FileExists(t, filepath.Join(refDir, "bundle.json"), "reference bundle.json must be written")

	// index.json records the reference with the directory path.
	idxBytes, err := os.ReadFile(filepath.Join(output, "references", "index.json"))
	require.NoError(t, err)
	var idx []refIndexEntry
	require.NoError(t, json.Unmarshal(idxBytes, &idx))
	require.Len(t, idx, 1)
	require.Equal(t, "baseline", idx[0].CatalogID)
	require.Equal(t, "acme", idx[0].Namespace)
	require.Equal(t, "2.1.0", idx[0].Version)
	require.Equal(t, "Apache-2.0", idx[0].License)
	require.Equal(t, filepath.Join("references", "imports", "acme", "baseline@2.1.0"), idx[0].Path)
	require.Equal(t, "sha256:refetag", idx[0].ManifestDigest)
}

// TestUnpack_References_LicenseHealedOnCacheHit guards the license-heal path: a
// coordinate cached WITHOUT a license (e.g. first fetched as a primary, or
// cached during a hub outage) must have its license looked up live on a later
// reference cache hit — filling references/index.json and upgrading the cache
// entry in place — instead of staying license-less forever.
func TestUnpack_References_LicenseHealedOnCacheHit(t *testing.T) {
	c := tempCache(t)
	workdir := isolatedWorkdir(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/.well-known/grc-store-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{"registry_url": "https://oci.invalid.test"})
		case r.URL.Path == "/v1/catalogs/acme/baseline":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"namespace": "acme", "catalog_id": "baseline",
				"releases": []map[string]string{{"version": "2.1.0", "license": "GPL-3.0-only"}},
			})
		case strings.HasPrefix(r.URL.Path, "/v1/catalogs/"):
			http.Error(w, "not found", http.StatusNotFound) // primary license baseline: best-effort
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	host := hostOf(srv.URL)

	primary := &bundle.Bundle{Files: []bundle.File{{Name: "catalog.yaml", Data: []byte(refBearingCatalogYAML)}}}
	putBundle(c, host, "myorg", "mycat", "1.0.0", primary, io.Discard)
	// The reference is cached with NO license — the poisoned/outage shape.
	ref := &bundle.Bundle{Files: []bundle.File{{Name: "baseline.yaml", Data: []byte("id: baseline\n")}}}
	refEntry, err := entryFromBundle(ref, "", "https://grc.store/acme/baseline")
	require.NoError(t, err)
	require.NoError(t, c.Put(host, "acme", "baseline", "2.1.0", refEntry))

	output := filepath.Join(workdir, "unpacked")
	out := runRoot(t, "unpack", "--url", srv.URL, "--repository", "myorg/mycat",
		"--version", "1.0.0", "--with-imports", "--no-verify", "--output", output)
	require.Contains(t, out, "resolved 1 reference(s), skipped 0")

	// The healed license lands in index.json...
	idxBytes, err := os.ReadFile(filepath.Join(output, "references", "index.json"))
	require.NoError(t, err)
	var idx []refIndexEntry
	require.NoError(t, json.Unmarshal(idxBytes, &idx))
	require.Len(t, idx, 1)
	require.Equal(t, "GPL-3.0-only", idx[0].License, "empty-license cache hit must be healed from the hub")

	// ...and the cache entry is upgraded in place, so the next hit has it.
	healed, found, err := c.Get(host, "acme", "baseline", "2.1.0")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "GPL-3.0-only", healed.License)
}

// TestUnpack_References_LicenseHealMemoized guards the heal's cost bound: when
// the hub CONFIRMS a coordinate has no license, the entry is marked checked and
// no later hit pays another hub call — the catalog endpoint must be hit exactly
// once across two runs.
func TestUnpack_References_LicenseHealMemoized(t *testing.T) {
	c := tempCache(t)
	workdir := isolatedWorkdir(t)
	refCatalogCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/.well-known/grc-store-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{"registry_url": "https://oci.invalid.test"})
		case r.URL.Path == "/v1/catalogs/acme/baseline":
			refCatalogCalls++
			// Lookup SUCCEEDS but the catalog genuinely records no license.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"namespace": "acme", "catalog_id": "baseline",
				"releases": []map[string]string{{"version": "2.1.0"}},
			})
		case strings.HasPrefix(r.URL.Path, "/v1/catalogs/"):
			http.Error(w, "not found", http.StatusNotFound)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	host := hostOf(srv.URL)

	primary := &bundle.Bundle{Files: []bundle.File{{Name: "catalog.yaml", Data: []byte(refBearingCatalogYAML)}}}
	putBundle(c, host, "myorg", "mycat", "1.0.0", primary, io.Discard)
	ref := &bundle.Bundle{Files: []bundle.File{{Name: "baseline.yaml", Data: []byte("id: baseline\n")}}}
	refEntry, err := entryFromBundle(ref, "", "https://grc.store/acme/baseline")
	require.NoError(t, err)
	require.NoError(t, c.Put(host, "acme", "baseline", "2.1.0", refEntry))

	for run := 1; run <= 2; run++ {
		out := runRoot(t, "unpack", "--url", srv.URL, "--repository", "myorg/mycat",
			"--version", "1.0.0", "--with-imports", "--no-verify", "--output", filepath.Join(workdir, fmt.Sprintf("out%d", run)))
		require.Contains(t, out, "resolved 1 reference(s), skipped 0")
	}
	require.Equal(t, 1, refCatalogCalls,
		"a confirmed license-less coordinate must be looked up exactly once, not per run")

	healed, found, err := c.Get(host, "acme", "baseline", "2.1.0")
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, healed.LicenseChecked, "the successful no-license lookup must be memoized")
	require.Empty(t, healed.License)
}

// TestWriteReference_NoPartialWriteOnRejection: a hostile name anywhere in the
// list must reject the reference BEFORE any file is written, leaving no
// orphaned content on disk.
func TestWriteReference_NoPartialWriteOnRejection(t *testing.T) {
	dir := t.TempDir()
	refDir := filepath.Join("references", "imports", "ns", "id@1.0.0")
	e := &cache.Entry{Files: []cache.File{
		{Name: "good.yaml", Data: []byte("id: good\n")},
		{Name: "../../../../evil.yaml", Data: []byte("tampered\n")},
	}}
	err := writeReference(dir, refDir, e, io.Discard)
	require.Error(t, err)
	require.NoFileExists(t, filepath.Join(dir, refDir, "good.yaml"),
		"names are validated before anything is written — no orphaned partial output")
	require.NoFileExists(t, filepath.Join(dir, "evil.yaml"))
}

// TestUnpack_References_OfflineWhenDiscoveryDown guards the lazy-discovery fix:
// with the hub's discovery endpoint failing (503) but the primary and its
// reference both cached, unpack must still resolve the reference — discovery is
// only needed for a registry pull, which a cache hit never performs. (The
// best-effort license heal for the unchecked cached entry DOES attempt a hub
// lookup here and fails fast with a diagnostic; that must not block anything.)
func TestUnpack_References_OfflineWhenDiscoveryDown(t *testing.T) {
	c := tempCache(t)
	workdir := isolatedWorkdir(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/catalogs/") {
			http.Error(w, "not found", http.StatusNotFound) // primary license baseline: best-effort
			return
		}
		http.Error(w, "discovery down", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	host := hostOf(srv.URL)

	primary := &bundle.Bundle{Files: []bundle.File{{Name: "catalog.yaml", Data: []byte(refBearingCatalogYAML)}}}
	putBundle(c, host, "myorg", "mycat", "1.0.0", primary, io.Discard)
	ref := &bundle.Bundle{Files: []bundle.File{{Name: "baseline.yaml", Data: []byte("id: baseline\n")}}, Etag: "sha256:ref"}
	refEntry, err := entryFromBundle(ref, "", "https://grc.store/acme/baseline")
	require.NoError(t, err)
	require.NoError(t, c.Put(host, "acme", "baseline", "2.1.0", refEntry))

	output := filepath.Join(workdir, "unpacked")
	out := runRoot(t, "unpack", "--url", srv.URL, "--repository", "myorg/mycat",
		"--version", "1.0.0", "--with-imports", "--no-verify", "--output", output)
	require.Contains(t, out, "resolved 1 reference(s), skipped 0",
		"a cached reference must resolve even when discovery is unreachable")
	require.FileExists(t, filepath.Join(output, "references", "imports", "acme", "baseline@2.1.0", "baseline.yaml"))
}

func TestWriteReference(t *testing.T) {
	dir := t.TempDir()
	e := &cache.Entry{
		Files:    []cache.File{{Name: "a.yaml", Data: []byte("id: a\n")}, {Name: "b.yaml", Data: []byte("id: b\n")}},
		Manifest: []byte(`{"bundle-version":"1"}`),
	}
	require.NoError(t, writeReference(dir, filepath.Join("references", "imports", "ns", "id@1.0.0"), e, io.Discard))
	base := filepath.Join(dir, "references", "imports", "ns", "id@1.0.0")
	for name, want := range map[string]string{"a.yaml": "id: a\n", "b.yaml": "id: b\n", "bundle.json": `{"bundle-version":"1"}`} {
		got, err := os.ReadFile(filepath.Join(base, name))
		require.NoError(t, err, name)
		require.Equal(t, want, string(got), name)
	}
}

func TestReferenceContentDigest(t *testing.T) {
	manifest := []byte(`{"bundle-version":"1"}`)
	withManifest := &cache.Entry{Files: []cache.File{{Name: "a", Data: []byte("x")}}, Manifest: manifest}
	require.Equal(t, cache.Digest(manifest), referenceContentDigest(withManifest),
		"manifest digest identifies the whole bundle")

	single := &cache.Entry{Files: []cache.File{{Name: "a", Data: []byte("only")}}}
	require.Equal(t, cache.Digest([]byte("only")), referenceContentDigest(single))

	// Multi-file, no manifest: a deterministic combined digest, never "".
	multiNoManifest := &cache.Entry{Files: []cache.File{{Name: "a", Data: []byte("x")}, {Name: "b", Data: []byte("y")}}}
	d := referenceContentDigest(multiNoManifest)
	require.NotEmpty(t, d, "index must always carry a content digest")
	require.Equal(t, d, referenceContentDigest(multiNoManifest), "digest must be deterministic")
	reordered := &cache.Entry{Files: []cache.File{{Name: "b", Data: []byte("y")}, {Name: "a", Data: []byte("x")}}}
	require.NotEqual(t, d, referenceContentDigest(reordered), "digest is over the ordered file list")

	require.Equal(t, "", referenceContentDigest(&cache.Entry{}), "no files, no manifest: nothing to digest")
}

func TestRefRelPath(t *testing.T) {
	refDir := filepath.Join("references", "imports", "acme", "baseline@2.1.0")

	good, err := refRelPath(refDir, "controls.yaml")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(refDir, "controls.yaml"), good)

	// Nested names are fine as long as they stay inside refDir.
	nested, err := refRelPath(refDir, filepath.Join("sub", "extra.yaml"))
	require.NoError(t, err)
	require.Equal(t, filepath.Join(refDir, "sub", "extra.yaml"), nested)

	for _, hostile := range []string{
		"",
		"..",
		"../../../../controls.yaml", // climbs out to the output root
		"../sibling.yaml",           // climbs into another reference's dir
		filepath.Join("sub", "..", "..", "escape.yaml"),
		".",
	} {
		_, err := refRelPath(refDir, hostile)
		require.Error(t, err, "name %q must be rejected", hostile)
	}
}

// TestWriteReference_RejectsTraversal is the end-to-end guard for the
// remote-controlled-name traversal: a reference bundle whose file name climbs
// out of its own directory must be rejected, and nothing outside refDir
// written.
func TestWriteReference_RejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	e := &cache.Entry{Files: []cache.File{
		{Name: "../../../../controls.yaml", Data: []byte("tampered\n")},
	}}
	err := writeReference(dir, filepath.Join("references", "imports", "ns", "id@1.0.0"), e, io.Discard)
	require.Error(t, err)
	require.NoFileExists(t, filepath.Join(dir, "controls.yaml"),
		"the traversal target must not have been written")
}

// TestEntryFromBundle_DropsImports documents that the v2 cache entry stores
// Files + manifest only — a referenced bundle's own transitive imports are not
// represented (reference resolution is direct-only). fetchReference
// warns via noteDroppedReferenceImports rather than dropping them silently.
func TestEntryFromBundle_DropsImports(t *testing.T) {
	b := &bundle.Bundle{
		Files:   []bundle.File{{Name: "controls.yaml", Data: []byte("id: a\n")}},
		Imports: []bundle.File{{Name: "dep.yaml", Data: []byte("id: dep\n")}},
	}
	e, err := entryFromBundle(b, "", "")
	require.NoError(t, err)
	require.Len(t, e.Files, 1, "only the artifact files are stored")
	require.Equal(t, "controls.yaml", e.Files[0].Name)
}

func TestNoteDroppedReferenceImports(t *testing.T) {
	var buf strings.Builder
	noteDroppedReferenceImports(&buf, "acme", "baseline", "2.1.0", 2)
	require.Contains(t, buf.String(), "acme/baseline@2.1.0")
	require.Contains(t, buf.String(), "2 transitive import")
	require.Contains(t, buf.String(), "not materialized")
}

func TestUserSuppliedRegistryCredential(t *testing.T) {
	t.Setenv("GRCLI_REGISTRY_TOKEN", "")
	t.Setenv("GRCLI_REGISTRY_USERNAME", "")
	t.Setenv("GRCLI_REGISTRY_PASSWORD", "")
	require.False(t, userSuppliedRegistryCredential())

	t.Setenv("GRCLI_REGISTRY_TOKEN", "tok")
	require.True(t, userSuppliedRegistryCredential())

	t.Setenv("GRCLI_REGISTRY_TOKEN", "")
	t.Setenv("GRCLI_REGISTRY_USERNAME", "u")
	require.False(t, userSuppliedRegistryCredential(), "username alone is not a complete basic-auth credential")
	t.Setenv("GRCLI_REGISTRY_PASSWORD", "p")
	require.True(t, userSuppliedRegistryCredential())
}

func TestMintRefPullToken(t *testing.T) {
	// userCreds=true: the user's credential wins; we must not mint or touch env.
	t.Setenv("GRCLI_REGISTRY_TOKEN", "user-token")
	require.NoError(t, mintRefPullToken(context.Background(), "https://hub.invalid.test", "ns/id", true))
	require.Equal(t, "user-token", os.Getenv("GRCLI_REGISTRY_TOKEN"), "user credential must be left untouched")

	// userCreds=false: mint a fresh per-repo token from the hub and export it,
	// even though GRCLI_REGISTRY_TOKEN is already set to a different repo's token.
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v2/token", r.URL.Path)
		require.Contains(t, r.URL.RawQuery, "repository%3Ans%2Fid%3Apull", "scope must target the reference repo")
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "fresh-ref-token"})
	}))
	t.Cleanup(tokenSrv.Close)
	t.Setenv("GRCLI_REGISTRY_TOKEN", "stale-primary-token")
	require.NoError(t, mintRefPullToken(context.Background(), tokenSrv.URL, "ns/id", false))
	require.Equal(t, "fresh-ref-token", os.Getenv("GRCLI_REGISTRY_TOKEN"),
		"a fresh per-repo token must replace the stale one")
}
