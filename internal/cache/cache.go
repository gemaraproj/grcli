// SPDX-License-Identifier: LicenseRef-Revanite-Proprietary

// Package cache is a Go-module-style on-disk cache for artifacts grcli pulls
// when resolving references (ADR-0039). grc.store tags are immutable
// (ADR-0033), so a coordinate (host, namespace, id, version) maps to fixed
// bytes forever — a cache hit can never be stale, which is what makes this
// sound. Entries are host-namespaced so prod, staging, and self-hosted hubs
// never collide. There is intentionally no eviction in this version; the cache
// grows like Go's module cache.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// layoutVersion namespaces the on-disk layout so a future format change can
// coexist with old entries instead of misreading them.
const layoutVersion = "v1"

// Entry is a cached artifact plus the metadata recorded about it. Body is held
// separately from the persisted meta.json (it is its own file on disk).
type Entry struct {
	Body []byte `json:"-"`

	// Digest is the sha256 of Body, recomputed and checked on every read as a
	// corruption guard.
	Digest string `json:"digest"`
	// ManifestDigest is the artifact's OCI manifest digest (its identity on
	// the hub), recorded for provenance.
	ManifestDigest string `json:"manifest_digest,omitempty"`
	// License is the dependency's own publication license (canonical SPDX).
	License string `json:"license,omitempty"`
	// SourceURL is the reference URL this entry was resolved from.
	SourceURL string `json:"source_url,omitempty"`
	// Ext is the body file extension (without the dot), e.g. "json".
	Ext string `json:"ext"`
	// Verified records whether the bytes were signature-verified. Always false
	// in the first cut — verify-on-pull is deferred (ADR-0039 amendment) — but
	// persisted so a later pass can upgrade entries in place.
	Verified bool `json:"verified"`
}

// Cache is a handle to an on-disk cache rooted at a directory.
type Cache struct {
	root string
}

// Open returns a Cache rooted at $GRCLI_CACHE if set, else
// os.UserCacheDir()/grcli. The directory is created lazily on Put.
func Open() (*Cache, error) {
	root := os.Getenv("GRCLI_CACHE")
	if root == "" {
		ucd, err := os.UserCacheDir()
		if err != nil {
			return nil, fmt.Errorf("resolving user cache dir (set $GRCLI_CACHE to override): %w", err)
		}
		root = filepath.Join(ucd, "grcli")
	}
	return &Cache{root: root}, nil
}

// Root is the cache's base directory (for diagnostics).
func (c *Cache) Root() string { return c.root }

// entryDir is the directory holding one coordinate's files. Components are
// sanitized so a hostile coordinate can't escape the cache root.
func (c *Cache) entryDir(host, namespace, id, version string) string {
	return filepath.Join(c.root, layoutVersion,
		sanitize(host), sanitize(namespace), sanitize(id), sanitize(version))
}

// Get returns the cached entry for a coordinate. found is false when the entry
// is absent. A present-but-corrupt entry (body digest mismatch) returns
// found=false with a non-nil error so the caller can warn and re-fetch.
func (c *Cache) Get(host, namespace, id, version string) (entry *Entry, found bool, err error) {
	dir := c.entryDir(host, namespace, id, version)
	metaBytes, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("reading cache metadata: %w", err)
	}
	var e Entry
	if err := json.Unmarshal(metaBytes, &e); err != nil {
		return nil, false, fmt.Errorf("decoding cache metadata for %s/%s@%s: %w", namespace, id, version, err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "body."+bodyExt(e.Ext)))
	if err != nil {
		return nil, false, fmt.Errorf("reading cached body for %s/%s@%s: %w", namespace, id, version, err)
	}
	if got := digestOf(body); got != e.Digest {
		return nil, false, fmt.Errorf("cached body for %s/%s@%s is corrupt (digest %s != recorded %s)",
			namespace, id, version, got, e.Digest)
	}
	e.Body = body
	return &e, true, nil
}

// Put writes an entry to the cache, computing and recording the body digest.
func (c *Cache) Put(host, namespace, id, version string, e Entry) error {
	dir := c.entryDir(host, namespace, id, version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating cache dir: %w", err)
	}
	e.Digest = digestOf(e.Body)
	if err := os.WriteFile(filepath.Join(dir, "body."+bodyExt(e.Ext)), e.Body, 0o644); err != nil {
		return fmt.Errorf("writing cache body: %w", err)
	}
	metaBytes, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding cache metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), metaBytes, 0o644); err != nil {
		return fmt.Errorf("writing cache metadata: %w", err)
	}
	return nil
}

// Digest returns the sha256 content digest of b in "sha256:<hex>" form — the
// same value recorded on a cache Entry, exported so callers can record it for
// content that bypasses the cache (e.g. under --no-cache).
func Digest(b []byte) string { return digestOf(b) }

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func bodyExt(ext string) string {
	if ext == "" {
		return "json"
	}
	return ext
}

// sanitize reduces a coordinate component to a safe single path segment:
// path separators and parent-dir tokens can't survive, so the join stays
// within the cache root.
func sanitize(s string) string {
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	s = strings.ReplaceAll(s, "..", "_")
	s = strings.TrimSpace(s)
	if s == "" {
		return "_"
	}
	return s
}
