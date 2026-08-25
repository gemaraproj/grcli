// SPDX-License-Identifier: Apache-2.0

// Package auth implements the interactive OIDC login flow for grcli —
// OAuth 2.0 Device Authorization Grant (RFC 8628) against a Keycloak
// configured per ADR-0028 — plus the credential storage and token
// refresh that lets `grcli publish` pick up a valid Bearer token
// without the user repeating `grcli login` every 15 minutes.
//
// CI federation (GitHub Actions OIDC → Keycloak via token-exchange) is
// a separate code path layered on top of the same Store / Resolve
// surface in a follow-up phase; this file deals only with the
// interactive flow.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// httpTimeout caps every outbound HTTP call in this package. Device
// authorization and token endpoints are well under a second in normal
// operation; 10s is wide enough to absorb a slow tunnel without letting
// a wedged Keycloak hang a CLI session indefinitely. The polling loop
// in PollForToken uses the device-grant's own `interval` instead — this
// timeout is per-request, not per-poll-cycle.
const httpTimeout = 10 * time.Second

// OIDCMetadata is the subset of the OpenID Connect Discovery 1.0
// document grcli's device-grant flow consumes. Fetched from
// `<issuer>/.well-known/openid-configuration` — the OIDC standard
// discovery path, not the hub's separate grc-store-configuration doc.
type OIDCMetadata struct {
	Issuer                      string `json:"issuer"`
	DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
	TokenEndpoint               string `json:"token_endpoint"`
}

// FetchOIDCMetadata loads the discovery doc for issuerURL and returns
// the endpoints we need. issuerURL is the canonical issuer (e.g.
// https://auth.grc.store/realms/gemara), not a hub URL — the value
// grcli got from the hub's /.well-known/grc-store-configuration doc's oidc_issuer
// field. Errors name the URL grcli used so the user can tell whether
// to blame the hub's discovery doc or the Keycloak itself.
func FetchOIDCMetadata(ctx context.Context, issuerURL string) (*OIDCMetadata, error) {
	issuerURL = strings.TrimRight(issuerURL, "/")
	if issuerURL == "" {
		return nil, errors.New("issuer URL is required")
	}
	discoveryURL := issuerURL + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return nil, fmt.Errorf("building OIDC discovery request for %s: %w", discoveryURL, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Timeout: httpTimeout}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", discoveryURL, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OIDC discovery at %s returned %d: %s", discoveryURL, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	m := &OIDCMetadata{}
	if err := json.Unmarshal(body, m); err != nil {
		return nil, fmt.Errorf("decoding OIDC discovery at %s: %w", discoveryURL, err)
	}
	if m.DeviceAuthorizationEndpoint == "" {
		return nil, fmt.Errorf("OIDC discovery at %s did not advertise device_authorization_endpoint; the auth server is not configured for the device grant", discoveryURL)
	}
	if m.TokenEndpoint == "" {
		return nil, fmt.Errorf("OIDC discovery at %s did not advertise token_endpoint", discoveryURL)
	}
	if m.Issuer == "" {
		// Mirror the URL grcli used as the canonical key. Some IdPs
		// trim trailing slashes; we already did.
		m.Issuer = issuerURL
	}
	return m, nil
}

