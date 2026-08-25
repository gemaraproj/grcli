// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// renewalWindow is how close to expiry a stored token gets before
// Resolve refreshes it preemptively. Keycloak's default access token
// TTL on the gemara realm is 900s (see realm-gemara.json.tpl), so a
// 60s window means we refresh ~6.7% before expiry — well clear of any
// reasonable clock skew between the user's machine and Keycloak.
const renewalWindow = 60 * time.Second

// ResolveInput is everything Resolve needs to decide which token to
// use. Grouped as a struct so callers (publish's token-resolution
// glue) don't have to thread a five-arg signature.
type ResolveInput struct {
	// ExplicitToken is the value of --token. Wins over everything
	// else if non-empty.
	ExplicitToken string
	// EnvToken is the value of GRCLI_TOKEN. Wins over stored creds
	// but loses to --token.
	EnvToken string
	// Issuer is the OIDC issuer URL that names which stored
	// credentials to look up. Typically the oidc_issuer field from
	// hub discovery. Empty means "don't look in the store" — used
	// when publish is invoked without --url so no discovery happened.
	Issuer string
	// ClientID is the OIDC client_id used to drive a refresh if the
	// stored token is near expiry. Typically the oidc_cli_client_id
	// field from hub discovery. If empty when a refresh is needed,
	// Resolve returns an error suggesting `grcli login` again.
	ClientID string
	// Store holds the on-disk credentials. Nil disables the stored
	// credentials path entirely — only ExplicitToken / EnvToken are
	// consulted.
	Store *Store
	// MetadataFetcher loads OIDC metadata for the issuer when a
	// refresh is needed. Defaults to FetchOIDCMetadata if nil; tests
	// override to avoid network.
	MetadataFetcher func(ctx context.Context, issuer string) (*OIDCMetadata, error)
	// Refresher exchanges a refresh token for a fresh access token
	// when called. Defaults to RefreshToken if nil; tests override.
	Refresher func(ctx context.Context, meta *OIDCMetadata, clientID, refreshToken string) (*Credentials, error)
	// Now returns the current time. Defaults to time.Now; tests pin
	// it so the renewal-window branch is deterministic.
	Now func() time.Time
	// Warn receives a one-line message when Resolve hits a non-fatal
	// issue worth telling the user about (e.g. silent failure to
	// re-persist a refreshed token). nil discards. Stderr is the
	// usual sink in production.
	Warn io.Writer
}

// ErrNoToken is returned when none of the resolution sources produced
// a token. The error string names every source Resolve tried so the
// user knows which one to populate.
type ErrNoToken struct {
	checkedStore bool
	issuer       string
}

func (e *ErrNoToken) Error() string {
	if e.checkedStore {
		return fmt.Sprintf("no token available: --token unset, GRCLI_TOKEN unset, and no stored credentials for %s — run `grcli login --url <hub>` to sign in", e.issuer)
	}
	return "no token available: --token unset, GRCLI_TOKEN unset, and --url not set so the credential store can't be consulted — pass --url, set GRCLI_TOKEN, or run `grcli login`"
}

// Resolve returns a Bearer token for the publish call. Resolution order:
//
//  1. --token flag (in.ExplicitToken)
//  2. GRCLI_TOKEN env (in.EnvToken)
//  3. Stored credentials keyed by in.Issuer, refreshed if near expiry
//
// Hitting step 3 with stored creds that are expired and no refresh
// token (or with a refresh that fails) returns an error pointing the
// user at `grcli login` — Resolve never silently re-prompts.
func Resolve(ctx context.Context, in ResolveInput) (string, error) {
	if in.ExplicitToken != "" {
		return in.ExplicitToken, nil
	}
	if in.EnvToken != "" {
		return in.EnvToken, nil
	}
	if in.Store == nil || in.Issuer == "" {
		return "", &ErrNoToken{checkedStore: false}
	}

	creds, err := in.Store.Get(in.Issuer)
	if err != nil {
		if errors.Is(err, ErrNoCredentials) {
			return "", &ErrNoToken{checkedStore: true, issuer: in.Issuer}
		}
		return "", err
	}

	now := time.Now
	if in.Now != nil {
		now = in.Now
	}
	if creds.ExpiresAt.After(now().Add(renewalWindow)) {
		return creds.AccessToken, nil
	}

	if creds.RefreshToken == "" {
		return "", fmt.Errorf("stored token for %s has expired and no refresh token is available — run `grcli login --url <hub>` to sign in again", in.Issuer)
	}
	if in.ClientID == "" {
		return "", fmt.Errorf("stored token for %s needs refresh but no OIDC client_id is available — re-run with --url so discovery can provide it, or run `grcli login` again", in.Issuer)
	}

	fetcher := in.MetadataFetcher
	if fetcher == nil {
		fetcher = FetchOIDCMetadata
	}
	refresher := in.Refresher
	if refresher == nil {
		refresher = RefreshToken
	}
	meta, err := fetcher(ctx, in.Issuer)
	if err != nil {
		return "", fmt.Errorf("loading OIDC metadata to refresh stored token: %w", err)
	}
	refreshed, err := refresher(ctx, meta, in.ClientID, creds.RefreshToken)
	if err != nil {
		return "", fmt.Errorf("refreshing stored token: %w — run `grcli login` again", err)
	}
	if err := in.Store.Put(refreshed); err != nil && in.Warn != nil {
		// Refresh succeeded but persistence failed — don't fail the
		// publish, but tell the user so they know they'll have to
		// refresh again on the next call.
		fmt.Fprintf(in.Warn, "warning: refreshed token but failed to persist it: %v\n", err)
	}
	return refreshed.AccessToken, nil
}
