// SPDX-License-Identifier: LicenseRef-Revanite-Proprietary

package auth

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestResolve_Order(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := &Store{Path: filepath.Join(dir, "credentials.json")}

	// Seed a stored token with plenty of remaining life so the
	// refresh branch never fires.
	const issuer = "https://auth.example/realms/r"
	require.NoError(t, store.Put(&Credentials{
		Issuer:      issuer,
		AccessToken: "from-store",
		ExpiresAt:   time.Now().Add(time.Hour),
	}))

	t.Run("explicit flag wins over env and store", func(t *testing.T) {
		got, err := Resolve(ctx, ResolveInput{
			ExplicitToken: "from-flag",
			EnvToken:      "from-env",
			Issuer:        issuer,
			Store:         store,
		})
		require.NoError(t, err)
		require.Equal(t, "from-flag", got)
	})

	t.Run("env wins over store when no flag", func(t *testing.T) {
		got, err := Resolve(ctx, ResolveInput{
			EnvToken: "from-env",
			Issuer:   issuer,
			Store:    store,
		})
		require.NoError(t, err)
		require.Equal(t, "from-env", got)
	})

	t.Run("store is consulted when flag and env are empty", func(t *testing.T) {
		got, err := Resolve(ctx, ResolveInput{
			Issuer: issuer,
			Store:  store,
		})
		require.NoError(t, err)
		require.Equal(t, "from-store", got)
	})

	t.Run("no token, no issuer, no store -> ErrNoToken with the no-discovery shape", func(t *testing.T) {
		_, err := Resolve(ctx, ResolveInput{})
		require.Error(t, err)
		var noTok *ErrNoToken
		require.ErrorAs(t, err, &noTok)
		require.Contains(t, err.Error(), "--url not set")
	})

	t.Run("no token, store consulted, no entry -> ErrNoToken with the store-checked shape", func(t *testing.T) {
		_, err := Resolve(ctx, ResolveInput{
			Issuer: "https://auth.example/realms/other",
			Store:  store,
		})
		require.Error(t, err)
		var noTok *ErrNoToken
		require.ErrorAs(t, err, &noTok)
		require.Contains(t, err.Error(), "grcli login")
		require.Contains(t, err.Error(), "https://auth.example/realms/other")
	})
}

func TestResolve_RefreshesWhenNearExpiry(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := &Store{Path: filepath.Join(dir, "credentials.json")}

	now := time.Now()
	require.NoError(t, store.Put(&Credentials{
		Issuer:       "https://auth.example/realms/r",
		AccessToken:  "stale",
		RefreshToken: "refresh-1",
		// 10s of life left — well inside renewalWindow (60s).
		ExpiresAt: now.Add(10 * time.Second),
	}))

	fetcherCalled := false
	refresherCalled := false
	got, err := Resolve(ctx, ResolveInput{
		Issuer:   "https://auth.example/realms/r",
		ClientID: "test-client",
		Store:    store,
		Now:      func() time.Time { return now },
		MetadataFetcher: func(_ context.Context, issuer string) (*OIDCMetadata, error) {
			fetcherCalled = true
			require.Equal(t, "https://auth.example/realms/r", issuer)
			return &OIDCMetadata{Issuer: issuer, TokenEndpoint: "https://auth.example/token"}, nil
		},
		Refresher: func(_ context.Context, _ *OIDCMetadata, clientID, refreshToken string) (*Credentials, error) {
			refresherCalled = true
			require.Equal(t, "test-client", clientID)
			require.Equal(t, "refresh-1", refreshToken)
			return &Credentials{
				Issuer:       "https://auth.example/realms/r",
				AccessToken:  "fresh",
				RefreshToken: "refresh-2",
				ExpiresAt:    now.Add(time.Hour),
			}, nil
		},
	})
	require.NoError(t, err)
	require.Equal(t, "fresh", got)
	require.True(t, fetcherCalled, "MetadataFetcher must run when refresh is needed")
	require.True(t, refresherCalled, "Refresher must run when refresh is needed")

	// And the freshly-refreshed credentials should have been persisted
	// so the next invocation doesn't re-refresh.
	persisted, err := store.Get("https://auth.example/realms/r")
	require.NoError(t, err)
	require.Equal(t, "fresh", persisted.AccessToken)
	require.Equal(t, "refresh-2", persisted.RefreshToken)
}

func TestResolve_RefreshFailureSurfacesLoginHint(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := &Store{Path: filepath.Join(dir, "credentials.json")}

	now := time.Now()
	require.NoError(t, store.Put(&Credentials{
		Issuer:       "https://auth.example/realms/r",
		AccessToken:  "stale",
		RefreshToken: "refresh-revoked",
		ExpiresAt:    now.Add(-time.Second), // already expired
	}))

	_, err := Resolve(ctx, ResolveInput{
		Issuer:   "https://auth.example/realms/r",
		ClientID: "test-client",
		Store:    store,
		Now:      func() time.Time { return now },
		MetadataFetcher: func(_ context.Context, issuer string) (*OIDCMetadata, error) {
			return &OIDCMetadata{Issuer: issuer, TokenEndpoint: "https://auth.example/token"}, nil
		},
		Refresher: func(_ context.Context, _ *OIDCMetadata, _, _ string) (*Credentials, error) {
			return nil, errors.New("invalid_grant: refresh token revoked")
		},
	})
	require.Error(t, err)
	// The user-facing message must point at `grcli login` — otherwise
	// users will retry the same expired refresh and not know what to
	// do next.
	require.Contains(t, err.Error(), "grcli login")
}

func TestResolve_NoRefreshTokenForcesReLogin(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := &Store{Path: filepath.Join(dir, "credentials.json")}

	now := time.Now()
	require.NoError(t, store.Put(&Credentials{
		Issuer:       "https://auth.example/realms/r",
		AccessToken:  "stale",
		RefreshToken: "", // no refresh available
		ExpiresAt:    now.Add(-time.Second),
	}))

	// Capture warn output to be sure no spurious warnings emerge from
	// the failure path.
	var warn bytes.Buffer
	_, err := Resolve(ctx, ResolveInput{
		Issuer:   "https://auth.example/realms/r",
		ClientID: "test-client",
		Store:    store,
		Now:      func() time.Time { return now },
		Warn:     &warn,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no refresh token")
	require.Contains(t, err.Error(), "grcli login")
	require.Empty(t, warn.String())
}
