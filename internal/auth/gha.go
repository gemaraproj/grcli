// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"strings"
	"time"
)

// InGitHubActions reports whether the process is running inside a GitHub
// Actions job with workload OIDC available — i.e. the workflow set
// `permissions: id-token: write` and the runtime injected the request
// URL + token. Used to pick the CI publish path (ADR-0032): no stored
// secret, the workflow's OIDC token IS the credential.
func InGitHubActions() bool {
	return os.Getenv("GITHUB_ACTIONS") == "true" &&
		os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL") != "" &&
		os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN") != ""
}

// FetchGitHubActionsToken requests a workload-OIDC token from the Actions
// runtime for the given audience and returns the raw JWT. The hub
// validates that token directly (its iss is GitHub's, its aud is the
// hub's CI audience) and maps the workflow's repository/ref through the
// trusted-publisher bindings — so the audience must match the hub's
// HUB_CI_OIDC_AUDIENCE. Callers pass the value the hub advertises as
// ci_audience in its discovery doc (falling back to the hub URL).
func FetchGitHubActionsToken(ctx context.Context, audience string) (string, error) {
	reqURL := os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL")
	reqTok := os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN")
	if reqURL == "" || reqTok == "" {
		return "", fmt.Errorf("not in GitHub Actions (ACTIONS_ID_TOKEN_REQUEST_URL/TOKEN unset)")
	}
	if audience == "" {
		return "", fmt.Errorf("audience is required for the GitHub Actions OIDC token")
	}
	sep := "?"
	if strings.Contains(reqURL, "?") {
		sep = "&"
	}
	full := reqURL + sep + "audience=" + neturl.QueryEscape(audience)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return "", fmt.Errorf("building GHA OIDC request: %w", err)
	}
	req.Header.Set("Authorization", "bearer "+reqTok)
	req.Header.Set("Accept", "application/json; api-version=2.0")

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching GHA OIDC token: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GHA OIDC endpoint returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("decoding GHA OIDC response: %w", err)
	}
	if out.Value == "" {
		return "", fmt.Errorf("GHA OIDC response had no token value")
	}
	return out.Value, nil
}
