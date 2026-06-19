// SPDX-License-Identifier: LicenseRef-Revanite-Proprietary

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
	body := []byte(`{"hello":"world"}`)
	in := Entry{Body: body, License: "Apache-2.0", ManifestDigest: "sha256:abc", SourceURL: "https://grc.store/acme/x", Ext: "json"}
	if err := c.Put("hub.grc.store", "acme", "x", "1.0.0", in); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, found, err := c.Get("hub.grc.store", "acme", "x", "1.0.0")
	if err != nil || !found {
		t.Fatalf("Get: found=%v err=%v", found, err)
	}
	if string(got.Body) != string(body) {
		t.Errorf("body = %q, want %q", got.Body, body)
	}
	if got.License != "Apache-2.0" || got.ManifestDigest != "sha256:abc" {
		t.Errorf("metadata not round-tripped: %+v", got)
	}
	if got.Digest != Digest(body) {
		t.Errorf("Digest = %q, want %q", got.Digest, Digest(body))
	}
	if got.Verified {
		t.Error("Verified should default to false")
	}
}

func TestGetMissing(t *testing.T) {
	c := openTemp(t)
	_, found, err := c.Get("hub.grc.store", "acme", "absent", "1.0.0")
	if found || err != nil {
		t.Fatalf("Get on missing: found=%v err=%v, want false/nil", found, err)
	}
}

func TestGetDetectsCorruption(t *testing.T) {
	c := openTemp(t)
	if err := c.Put("hub.grc.store", "acme", "x", "1.0.0", Entry{Body: []byte("original"), Ext: "json"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Tamper with the stored body so its digest no longer matches meta.json.
	bodyPath := filepath.Join(c.entryDir("hub.grc.store", "acme", "x", "1.0.0"), "body.json")
	if err := os.WriteFile(bodyPath, []byte("tampered"), 0o644); err != nil {
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

func TestHostNamespacingPreventsCollision(t *testing.T) {
	c := openTemp(t)
	if err := c.Put("hub.grc.store", "acme", "x", "1.0.0", Entry{Body: []byte("prod"), Ext: "json"}); err != nil {
		t.Fatalf("Put prod: %v", err)
	}
	if err := c.Put("hub.preview.grc.store", "acme", "x", "1.0.0", Entry{Body: []byte("staging"), Ext: "json"}); err != nil {
		t.Fatalf("Put staging: %v", err)
	}
	prod, _, _ := c.Get("hub.grc.store", "acme", "x", "1.0.0")
	staging, _, _ := c.Get("hub.preview.grc.store", "acme", "x", "1.0.0")
	if string(prod.Body) != "prod" || string(staging.Body) != "staging" {
		t.Errorf("hosts collided: prod=%q staging=%q", prod.Body, staging.Body)
	}
}

func TestSanitizeBlocksTraversal(t *testing.T) {
	c := openTemp(t)
	// A hostile version component must not escape the cache root.
	if err := c.Put("hub.grc.store", "acme", "x", "../../etc", Entry{Body: []byte("x"), Ext: "json"}); err != nil {
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
