// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"os"
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T) *Cache {
	t.Helper()
	t.Setenv("GRCLI_CACHE", t.TempDir())
	c, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return c
}

func TestPutGetRoundtrip(t *testing.T) {
	c := openTemp(t)
	in := Entry{
		Files: []File{
			{Name: "controls.yaml", Data: []byte("id: acme\n")},
			{Name: "mappings.yaml", Data: []byte("maps: []\n")},
		},
		Manifest:       []byte(`{"schemaVersion":2}`),
		License:        "Apache-2.0",
		LicenseChecked: true,
		ManifestDigest: "sha256:abc",
		SourceURL:      "https://grc.store/acme/x",
	}
	if err := c.Put("hub.grc.store", "acme", "x", "1.0.0", in); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, found, err := c.Get("hub.grc.store", "acme", "x", "1.0.0")
	if err != nil || !found {
		t.Fatalf("Get: found=%v err=%v", found, err)
	}
	if len(got.Files) != 2 {
		t.Fatalf("got %d files, want 2", len(got.Files))
	}
	for i, want := range in.Files {
		if got.Files[i].Name != want.Name || string(got.Files[i].Data) != string(want.Data) {
			t.Errorf("file %d = %+v, want %+v", i, got.Files[i], want)
		}
	}
	if string(got.Manifest) != string(in.Manifest) {
		t.Errorf("manifest = %q, want %q", got.Manifest, in.Manifest)
	}
	if got.License != "Apache-2.0" || got.ManifestDigest != "sha256:abc" || got.SourceURL != in.SourceURL {
		t.Errorf("metadata not round-tripped: %+v", got)
	}
	if !got.LicenseChecked {
		t.Error("LicenseChecked not round-tripped")
	}
	if got.Verified {
		t.Error("Verified should default to false")
	}
}

func TestPutGetNoManifest(t *testing.T) {
	c := openTemp(t)
	in := Entry{Files: []File{{Name: "body.json", Data: []byte(`{"a":1}`)}}}
	if err := c.Put("hub.grc.store", "acme", "x", "1.0.0", in); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, found, err := c.Get("hub.grc.store", "acme", "x", "1.0.0")
	if err != nil || !found {
		t.Fatalf("Get: found=%v err=%v", found, err)
	}
	if got.Manifest != nil {
		t.Errorf("Manifest = %q, want nil", got.Manifest)
	}
	if len(got.Files) != 1 || string(got.Files[0].Data) != `{"a":1}` {
		t.Errorf("files = %+v", got.Files)
	}
}

func TestGetMissing(t *testing.T) {
	c := openTemp(t)
	_, found, err := c.Get("hub.grc.store", "acme", "absent", "1.0.0")
	if found || err != nil {
		t.Fatalf("Get on missing: found=%v err=%v, want false/nil", found, err)
	}
}

