// SPDX-License-Identifier: LicenseRef-Revanite-Proprietary

// Package hub calls the grc.store backend's POST /v1/bundles/sync
// endpoint so the hub indexes a bundle that grcli has already pushed
// to the OCI registry. The request body matches the handler's
// syncRequest struct (internal/server/sync.go in grc.store-backend).
package hub

import (
	"bytes"
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

// SyncRequest mirrors the backend's syncRequest.
type SyncRequest struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
}

// SyncResponse mirrors the backend's syncResponse.
type SyncResponse struct {
	Repository    string   `json:"repository"`
	Tag           string   `json:"tag"`
	ManifestEtag  string   `json:"manifest_etag"`
	ArtifactCount int      `json:"artifact_count"`
	NewCount      int      `json:"new_count"`
	Types         []string `json:"types"`
}

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

// VersionStatus reports whether a (namespace, catalogID, version)
// coordinate is already taken on the hub.
type VersionStatus int

const (
	// VersionAbsent — the coordinate is free to publish (hub 404).
	VersionAbsent VersionStatus = iota
	// VersionPresent — already published at this exact coordinate (hub 200).
	VersionPresent
	// VersionTombstoned — previously published then yanked (hub 410). The
	// coordinate stays permanently taken; versions are immutable.
	VersionTombstoned
)

// VersionExists checks whether a version coordinate is already published,
// via GET /v1/catalogs/{ns}/{id}/versions/{version}. Reads are public, so
// no token is required. Used by `grcli publish` as a pre-flight so it
// halts BEFORE packing/pushing when the version is taken (versions are
// immutable — the registry write would otherwise clobber the existing
// bytes before the hub's sync-time guard could reject it).
func (c *Client) VersionExists(ctx context.Context, namespace, catalogID, version string) (VersionStatus, error) {
	if c.BaseURL == "" {
		return VersionAbsent, errors.New("hub base URL is required")
	}
	url := fmt.Sprintf("%s/v1/catalogs/%s/%s/versions/%s",
		c.BaseURL,
		neturl.PathEscape(namespace),
		neturl.PathEscape(catalogID),
		neturl.PathEscape(version))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return VersionAbsent, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return VersionAbsent, err
	}
	defer resp.Body.Close() //nolint:errcheck
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return VersionAbsent, fmt.Errorf("reading version-check response from %s: %w", url, err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
		return VersionPresent, nil
	case http.StatusNotFound:
		return VersionAbsent, nil
	case http.StatusGone:
		return VersionTombstoned, nil
	default:
		// Same shape as GetCatalog's default branch (URL + status + body
		// snippet) so an operator chasing a 5xx on either endpoint gets the
		// same diagnostic surface.
		return VersionAbsent, fmt.Errorf("hub version check %s returned %d: %s",
			url, resp.StatusCode, strings.TrimSpace(string(body)))
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

// Sync calls POST /v1/bundles/sync. The hub fetches the bundle from
// the registry server-side using its zot connection, so the call
// returns quickly without re-uploading any bytes from this client.
func (c *Client) Sync(ctx context.Context, repository, tag string) (*SyncResponse, error) {
	if c.BaseURL == "" {
		return nil, errors.New("hub base URL is required")
	}
	if c.Token == "" {
		return nil, errors.New("hub token is required (--token or GRCLI_TOKEN)")
	}
	body, err := json.Marshal(SyncRequest{Repository: repository, Tag: tag})
	if err != nil {
		return nil, err
	}
	url := c.BaseURL + "/v1/bundles/sync"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Token)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck

	rb, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("reading sync response from %s: %w", url, err)
	}
	if resp.StatusCode/100 != 2 {
		// Same shape as VersionExists/GetCatalog default branches (URL +
		// status + body snippet) so an operator chasing a 5xx on any hub
		// endpoint sees the same diagnostic surface.
		return nil, fmt.Errorf("hub sync %s returned %d: %s",
			url, resp.StatusCode, strings.TrimSpace(string(rb)))
	}
	out := &SyncResponse{}
	if err := json.Unmarshal(rb, out); err != nil {
		return nil, fmt.Errorf("decoding hub response: %w", err)
	}
	return out, nil
}
