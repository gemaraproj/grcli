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
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))

	switch resp.StatusCode {
	case http.StatusOK:
		return VersionPresent, nil
	case http.StatusNotFound:
		return VersionAbsent, nil
	case http.StatusGone:
		return VersionTombstoned, nil
	default:
		return VersionAbsent, fmt.Errorf("hub version check %s returned %d", url, resp.StatusCode)
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/v1/bundles/sync", bytes.NewReader(body))
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

	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("hub /v1/bundles/sync returned %d: %s", resp.StatusCode, strings.TrimSpace(string(rb)))
	}
	out := &SyncResponse{}
	if err := json.Unmarshal(rb, out); err != nil {
		return nil, fmt.Errorf("decoding hub response: %w", err)
	}
	return out, nil
}