func TestGetDetectsFileCorruption(t *testing.T) {
	c := openTemp(t)
	if err := c.Put("hub.grc.store", "acme", "x", "1.0.0", Entry{Files: []File{{Name: "controls.yaml", Data: []byte("original")}}}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Tamper with the stored file so its digest no longer matches meta.json.
	blob := filepath.Join(c.entryDir("hub.grc.store", "acme", "x", "1.0.0"), "files", "0")
	if err := os.WriteFile(blob, []byte("tampered"), 0o644); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	_, found, err := c.Get("hub.grc.store", "acme", "x", "1.0.0")
	if found {
		t.Error("corrupt entry should not be reported as found")
	}
	if err == nil {
		t.Error("corrupt entry should return an error so the caller re-fetches")
	}
}

func TestGetDetectsCorruptionAtNonZeroIndex(t *testing.T) {
	c := openTemp(t)
	in := Entry{Files: []File{
		{Name: "a.yaml", Data: []byte("first")},
		{Name: "b.yaml", Data: []byte("second")},
	}}
	if err := c.Put("hub.grc.store", "acme", "x", "1.0.0", in); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Tamper the SECOND file — corruption must be caught regardless of index.
	blob := filepath.Join(c.entryDir("hub.grc.store", "acme", "x", "1.0.0"), "files", "1")
	if err := os.WriteFile(blob, []byte("tampered"), 0o644); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	_, found, err := c.Get("hub.grc.store", "acme", "x", "1.0.0")
	if found || err == nil {
		t.Errorf("corrupt file at index 1: found=%v err=%v, want false/non-nil", found, err)
	}
}

// TestV1EntriesAreIgnored guards the "no migration" decision (ADR-0042 dec. 3):
// a v1-shaped entry on disk must not be read at the v2 coordinate. This is the
// invariant the whole layoutVersion bump rests on.
func TestV1EntriesAreIgnored(t *testing.T) {
	c := openTemp(t)
	// Hand-build a v1 entry (single body.json + flat meta.json) at the v1 path.
	v1dir := filepath.Join(c.Root(), "v1", "hub.grc.store", "acme", "x", "1.0.0")
	if err := os.MkdirAll(v1dir, 0o755); err != nil {
		t.Fatalf("mkdir v1: %v", err)
	}
	if err := os.WriteFile(filepath.Join(v1dir, "body.json"), []byte(`{"legacy":true}`), 0o644); err != nil {
		t.Fatalf("write v1 body: %v", err)
	}
	if err := os.WriteFile(filepath.Join(v1dir, "meta.json"), []byte(`{"ext":"json"}`), 0o644); err != nil {
		t.Fatalf("write v1 meta: %v", err)
	}
	// The v2 Get must treat this coordinate as absent (clean miss, no error).
	_, found, err := c.Get("hub.grc.store", "acme", "x", "1.0.0")
	if found || err != nil {
		t.Fatalf("v1 entry leaked into v2 read: found=%v err=%v, want false/nil", found, err)
	}
}

func TestGetDetectsManifestCorruption(t *testing.T) {
	c := openTemp(t)
	in := Entry{Files: []File{{Name: "controls.yaml", Data: []byte("ok")}}, Manifest: []byte(`{"schemaVersion":2}`)}
	if err := c.Put("hub.grc.store", "acme", "x", "1.0.0", in); err != nil {
		t.Fatalf("Put: %v", err)
	}
	mf := filepath.Join(c.entryDir("hub.grc.store", "acme", "x", "1.0.0"), manifestFile)
	if err := os.WriteFile(mf, []byte(`{"tampered":true}`), 0o644); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	_, found, err := c.Get("hub.grc.store", "acme", "x", "1.0.0")
	if found || err == nil {
		t.Errorf("corrupt manifest: found=%v err=%v, want false/non-nil", found, err)
	}
}

func TestGetDetectsMissingBlob(t *testing.T) {
	c := openTemp(t)
	if err := c.Put("hub.grc.store", "acme", "x", "1.0.0", Entry{Files: []File{{Name: "controls.yaml", Data: []byte("ok")}}}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	blob := filepath.Join(c.entryDir("hub.grc.store", "acme", "x", "1.0.0"), "files", "0")
	if err := os.Remove(blob); err != nil {
		t.Fatalf("remove: %v", err)
	}
	_, found, err := c.Get("hub.grc.store", "acme", "x", "1.0.0")
	if found || err == nil {
		t.Errorf("missing blob: found=%v err=%v, want false/non-nil", found, err)
	}
}

func TestHostNamespacingPreventsCollision(t *testing.T) {
	c := openTemp(t)
	if err := c.Put("hub.grc.store", "acme", "x", "1.0.0", Entry{Files: []File{{Name: "b", Data: []byte("prod")}}}); err != nil {
		t.Fatalf("Put prod: %v", err)
	}
	if err := c.Put("hub.preview.grc.store", "acme", "x", "1.0.0", Entry{Files: []File{{Name: "b", Data: []byte("staging")}}}); err != nil {
		t.Fatalf("Put staging: %v", err)
	}
	prod, _, _ := c.Get("hub.grc.store", "acme", "x", "1.0.0")
	staging, _, _ := c.Get("hub.preview.grc.store", "acme", "x", "1.0.0")
	if string(prod.Files[0].Data) != "prod" || string(staging.Files[0].Data) != "staging" {
		t.Errorf("hosts collided: prod=%q staging=%q", prod.Files[0].Data, staging.Files[0].Data)
	}
}

func TestSanitizeBlocksTraversal(t *testing.T) {
	c := openTemp(t)
	// A hostile version component must not escape the cache root.
	if err := c.Put("hub.grc.store", "acme", "x", "../../etc", Entry{Files: []File{{Name: "b", Data: []byte("x")}}}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	dir := c.entryDir("hub.grc.store", "acme", "x", "../../etc")
	rel, err := filepath.Rel(c.Root(), dir)
	if err != nil {
		t.Fatalf("Rel: %v", err)
	}
	if filepath.IsAbs(rel) || rel == ".." || len(rel) >= 2 && rel[0] == '.' && rel[1] == '.' {
		t.Errorf("entry dir %q escaped cache root %q (rel %q)", dir, c.Root(), rel)
	}
}
