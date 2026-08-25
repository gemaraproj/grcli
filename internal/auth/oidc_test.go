// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDeviceFlow_HappyPath drives a full device-grant cycle against an
// httptest server: discovery, device authorization, two
// authorization_pending replies, then success. Mirrors the actual
// Keycloak response shapes so the test catches drift between
// PollForToken's polling logic and what a real Keycloak emits.
func TestDeviceFlow_HappyPath(t *testing.T) {
	var tokenCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		issuer := "http://" + r.Host
		fmt.Fprintf(w, `{
		  "issuer": %q,
		  "device_authorization_endpoint": "%s/device-auth",
		  "token_endpoint": "%s/token"
		}`, issuer, issuer, issuer)
	})
	mux.HandleFunc("/device-auth", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{
		  "device_code": "DEV-1",
		  "user_code": "ABCD-EFGH",
		  "verification_uri": "https://auth.example/device",
		  "verification_uri_complete": "https://auth.example/device?user_code=ABCD-EFGH",
		  "expires_in": 300,
		  "interval": 0
		}`)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		switch tokenCalls.Add(1) {
		case 1, 2:
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"authorization_pending","error_description":"keep polling"}`)
		default:
			fmt.Fprint(w, `{
			  "access_token": "real-access",
			  "refresh_token": "real-refresh",
			  "token_type": "Bearer",
			  "expires_in": 900
			}`)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx := context.Background()
	meta, err := FetchOIDCMetadata(ctx, srv.URL)
	require.NoError(t, err)
	require.Equal(t, srv.URL+"/device-auth", meta.DeviceAuthorizationEndpoint)
	require.Equal(t, srv.URL+"/token", meta.TokenEndpoint)

	da, err := StartDeviceFlow(ctx, meta, "test-client")
	require.NoError(t, err)
	require.Equal(t, "ABCD-EFGH", da.UserCode)
	// interval=0 in the response must default to 5s per RFC 8628.
	// In tests we don't want to sleep that long; PollForToken would
	// burn 15s waiting through three rounds. Override it.
	da.Interval = 0

	// Drop the interval to a millisecond range for the test poll loop
	// — the polling code reads da.Interval at top of the loop, then
	// reads its own local `interval` thereafter. We can shortcut by
	// setting Interval=0 to make the default 5s kick in, then patching
	// after StartDeviceFlow. Simpler: post-create, mutate Interval.
	da.Interval = 1 // 1 second is still slow but tolerable

	creds, err := PollForToken(ctx, meta, "test-client", da)
	require.NoError(t, err)
	require.Equal(t, "real-access", creds.AccessToken)
	require.Equal(t, "real-refresh", creds.RefreshToken)
	require.Equal(t, srv.URL, creds.Issuer, "Credentials.Issuer must match the OIDCMetadata.Issuer so the store keys correctly")
	require.GreaterOrEqual(t, tokenCalls.Load(), int32(3), "must have polled past the two authorization_pending replies")
}

func TestDeviceFlow_AccessDeniedIsSentinel(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"access_denied","error_description":"the user said no"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	_, err := PollForToken(context.Background(),
		&OIDCMetadata{TokenEndpoint: srv.URL + "/token"},
		"test-client",
		&DeviceAuthorization{DeviceCode: "DEV-1", Interval: 1},
	)
	// Callers branch on the sentinel — "user denied" is a different
	// recovery path from "auth server is broken".
	require.ErrorIs(t, err, ErrAccessDenied)
}

func TestDeviceFlow_ExpiredTokenIsSentinel(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"expired_token","error_description":"user waited too long"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	_, err := PollForToken(context.Background(),
		&OIDCMetadata{TokenEndpoint: srv.URL + "/token"},
		"test-client",
		&DeviceAuthorization{DeviceCode: "DEV-1", Interval: 1},
	)
	require.ErrorIs(t, err, ErrExpiredDeviceCode)
}

// TestStartDeviceFlow_InvalidClientPointsAtAuthServer guards against
// regressing the "raw {invalid_client} body" UX failure — when Keycloak
// rejects the client, the error must steer the user at the auth-server
// provisioning step, not just reflect the OIDC error code at them.
func TestStartDeviceFlow_InvalidClientPointsAtAuthServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"invalid_client","error_description":"Invalid client or Invalid client credentials"}`)
	}))
	defer srv.Close()

	_, err := StartDeviceFlow(context.Background(),
		&OIDCMetadata{DeviceAuthorizationEndpoint: srv.URL, TokenEndpoint: srv.URL + "/token"},
		"grcli",
	)
	require.Error(t, err)
	// The user-facing message must name the client AND tell them this
	// can't be fixed with credentials grcli provides. Otherwise people
	// hunt for a username/password to type, which doesn't exist for a
	// device-grant client.
	require.Contains(t, err.Error(), `grcli`)
	require.Contains(t, err.Error(), "publicClient=true")
	require.Contains(t, err.Error(), "not a credential you can supply from grcli")
}

func TestFetchOIDCMetadata_RejectsMissingEndpoints(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		// Auth server that doesn't support the device grant — token
		// endpoint present, device endpoint absent. grcli must refuse
		// to proceed rather than getting a generic 404 from the next
		// call.
		fmt.Fprint(w, `{"issuer":"http://x","token_endpoint":"http://x/token"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	_, err := FetchOIDCMetadata(context.Background(), srv.URL)
	require.Error(t, err)
	require.Contains(t, err.Error(), "device_authorization_endpoint")
}
