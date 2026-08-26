// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStore_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Path: filepath.Join(dir, "credentials.json")}

	// Get on a missing file returns ErrNoCredentials, not a wrapped
	// os error. Callers depend on this to fall back cleanly.
	_, err := s.Get("https://auth.grc.store/realms/gemara")
	require.ErrorIs(t, err, ErrNoCredentials)

	creds := &Credentials{
		Issuer:       "https://auth.grc.store/realms/gemara",
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		TokenType:    "Bearer",
		ExpiresAt:    time.Now().Add(time.Hour).UTC().Truncate(time.Second),
	}
	require.NoError(t, s.Put(creds))

	got, err := s.Get(creds.Issuer)
	require.NoError(t, err)
	require.Equal(t, creds.AccessToken, got.AccessToken)
	require.Equal(t, creds.RefreshToken, got.RefreshToken)
	require.True(t, got.ExpiresAt.Equal(creds.ExpiresAt), "ExpiresAt round-trip mismatch: %v vs %v", got.ExpiresAt, creds.ExpiresAt)

	// Delete removes the entry; subsequent Get is ErrNoCredentials again.
	require.NoError(t, s.Delete(creds.Issuer))
	_, err = s.Get(creds.Issuer)
	require.ErrorIs(t, err, ErrNoCredentials)
}

func TestStore_FilePermsAre0600(t *testing.T) {
	// Skip on Windows — file mode bits there mean something different
	// and 0600 is not enforced the same way. The XDG path layout we
	// target is POSIX in practice.
	if runtime.GOOS == "windows" {
		t.Skip("posix-style perms not enforced on windows")
	}

	dir := t.TempDir()
	s := &Store{Path: filepath.Join(dir, "credentials.json")}
	require.NoError(t, s.Put(&Credentials{
		Issuer:      "https://auth.example/realms/r",
		AccessToken: "x",
		ExpiresAt:   time.Now().Add(time.Hour),
	}))

	info, err := os.Stat(s.Path)
	require.NoError(t, err)
	// The token file holds long-lived credentials. World-readable
	// here would be a real footgun on shared dev machines.
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "credentials file must be 0600")
}

func TestStore_MultipleIssuersCoexist(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Path: filepath.Join(dir, "credentials.json")}

	a := &Credentials{Issuer: "https://auth.example/realms/one", AccessToken: "tok-1", ExpiresAt: time.Now().Add(time.Hour)}
	b := &Credentials{Issuer: "https://auth.example/realms/two", AccessToken: "tok-2", ExpiresAt: time.Now().Add(time.Hour)}
	require.NoError(t, s.Put(a))
	require.NoError(t, s.Put(b))

	gotA, err := s.Get(a.Issuer)
	require.NoError(t, err)
	require.Equal(t, "tok-1", gotA.AccessToken)
	gotB, err := s.Get(b.Issuer)
	require.NoError(t, err)
	require.Equal(t, "tok-2", gotB.AccessToken)

	// Deleting one issuer does not touch the other — the store's "one
	// JSON file holding all logins" shape risks this if the rewrite
	// drops other entries.
	require.NoError(t, s.Delete(a.Issuer))
	_, err = s.Get(a.Issuer)
	require.ErrorIs(t, err, ErrNoCredentials)
	gotB2, err := s.Get(b.Issuer)
	require.NoError(t, err)
	require.Equal(t, "tok-2", gotB2.AccessToken)
}

func TestStore_TrailingSlashIsCanonicalized(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Path: filepath.Join(dir, "credentials.json")}

	require.NoError(t, s.Put(&Credentials{
		Issuer:      "https://auth.example/realms/r/",
		AccessToken: "tok",
		ExpiresAt:   time.Now().Add(time.Hour),
	}))
	// Look up both with-slash and without-slash; either should hit
	// the same record.
	for _, key := range []string{"https://auth.example/realms/r/", "https://auth.example/realms/r"} {
		got, err := s.Get(key)
		require.NoError(t, err, "lookup with key %q", key)
		require.Equal(t, "tok", got.AccessToken)
	}
}

func TestStore_CorruptFileReportsActionableError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))
	s := &Store{Path: path}
	_, err := s.Get("https://auth.example/realms/r")
	require.Error(t, err)
	// The error should point the user at the file path and at the
	// "delete to start over" recovery — vague decoding errors leave
	// users guessing.
	require.Contains(t, err.Error(), "delete it to start over")
}

func TestStore_VersionMismatchRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	// Future-version file shape — grcli should refuse to downgrade
	// rather than silently treating it as empty.
	require.NoError(t, os.WriteFile(path, []byte(`{"version":99,"credentials":{}}`), 0o600))
	s := &Store{Path: path}
	_, err := s.Get("https://auth.example/realms/r")
	require.Error(t, err)
	require.Contains(t, err.Error(), "version 99")
	// And explicitly NOT ErrNoCredentials — that would silently
	// shadow the version mismatch and lead to weird re-login loops.
	require.False(t, errors.Is(err, ErrNoCredentials), "version mismatch must not collapse into ErrNoCredentials")
}
