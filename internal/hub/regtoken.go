// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
	"time"

	"github.com/revanite-io/grc-store-protocol/registrytoken"
)

// FetchRegistryToken exchanges a hub (Keycloak) bearer token for a
// short-lived OCI Distribution token scoped to the given repository and
// actions, minted by the hub's GET /v2/token endpoint — the bearer realm
// the registry (zot) trusts (ADR-0031 on the backend).
//
// The hub grants pull to everyone and push only to a namespace owner or
// hub admin, so:
//   - pass bearer="" for an anonymous pull token (public reads), and
//   - pass the caller's hub access token (from `grcli login`) for a push
//     token; the hub strips push from the grant if the caller doesn't
//     own the repository's namespace.
//
// The returned token is presented directly to the registry as a Bearer
// credential — grcli pre-fetches rather than doing the WWW-Authenticate
// challenge dance, because the realm is gated on a Keycloak token the
// registry client can't supply on its own.
func FetchRegistryToken(ctx context.Context, hubBaseURL, bearer, repository string, actions []string) (string, error) {
	base := strings.TrimRight(hubBaseURL, "/")
	if base == "" {
		return "", errors.New("hub base URL is required to fetch a registry token")
	}
	if repository == "" {
		return "", errors.New("repository is required to fetch a registry token")
	}
	if len(actions) == 0 {
		return "", errors.New("at least one action (pull/push) is required")
	}

	q := neturl.Values{}
	// service is informational here — the hub sets the token audience from
	// its own config and the registry does not validate it — but we send
	// the scope the registry challenge would ask for, spec-shaped.
	q.Set("scope", "repository:"+repository+":"+strings.Join(actions, ","))
	reqURL := base + "/v2/token?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", fmt.Errorf("building registry-token request for %s: %w", reqURL, err)
	}
	req.Header.Set("Accept", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching registry token from %s: %w", reqURL, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("hub registry-token endpoint %s returned %d: %s",
			reqURL, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var tr registrytoken.Response
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("decoding registry token from %s: %w", reqURL, err)
	}
	tok := tr.BearerToken() // prefer token, fall back to access_token (shared helper)
	if tok == "" {
		return "", fmt.Errorf("hub registry-token endpoint %s returned no token", reqURL)
	}
	return tok, nil
}
