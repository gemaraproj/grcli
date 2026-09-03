// SPDX-License-Identifier: Apache-2.0

// Package hub is grcli's read-side client for the grc.store catalog
// routes (GET /v1/catalogs/...). The publish-side surface (discovery,
// registry tokens, version preflight, sync) lives in
// grc-store-clientkit/hub, shared with privateer-sdk.
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
)

// Client is the typed wrapper around the hub's HTTP API.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// New returns a Client with sensible defaults.
func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}
}

// ErrCatalogNotFound wraps a hub 404 for a catalog coordinate.
// ErrCatalogTombstoned wraps a hub 410 (the catalog was published and
// later yanked — the coordinate stays permanently taken). Both are
// exported so callers can errors.Is against them to distinguish each
// hub-modeled outcome from a transport failure.
var (
	ErrCatalogNotFound   = errors.New("catalog not found")
	ErrCatalogTombstoned = errors.New("catalog was yanked")
)

// Release is one published version of a catalog, as returned in the
// releases[] array of GET /v1/catalogs/{ns}/{id}.
type Release struct {
	Version        string `json:"version"`
	ManifestDigest string `json:"manifest_digest"`
	PushedAt       string `json:"pushed_at"`
	// License is this version's publication license (canonical SPDX
	// expression), exposed per-release by the hub. Absent when none was
	// declared. Used by reference resolution to compare a dependency's
	// license against the primary's.
	License string `json:"license,omitempty"`
}

// ReleaseFor returns the release matching version, or nil if the catalog has
// no such version.
func (c *Catalog) ReleaseFor(version string) *Release {
	for i := range c.Releases {
		if c.Releases[i].Version == version {
			return &c.Releases[i]
		}
	}
	return nil
}

// Catalog mirrors the JSON returned by GET /v1/catalogs/{ns}/{id}.
// Only the fields the CLI currently surfaces are typed; the hub may
// add more without breaking this client.
type Catalog struct {
	Namespace            string    `json:"namespace"`
	CatalogID            string    `json:"catalog_id"`
	Type                 string    `json:"type"`
	Category             string    `json:"category"`
	Title                string    `json:"title"`
	Summary              string    `json:"summary"`
	AuthorName           string    `json:"author_name"`
	LatestVersion        string    `json:"latest_version"`
	LatestManifestDigest string    `json:"latest_manifest_digest"`
	Releases             []Release `json:"releases"`
	// SignerIdentity is the canonical keyless signer the hub verified and
	// TOFU-pinned for this coordinate at ingest — "keyless:<issuer>#<workflow-path>",
	// ref-stripped (grc-store-protocol/identity). Absent when
	// no signed version has been ingested (or the hub predates hub-side
	// verification); grcli verify's zero-flag mode reads it to derive the cosign
	// trust policy without the consumer having to know the workflow path.
	SignerIdentity string `json:"signer_identity,omitempty"`
}

// GetCatalog fetches a catalog and its releases via
// GET /v1/catalogs/{ns}/{id}. Reads are public, so no token is required.
// Returns a wrapped ErrCatalogNotFound on 404 and ErrCatalogTombstoned
// on 410 so callers can errors.Is against either to distinguish the
// "no such catalog" and "yanked" cases from a transport failure.
func (c *Client) GetCatalog(ctx context.Context, namespace, catalogID string) (*Catalog, error) {
	if c.BaseURL == "" {
		return nil, errors.New("hub base URL is required")
	}
	if namespace == "" || catalogID == "" {
		return nil, errors.New("namespace and catalog id are required")
	}
	url := fmt.Sprintf("%s/v1/catalogs/%s/%s",
		c.BaseURL,
		neturl.PathEscape(namespace),
		neturl.PathEscape(catalogID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("reading catalog response from %s: %w", url, err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
		if len(body) == 0 {
			return nil, fmt.Errorf("hub catalog lookup %s returned 200 with empty body", url)
		}
		out := &Catalog{}
		if err := json.Unmarshal(body, out); err != nil {
			return nil, fmt.Errorf("decoding catalog response: %w", err)
		}
		return out, nil
	case http.StatusNotFound:
		return nil, fmt.Errorf("%w: %s/%s", ErrCatalogNotFound, namespace, catalogID)
	case http.StatusGone:
		return nil, fmt.Errorf("%w: %s/%s", ErrCatalogTombstoned, namespace, catalogID)
	default:
		return nil, fmt.Errorf("hub catalog lookup %s returned %d: %s",
			url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
}

// GetVersionBody fetches a single version's artifact body via
// GET /v1/catalogs/{ns}/{id}/versions/{version}. Reads are public, so no token
// is required. Returns the body bytes and the artifact's OCI manifest digest
// (from the X-Gemara-Manifest-Digest response header). Wraps ErrCatalogNotFound
// on 404 and ErrCatalogTombstoned on 410 (a yanked version) so callers can
// distinguish those from a transport failure.
func (c *Client) GetVersionBody(ctx context.Context, namespace, catalogID, version string) (body []byte, manifestDigest string, err error) {
	if c.BaseURL == "" {
		return nil, "", errors.New("hub base URL is required")
	}
	if namespace == "" || catalogID == "" || version == "" {
		return nil, "", errors.New("namespace, catalog id, and version are required")
	}
	url := fmt.Sprintf("%s/v1/catalogs/%s/%s/versions/%s",
		c.BaseURL,
		neturl.PathEscape(namespace),
		neturl.PathEscape(catalogID),
		neturl.PathEscape(version))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close() //nolint:errcheck
	// Bodies are small Gemara artifacts; 16 MiB is a generous ceiling that
	// still guards against a runaway response.
	rb, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, "", fmt.Errorf("reading version body from %s: %w", url, err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
		if len(rb) == 0 {
			return nil, "", fmt.Errorf("hub version fetch %s returned 200 with empty body", url)
		}
		return rb, resp.Header.Get("X-Gemara-Manifest-Digest"), nil
	case http.StatusNotFound:
		return nil, "", fmt.Errorf("%w: %s/%s@%s", ErrCatalogNotFound, namespace, catalogID, version)
	case http.StatusGone:
		return nil, "", fmt.Errorf("%w: %s/%s@%s", ErrCatalogTombstoned, namespace, catalogID, version)
	default:
		return nil, "", fmt.Errorf("hub version fetch %s returned %d: %s",
			url, resp.StatusCode, strings.TrimSpace(string(rb)))
	}
}
