// SPDX-License-Identifier: LicenseRef-Revanite-Proprietary

package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Credentials is one issuer's saved tokens. The Issuer field doubles as
// the storage key so callers don't have to track that separately.
type Credentials struct {
	Issuer       string    `json:"issuer"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// Store is grcli's on-disk credential cache. v1 is a single JSON file
// at `${XDG_DATA_HOME:-~/.local/share}/grcli/credentials.json` with one
// entry per issuer URL. The file is created with 0600 perms (the
// directory with 0700) — the data inside is functionally a long-lived
// password.
//
// Why a flat file instead of the OS keyring (Keychain on macOS,
// libsecret on Linux): native keyring bindings pull in platform-
// specific cgo dependencies that complicate cross-compilation, and
// grcli is still pre-built-binaries. A flat 0600 file is the same
// posture `gh`, `flyctl`, and `gcloud` ship with by default. A
// keyring backend can be added later behind the same Store interface
// without breaking callers.
type Store struct {
	Path string
}

// storeFile is the on-disk JSON shape. Versioned so a future migration
// can detect old layouts.
type storeFile struct {
	Version     int                     `json:"version"`
	Credentials map[string]*Credentials `json:"credentials"`
}

const currentStoreVersion = 1

// ErrNoCredentials is returned by Get when the store has no entry for
// the given issuer. Callers compare with errors.Is to drive "fall back
// to GRCLI_TOKEN / explicit flag" branches.
var ErrNoCredentials = errors.New("no stored credentials for this issuer")

// NewDefaultStore returns a Store rooted at the standard XDG path. The
// directory is created lazily on first write — Get on a missing file
// just returns ErrNoCredentials.
func NewDefaultStore() (*Store, error) {
	dir, err := defaultStoreDir()
	if err != nil {
		return nil, err
	}
	return &Store{Path: filepath.Join(dir, "credentials.json")}, nil
}

func defaultStoreDir() (string, error) {
	if xdg := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); xdg != "" {
		return filepath.Join(xdg, "grcli"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home dir for credential store: %w", err)
	}
	return filepath.Join(home, ".local", "share", "grcli"), nil
}

// Get returns the credentials saved for issuer. ErrNoCredentials when
// either the file is absent (no logins yet) or no entry exists for the
// given issuer.
func (s *Store) Get(issuer string) (*Credentials, error) {
	issuer = canonicalIssuer(issuer)
	if issuer == "" {
		return nil, errors.New("issuer is required")
	}
	f, err := s.load()
	if err != nil {
		return nil, err
	}
	c, ok := f.Credentials[issuer]
	if !ok || c == nil {
		return nil, ErrNoCredentials
	}
	// Defensive: ensure the in-memory record's Issuer field matches the
	// key. A divergence would mean someone hand-edited the JSON; we
	// trust the key.
	c.Issuer = issuer
	return c, nil
}

// Put writes creds.Issuer → creds into the store, replacing any
// existing entry for that issuer. The directory is created if needed
// with 0700; the file is rewritten atomically (temp-file + rename)
// with 0600 perms.
func (s *Store) Put(creds *Credentials) error {
	if creds == nil {
		return errors.New("credentials are required")
	}
	issuer := canonicalIssuer(creds.Issuer)
	if issuer == "" {
		return errors.New("credentials.Issuer is required")
	}
	creds.Issuer = issuer

	f, err := s.load()
	if err != nil {
		return err
	}
	if f.Credentials == nil {
		f.Credentials = map[string]*Credentials{}
	}
	f.Credentials[issuer] = creds
	return s.save(f)
}

// Delete removes the entry for issuer. A missing entry is not an error
// — `grcli logout` against an issuer the user never logged into should
// still succeed.
func (s *Store) Delete(issuer string) error {
	issuer = canonicalIssuer(issuer)
	if issuer == "" {
		return errors.New("issuer is required")
	}
	f, err := s.load()
	if err != nil {
		return err
	}
	delete(f.Credentials, issuer)
	return s.save(f)
}

// load reads the on-disk store. A missing file is not an error —
// callers (Get, Put, Delete) treat that as "empty store". Any other
// error (corrupt JSON, perm denied, wrong version) bubbles up.
func (s *Store) load() (*storeFile, error) {
	data, err := os.ReadFile(s.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &storeFile{Version: currentStoreVersion, Credentials: map[string]*Credentials{}}, nil
		}
		return nil, fmt.Errorf("reading credential store %s: %w", s.Path, err)
	}
	f := &storeFile{}
	if err := json.Unmarshal(data, f); err != nil {
		return nil, fmt.Errorf("decoding credential store %s: %w (delete it to start over)", s.Path, err)
	}
	if f.Version != 0 && f.Version != currentStoreVersion {
		return nil, fmt.Errorf("credential store %s is version %d, grcli only knows version %d", s.Path, f.Version, currentStoreVersion)
	}
	if f.Credentials == nil {
		f.Credentials = map[string]*Credentials{}
	}
	f.Version = currentStoreVersion
	return f, nil
}

// save writes f to disk atomically. Writes through a temp file in the
// same directory and renames so a crash mid-write can't leave the
// store half-written. The temp file inherits 0600 perms; the rename
// preserves them.
func (s *Store) save(f *storeFile) error {
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating credential store directory %s: %w", dir, err)
	}
	f.Version = currentStoreVersion
	body, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding credential store: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "credentials-*.json.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	// Best-effort cleanup if anything below fails before the rename.
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err := os.Rename(tmpPath, s.Path); err != nil {
		return fmt.Errorf("renaming temp file to %s: %w", s.Path, err)
	}
	cleanup = false
	return nil
}

// canonicalIssuer trims trailing slashes and whitespace so issuer
// "https://auth.grc.store/realms/gemara/" and "https://auth.grc.store/realms/gemara"
// hit the same store entry. Keycloak emits the no-slash form in its
// `iss` claim, but operators or copy-pasted URLs may include one.
func canonicalIssuer(s string) string {
	return strings.TrimRight(strings.TrimSpace(s), "/")
}
