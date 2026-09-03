// SPDX-License-Identifier: Apache-2.0

// Package cache is a Go-module-style on-disk cache for artifacts grcli
// pulls. grc.store tags are immutable,
// so a coordinate (host, namespace, id, version) maps to fixed bytes forever —
// a cache hit can never be stale, which is what makes this sound. Entries are
// host-namespaced so prod, staging, and self-hosted hubs stay separate. (Caveat:
// coordinate components are sanitized to single path segments, so two coordinates
// that differ only in characters sanitize() folds together — e.g. "a/b" vs "a_b"
// — would alias the same entry. A collision-resistant encoding is a tracked
// follow-up; today's coordinates don't hit it.)
//
// An entry stores the complete decoded bundle: every artifact file plus the
// bundle.json manifest (when present), enough to serve both `unpack` (write the
// dir) and `cat` (stream the content) offline. It does not store the raw OCI
// layout or the cosign signature, so `verify` still fetches from the registry.
// There is intentionally no eviction in this version; the cache grows like Go's
// module cache.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// layoutVersion namespaces the on-disk layout so a format change can coexist
// with old entries instead of misreading them. v2 stores a full
// bundle; v1 entries (single body) are simply never read.
const layoutVersion = "v2"

// File is one artifact file in a cached bundle. Data is held separately from the
// persisted meta.json — each file is its own blob on disk.
type File struct {
	Name string
	Data []byte
}

// Entry is a cached bundle plus the metadata recorded about it.
type Entry struct {
	// Files are the bundle's artifact files (bundle.Files), in order.
	Files []File
	// Manifest is the bundle.json bytes (the JSON-encoded OCI manifest, with any
	// SLSA-shaped provenance). Nil for an entry that carries no manifest.
	Manifest []byte

	// ManifestDigest is the artifact's OCI manifest digest (its identity on the
	// hub — bundle.Etag), recorded for provenance.
	ManifestDigest string
	// License is the artifact's own publication license (canonical SPDX).
	License string
	// LicenseChecked records that a hub license lookup SUCCEEDED for this
	// entry (even if the catalog records no license). It distinguishes
	// "hub confirmed no license — stop asking" from "lookup failed or never
	// attempted — retry on a later hit", so a license-less coordinate is
	// healed at most once instead of paying a live hub call on every hit.
	LicenseChecked bool
	// SourceURL is the reference URL this entry was resolved from, if any.
	SourceURL string
	// Verified records whether the bytes were signature-verified. Always false
	// for now — verify-on-pull is deferred — but persisted
	// so a later pass can upgrade entries in place.
	Verified bool
}

// entryMeta is the persisted meta.json. File bodies and bundle.json live in
// their own files; meta.json records their names and digests.
type entryMeta struct {
	Files          []fileMeta `json:"files"`
	Manifest       *fileMeta  `json:"manifest,omitempty"`
	ManifestDigest string     `json:"manifest_digest,omitempty"`
	License        string     `json:"license,omitempty"`
	LicenseChecked bool       `json:"license_checked,omitempty"`
	SourceURL      string     `json:"source_url,omitempty"`
	Verified       bool       `json:"verified"`
}

// fileMeta records one stored blob's original name and content digest.
type fileMeta struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

// manifestFile is the fixed on-disk name for the cached bundle.json.
const manifestFile = "bundle.json"

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
// is absent. A present-but-corrupt entry (any blob's digest mismatches, or a
// recorded blob is missing) returns found=false with a non-nil error so the
// caller can warn and re-fetch.
func (c *Cache) Get(host, namespace, id, version string) (entry *Entry, found bool, err error) {
	dir := c.entryDir(host, namespace, id, version)
	metaBytes, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("reading cache metadata: %w", err)
	}
	var m entryMeta
	if err := json.Unmarshal(metaBytes, &m); err != nil {
		return nil, false, fmt.Errorf("decoding cache metadata for %s/%s@%s: %w", namespace, id, version, err)
	}

	e := &Entry{
		ManifestDigest: m.ManifestDigest,
		License:        m.License,
		LicenseChecked: m.LicenseChecked,
		SourceURL:      m.SourceURL,
		Verified:       m.Verified,
	}
	for i, fm := range m.Files {
		data, rerr := readBlob(dir, "files", strconv.Itoa(i), fm, namespace, id, version)
		if rerr != nil {
			return nil, false, rerr
		}
		e.Files = append(e.Files, File{Name: fm.Name, Data: data})
	}
	if m.Manifest != nil {
		data, rerr := readBlob(dir, "", manifestFile, *m.Manifest, namespace, id, version)
		if rerr != nil {
			return nil, false, rerr
		}
		e.Manifest = data
	}
	return e, true, nil
}

// readBlob reads dir/[sub/]name, verifying its digest against the recorded
// fileMeta. A missing or corrupt blob is an error so the caller re-fetches.
func readBlob(dir, sub, name string, fm fileMeta, namespace, id, version string) ([]byte, error) {
	path := filepath.Join(dir, sub, name)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading cached %s for %s/%s@%s: %w", fm.Name, namespace, id, version, err)
	}
	if got := digestOf(data); got != fm.Digest {
		return nil, fmt.Errorf("cached %s for %s/%s@%s is corrupt (digest %s != recorded %s)",
			fm.Name, namespace, id, version, got, fm.Digest)
	}
	return data, nil
}

// Put writes an entry to the cache, computing and recording each blob's digest.
// What makes a later Get safe is the per-blob digest check on read: a blob whose
// bytes don't match the digest recorded in meta.json is rejected as corrupt.
// meta.json is written last only so a half-written brand-new entry reads as a
// clean miss (no meta.json ⇒ found=false) rather than a partial hit.
func (c *Cache) Put(host, namespace, id, version string, e Entry) error {
	dir := c.entryDir(host, namespace, id, version)
	filesDir := filepath.Join(dir, "files")
	if err := os.MkdirAll(filesDir, 0o755); err != nil {
		return fmt.Errorf("creating cache dir: %w", err)
	}

	m := entryMeta{
		ManifestDigest: e.ManifestDigest,
		License:        e.License,
		LicenseChecked: e.LicenseChecked,
		SourceURL:      e.SourceURL,
		Verified:       e.Verified,
	}
	for i, f := range e.Files {
		if err := os.WriteFile(filepath.Join(filesDir, strconv.Itoa(i)), f.Data, 0o644); err != nil {
			return fmt.Errorf("writing cache file %q: %w", f.Name, err)
		}
		m.Files = append(m.Files, fileMeta{Name: f.Name, Digest: digestOf(f.Data)})
	}
	if e.Manifest != nil {
		if err := os.WriteFile(filepath.Join(dir, manifestFile), e.Manifest, 0o644); err != nil {
			return fmt.Errorf("writing cache manifest: %w", err)
		}
		m.Manifest = &fileMeta{Name: manifestFile, Digest: digestOf(e.Manifest)}
	}

	metaBytes, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding cache metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), metaBytes, 0o644); err != nil {
		return fmt.Errorf("writing cache metadata: %w", err)
	}
	return nil
}

// Digest returns the sha256 content digest of b in "sha256:<hex>" form — the
// same value recorded on a cache blob, exported so callers can record it for
// content that bypasses the cache (e.g. under --no-cache).
func Digest(b []byte) string { return digestOf(b) }

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
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