// DeviceAuthorization is RFC 8628 §3.2's device authorization response.
type DeviceAuthorization struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// StartDeviceFlow calls the device_authorization_endpoint and returns
// the response. The caller is responsible for displaying user_code +
// verification_uri to the user and then handing the response to
// PollForToken to wait for completion.
func StartDeviceFlow(ctx context.Context, meta *OIDCMetadata, clientID string) (*DeviceAuthorization, error) {
	form := url.Values{}
	form.Set("client_id", clientID)
	// Standard OIDC scopes — explicitly NOT including `offline_access`.
	// Offline tokens require both the client to allow them and the user
	// to hold the `offline_access` realm role; on a freshly-bootstrapped
	// realm the latter is easy to miss and the device endpoint then
	// refuses the whole flow with "not_allowed: Offline tokens not
	// allowed for the user or client". For an interactive CLI we don't
	// need offline tokens — the regular SSO-session-bound refresh
	// token Keycloak still issues here is good for the realm's idle
	// window (~30m sliding, ~10h max with defaults). Beyond that, the
	// user just re-runs `grcli login`. If a future use case actually
	// needs longer-lived tokens (a daemon, a service account that
	// can't re-login), add `offline_access` back here and make sure
	// the realm's default-roles-<realm> composite grants it.
	form.Set("scope", "openid profile email")

	resp, body, err := postForm(ctx, meta.DeviceAuthorizationEndpoint, form)
	if err != nil {
		return nil, fmt.Errorf("device authorization request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// Decode the standard OIDC error shape so we can pattern-match
		// on the specific failure modes that have actionable fixes,
		// rather than reflecting the raw body back at the user.
		var er struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		_ = json.Unmarshal(body, &er)
		switch er.Error {
		case "invalid_client":
			// This fires when the client_id isn't recognized at all,
			// or when it's marked confidential and grcli (correctly,
			// per the public-client spec for device grant) sent no
			// secret. The actual fix is always on the auth server
			// side — there's no client-side credential we can mint to
			// satisfy it. Steer the user there explicitly.
			return nil, fmt.Errorf("device authorization rejected: Keycloak does not recognize client_id %q as a public device-grant client. The client either doesn't exist in the realm yet, isn't marked publicClient=true, or doesn't have oauth2.device.authorization.grant.enabled=true. Ask whoever runs the auth server to provision the client; this is not a credential you can supply from grcli", clientID)
		case "unauthorized_client":
			return nil, fmt.Errorf("device authorization rejected: client_id %q exists but is not allowed to use the device grant. Enable oauth2.device.authorization.grant.enabled=true on the client", clientID)
		}
		// Fall through to the raw body for anything we haven't seen.
		return nil, fmt.Errorf("device_authorization_endpoint returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	d := &DeviceAuthorization{}
	if err := json.Unmarshal(body, d); err != nil {
		return nil, fmt.Errorf("decoding device authorization response: %w (body: %s)", err, strings.TrimSpace(string(body)))
	}
	if d.DeviceCode == "" || d.UserCode == "" || d.VerificationURI == "" {
		return nil, fmt.Errorf("device authorization response missing required fields: %s", strings.TrimSpace(string(body)))
	}
	if d.Interval <= 0 {
		// RFC 8628 §3.2 — interval defaults to 5s if absent.
		d.Interval = 5
	}
	return d, nil
}

// tokenResponse is the JSON returned by the OIDC token endpoint. Both
// the success and error shapes share this struct; on errors `Error` is
// populated and the other fields are zero.
type tokenResponse struct {
	AccessToken      string `json:"access_token,omitempty"`
	RefreshToken     string `json:"refresh_token,omitempty"`
	TokenType        string `json:"token_type,omitempty"`
	ExpiresIn        int    `json:"expires_in,omitempty"`
	IDToken          string `json:"id_token,omitempty"`
	Error            string `json:"error,omitempty"`
	ErrorDescription string `json:"error_description,omitempty"`
}

// PollForToken blocks polling the token endpoint until the device flow
// terminates. Returns Credentials on success (with ExpiresAt computed
// from `expires_in`), a sentinel error on user-facing failures
// (ErrAccessDenied, ErrExpiredToken), or a wrapped error on transport
// or unexpected-response problems.
//
// Honors `slow_down` per RFC 8628 §3.5 by adding 5s to the polling
// interval. Honors the response `interval` set by Keycloak if it
// changes mid-flow.
func PollForToken(ctx context.Context, meta *OIDCMetadata, clientID string, da *DeviceAuthorization) (*Credentials, error) {
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
	form.Set("device_code", da.DeviceCode)

	interval := time.Duration(da.Interval) * time.Second

	for {
		// Wait first — the spec says "after the user has presented the
		// user code in their device" so an immediate poll is wasted,
		// and many Keycloak instances will return slow_down if you do.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}

		_, body, err := postForm(ctx, meta.TokenEndpoint, form)
		if err != nil {
			return nil, fmt.Errorf("polling token endpoint: %w", err)
		}
		tr := tokenResponse{}
		if err := json.Unmarshal(body, &tr); err != nil {
			return nil, fmt.Errorf("decoding token response: %w (body: %s)", err, strings.TrimSpace(string(body)))
		}
		switch tr.Error {
		case "":
			// Success.
			if tr.AccessToken == "" {
				return nil, fmt.Errorf("token endpoint returned no access_token and no error: %s", strings.TrimSpace(string(body)))
			}
			return credsFromTokenResponse(meta.Issuer, &tr), nil
		case "authorization_pending":
			continue
		case "slow_down":
			interval += 5 * time.Second
			continue
		case "access_denied":
			return nil, ErrAccessDenied
		case "expired_token":
			return nil, ErrExpiredDeviceCode
		default:
			// Surface anything unexpected with the auth server's own
			// description if it provided one — Keycloak does.
			if tr.ErrorDescription != "" {
				return nil, fmt.Errorf("token endpoint error %q: %s", tr.Error, tr.ErrorDescription)
			}
			return nil, fmt.Errorf("token endpoint error %q", tr.Error)
		}
	}
}

// RefreshToken exchanges a refresh_token for a new access token. Used by
// Resolve when stored credentials are within the renewal window.
// Returns a fresh Credentials populated with the new access + refresh
// tokens. Errors are wrapped — callers typically tell the user to run
// `grcli login` again if refresh fails.
func RefreshToken(ctx context.Context, meta *OIDCMetadata, clientID, refreshToken string) (*Credentials, error) {
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)

	_, body, err := postForm(ctx, meta.TokenEndpoint, form)
	if err != nil {
		return nil, fmt.Errorf("refresh request: %w", err)
	}
	tr := tokenResponse{}
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("decoding refresh response: %w (body: %s)", err, strings.TrimSpace(string(body)))
	}
	if tr.Error != "" {
		if tr.ErrorDescription != "" {
			return nil, fmt.Errorf("refresh failed (%s): %s", tr.Error, tr.ErrorDescription)
		}
		return nil, fmt.Errorf("refresh failed: %s", tr.Error)
	}
	if tr.AccessToken == "" {
		return nil, fmt.Errorf("refresh response missing access_token: %s", strings.TrimSpace(string(body)))
	}
	return credsFromTokenResponse(meta.Issuer, &tr), nil
}

// postForm posts the given form to url and returns (response, body, err).
// Pulled out because every Keycloak call in this file uses the same
// shape, and inlining would mean three near-identical 10-line blobs.
func postForm(ctx context.Context, endpoint string, form url.Values) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, nil, fmt.Errorf("building request for %s: %w", endpoint, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Timeout: httpTimeout}).Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("fetching %s: %w", endpoint, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	return resp, body, nil
}

// credsFromTokenResponse converts a tokenResponse into a Credentials
// record. The 30-second floor on lifetime keeps freshly-issued tokens
// from being flagged "expired" by Resolve's 60-second renewal window
// when the auth server issues tokens with a tiny TTL (some Keycloak
// dev configs use 60s). Without the floor we'd refresh-loop.
func credsFromTokenResponse(issuer string, tr *tokenResponse) *Credentials {
	lifetime := time.Duration(tr.ExpiresIn) * time.Second
	if lifetime < 30*time.Second {
		lifetime = 30 * time.Second
	}
	return &Credentials{
		Issuer:       issuer,
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		TokenType:    tr.TokenType,
		ExpiresAt:    time.Now().Add(lifetime),
	}
}

// User-facing sentinel errors returned by PollForToken.
var (
	ErrAccessDenied      = errors.New("authorization denied by the user")
	ErrExpiredDeviceCode = errors.New("device code expired before authorization completed; run `grcli login` again")
)
