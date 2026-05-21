// SPDX-License-Identifier: LicenseRef-Revanite-Proprietary

package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Discovery mirrors the JSON shape served by the hub's GET
// /.well-known/ext.grc-store endpoint (ADR-0026 on the backend side,
// extended by ADR-0028 with the OIDC fields). The hub advertises its
// paired OCI registry plus the Keycloak coordinates grcli's device-grant
// login uses; the OIDC fields are optional — a hub that hasn't been
// configured for device-grant just omits them, and grcli falls back to
// requiring an explicit --token / GRCLI_TOKEN.
type Discovery struct {
	RegistryURL     string `json:"registry_url"`
	HubURL          string `json:"hub_url"`
	APIVersion      string `json:"api_version"`
	OIDCIssuer      string `json:"oidc_issuer,omitempty"`
	OIDCCLIClientID string `json:"oidc_cli_client_id,omitempty"`
}

// wellKnownPath is appended to the user-supplied hub base URL. RFC
// 8615 §3 'ext.' prefix avoids needing IANA registration.
const wellKnownPath = "/.well-known/ext.grc-store"

// discoveryCache holds one Discovery per normalized base URL for the
// process lifetime. No on-disk cache — discovery is cheap and we want
// the fresh value on every invocation.
var discoveryCache sync.Map // map[string]*Discovery

// Discover fetches the well-known discovery doc from the hub at baseURL.
// Validates that registry_url is present and non-empty; on any failure
// returns an error that names the URL grcli used and what was expected
// so the user knows whether to blame the hub, the network, or the flag.
//
// The result is cached per normalized baseURL for the process lifetime.
// A second call with the same baseURL is a map lookup, no HTTP.
func Discover(ctx context.Context, baseURL string) (*Discovery, error) {
	key := strings.TrimRight(baseURL, "/")
	if key == "" {
		return nil, errors.New("hub base URL is required")
	}
	if cached, ok := discoveryCache.Load(key); ok {
		return cached.(*Discovery), nil
	}

	url := key + wellKnownPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("building discovery request for %s: %w", url, err)
	}
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hub discovery at %s returned %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	d := &Discovery{}
	if err := json.Unmarshal(body, d); err != nil {
		return nil, fmt.Errorf("decoding hub discovery at %s: %w (body: %s)", url, err, strings.TrimSpace(string(body)))
	}
	if d.RegistryURL == "" {
		return nil, fmt.Errorf("hub discovery at %s did not advertise registry_url; the hub is misconfigured (HUB_OCI_PUBLIC_URL must be set)", url)
	}

	discoveryCache.Store(key, d)
	return d, nil
}

// resetDiscoveryCacheForTest is the test-only escape hatch for clearing
// the package-level cache between subtests. Not exported.
func resetDiscoveryCacheForTest() {
	discoveryCache.Range(func(k, _ any) bool {
		discoveryCache.Delete(k)
		return true
	})
}
