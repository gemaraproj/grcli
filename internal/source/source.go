// SPDX-License-Identifier: Apache-2.0

// Package source loads grcli input files, verifies they describe a single
// artifact, and emits the merged YAML body that goes into the bundle.
package source

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	gemara "github.com/gemaraproj/go-gemara"
	"github.com/gemaraproj/go-gemara/fetcher"
	"sigs.k8s.io/yaml"

	"github.com/gemaraproj/grcli/internal/digest"
)

// Loaded is the result of merging the provided input files into a single
// in-memory artifact ready to be placed in a Gemara bundle.
type Loaded struct {
	// Type is the artifact's metadata.type (e.g. "ControlCatalog").
	Type string
	// ID is the artifact's metadata.id.
	ID string
	// Version is the artifact's metadata.version (used as the OCI tag).
	Version string
	// AuthorID is metadata.author.id; the hub maps this to namespace.
	AuthorID string
	// GemaraVersion is metadata.gemara-version, the spec the artifact targets.
	GemaraVersion string
	// Filename is the bundle-relative name of the merged artifact layer.
	Filename string
	// Body is the YAML bytes that get packed into the bundle as one layer.
	Body []byte
	// SourceDigests maps each input file path to its sha256:<hex>.
	// Used by provenance to record what went in.
	SourceDigests map[string]string
}

// peekedMetadata is the minimal projection of metadata.* fields the loader
// needs before deciding whether to merge or pass through. sigs.k8s.io/yaml
// decodes via JSON, so only json tags are needed.
type peekedMetadata struct {
	Metadata struct {
		ID            string `json:"id"`
		Type          string `json:"type"`
		Version       string `json:"version"`
		GemaraVersion string `json:"gemara-version"`
		Author        struct {
			ID string `json:"id"`
		} `json:"author"`
	} `json:"metadata"`
}

// Load reads sources, ensures they describe one artifact (matching type
// + id), and returns the bytes that will be packed into the bundle.
//
// For ControlCatalog and GuidanceCatalog, multiple sources are merged
// via go-gemara's LoadFiles. For any other type, exactly one source is
// allowed — the file passes through unchanged.
func Load(ctx context.Context, sources []string) (*Loaded, error) {
	if len(sources) == 0 {
		return nil, errors.New("no source files provided")
	}

	digests, err := digestAll(sources)
	if err != nil {
		return nil, err
	}

	var first peekedMetadata
	if err := readYAML(sources[0], &first); err != nil {
		return nil, fmt.Errorf("reading %s: %w", sources[0], err)
	}
	if first.Metadata.Type == "" || first.Metadata.ID == "" {
		return nil, fmt.Errorf("%s: metadata.type and metadata.id are required", sources[0])
	}

	for _, path := range sources[1:] {
		var next peekedMetadata
		if err := readYAML(path, &next); err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		if next.Metadata.Type != first.Metadata.Type {
			return nil, fmt.Errorf("%s declares type %q but %s declares %q — all inputs must describe one artifact",
				path, next.Metadata.Type, sources[0], first.Metadata.Type)
		}
		if next.Metadata.ID != first.Metadata.ID {
			return nil, fmt.Errorf("%s declares id %q but %s declares %q — all inputs must describe one artifact",
				path, next.Metadata.ID, sources[0], first.Metadata.ID)
		}
	}

	body, name, err := mergeOrPassThrough(ctx, first.Metadata.Type, sources)
	if err != nil {
		return nil, err
	}

	return &Loaded{
		Type:          first.Metadata.Type,
		ID:            first.Metadata.ID,
		Version:       first.Metadata.Version,
		AuthorID:      first.Metadata.Author.ID,
		GemaraVersion: first.Metadata.GemaraVersion,
		Filename:      name,
		Body:          body,
		SourceDigests: digests,
	}, nil
}

// mergeOrPassThrough returns the bundle-bound YAML body and a filename.
// Multi-file inputs are merged for the two catalog types go-gemara
// supports; everything else must be a single file.
func mergeOrPassThrough(ctx context.Context, artifactType string, sources []string) ([]byte, string, error) {
	if len(sources) == 1 {
		body, err := os.ReadFile(sources[0])
		if err != nil {
			return nil, "", err
		}
		return body, filepath.Base(sources[0]), nil
	}

	fileFetcher := &fetcher.File{}
	switch artifactType {
	case "ControlCatalog":
		catalog := &gemara.ControlCatalog{}
		if err := catalog.LoadFiles(ctx, fileFetcher, sources); err != nil {
			return nil, "", fmt.Errorf("merging control catalogs: %w", err)
		}
		body, err := yaml.Marshal(catalog)
		if err != nil {
			return nil, "", fmt.Errorf("marshaling merged control catalog: %w", err)
		}
		return body, "control-catalog.yaml", nil
	case "GuidanceCatalog":
		catalog := &gemara.GuidanceCatalog{}
		if err := catalog.LoadFiles(ctx, fileFetcher, sources); err != nil {
			return nil, "", fmt.Errorf("merging guidance catalogs: %w", err)
		}
		body, err := yaml.Marshal(catalog)
		if err != nil {
			return nil, "", fmt.Errorf("marshaling merged guidance catalog: %w", err)
		}
		return body, "guidance-catalog.yaml", nil
	default:
		return nil, "", fmt.Errorf("artifact type %q does not support multi-file merge — pass exactly one --file", artifactType)
	}
}

func readYAML(path string, dst any) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(body, dst)
}

func digestAll(paths []string) (map[string]string, error) {
	digests := make(map[string]string, len(paths))
	for _, path := range paths {
		hashed, err := digest.File(path)
		if err != nil {
			return nil, fmt.Errorf("digesting %s: %w", path, err)
		}
		digests[path] = hashed
	}
	return digests, nil
}
