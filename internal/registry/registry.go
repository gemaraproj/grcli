// SPDX-License-Identifier: LicenseRef-Revanite-Proprietary

// Package registry packs a Gemara bundle and writes it to an OCI target.
// The same Pack call services both the live-push path (remote.Repository)
// and the dry-run path (oci.Store on disk) — the only difference is
// which target is passed in.
package registry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/gemaraproj/go-gemara/bundle"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/revanite-io/grcli/internal/digest"
)

// PackInput is the data registry.Pack needs to build the bundle.
// Body is the merged artifact YAML; Provenance is the SLSA predicate
// (typically a provenance.Predicate) embedded in the OCI config blob
// under metadata.provenance.
type PackInput struct {
	Filename      string
	ArtifactType  string
	ArtifactID    string
	GemaraVersion string
	Body          []byte
	Provenance    any // marshaled into bundle.Manifest.Metadata
}

// PushResult reports what was published.
type PushResult struct {
	ManifestDigest string
	BodyDigest     string
	Tag            string
	Reference      string // <registry>/<repository>:<tag>
}

// PushRemote packs the bundle and pushes it to <registry>/<repository>:<tag>.
// Auth flows through the default Docker credential chain plus the
// $GRCLI_REGISTRY_PASSWORD / $GRCLI_REGISTRY_USERNAME env pair if set,
// matching how oras CLI resolves auth.
func PushRemote(ctx context.Context, registryHost, repository, tag string, in PackInput) (*PushResult, error) {
	if tag == "" {
		return nil, errors.New("--tag is required (or derivable from metadata.version)")
	}
	repo, err := newRemoteRepo(registryHost, repository)
	if err != nil {
		return nil, err
	}

	desc, bodyDigest, err := pack(ctx, repo, tag, in)
	if err != nil {
		return nil, err
	}
	return &PushResult{
		ManifestDigest: desc.Digest.String(),
		BodyDigest:     bodyDigest,
		Tag:            tag,
		Reference:      fmt.Sprintf("%s/%s:%s", registryHost, repository, tag),
	}, nil
}

// UnpackRemote pulls a Gemara bundle from <registry>/<repository>:<tag>.
// Auth uses the same chain as PushRemote.
func UnpackRemote(ctx context.Context, registryHost, repository, tag string) (*bundle.Bundle, error) {
	if tag == "" {
		return nil, errors.New("--tag is required")
	}
	repo, err := newRemoteRepo(registryHost, repository)
	if err != nil {
		return nil, err
	}
	return bundle.Unpack(ctx, repo, tag)
}

// newRemoteRepo constructs an authenticated oras remote.Repository for
// the given host + repo path. Shared by PushRemote and UnpackRemote.
//
// registryHost may include an http:// or https:// scheme prefix —
// useful when the hub's discovery endpoint advertises a full URL via
// HUB_OCI_PUBLIC_URL (ADR-0026). When http://, the resulting client
// uses plain-HTTP for the upstream registry traffic. When https:// or
// no scheme, TLS is used (oras-go's default).
func newRemoteRepo(registryHost, repository string) (*remote.Repository, error) {
	if registryHost == "" {
		return nil, errors.New("--registry is required")
	}
	if repository == "" {
		return nil, errors.New("--repository is required")
	}
	host, plainHTTP := stripScheme(registryHost)
	repo, err := remote.NewRepository(host + "/" + repository)
	if err != nil {
		return nil, fmt.Errorf("constructing repository client: %w", err)
	}
	repo.PlainHTTP = plainHTTP
	creds, err := dockerCredentials()
	if err != nil {
		return nil, fmt.Errorf("loading docker credentials: %w", err)
	}
	repo.Client = &auth.Client{
		Client:     retry.DefaultClient,
		Cache:      auth.NewCache(),
		Credential: creds,
	}
	return repo, nil
}

// stripScheme accepts a registry hostname that may be a bare host or
// a URL with an http://[s]:// prefix. Returns the bare host and a
// plainHTTP flag indicating whether the original scheme was plain HTTP.
func stripScheme(in string) (host string, plainHTTP bool) {
	switch {
	case strings.HasPrefix(in, "http://"):
		return strings.TrimPrefix(in, "http://"), true
	case strings.HasPrefix(in, "https://"):
		return strings.TrimPrefix(in, "https://"), false
	default:
		return in, false
	}
}

// UnpackLocal reads a Gemara bundle from an OCI image layout directory.
// It is the inverse of PushLocal: the same dir + tag round-trips the bundle.
func UnpackLocal(ctx context.Context, dir, tag string) (*bundle.Bundle, error) {
	if dir == "" {
		return nil, errors.New("source directory is required")
	}
	if tag == "" {
		return nil, errors.New("tag is required")
	}
	store, err := oci.New(dir)
	if err != nil {
		return nil, fmt.Errorf("opening OCI layout: %w", err)
	}
	return bundle.Unpack(ctx, store, tag)
}

// PushLocal writes the same bundle to an OCI image layout directory.
// Used by --dry-run; identical bundle shape, no network.
func PushLocal(ctx context.Context, dir, tag string, in PackInput) (*PushResult, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating output dir: %w", err)
	}
	store, err := oci.New(dir)
	if err != nil {
		return nil, fmt.Errorf("opening OCI layout: %w", err)
	}
	desc, bodyDigest, err := pack(ctx, store, tag, in)
	if err != nil {
		return nil, err
	}
	return &PushResult{
		ManifestDigest: desc.Digest.String(),
		BodyDigest:     bodyDigest,
		Tag:            tag,
		Reference:      fmt.Sprintf("oci:%s:%s", dir, tag),
	}, nil
}

// pack is the shared assembly path: build the in-memory Bundle, call
// bundle.Pack against the target, then tag the resulting manifest.
func pack(ctx context.Context, target oras.Target, tag string, in PackInput) (ocispec.Descriptor, string, error) {
	if len(in.Body) == 0 {
		return ocispec.Descriptor{}, "", errors.New("artifact body is empty")
	}
	if in.Filename == "" {
		return ocispec.Descriptor{}, "", errors.New("artifact filename is empty")
	}

	bodyDigest := digest.Bytes(in.Body)

	manifest := bundle.Manifest{
		BundleVersion: "1.0",
		GemaraVersion: in.GemaraVersion,
		Metadata:      map[string]any{},
		Artifacts: []bundle.Artifact{{
			Name: in.Filename,
			Type: in.ArtifactType,
			ID:   in.ArtifactID,
			Role: "artifact",
		}},
	}
	if in.Provenance != nil {
		manifest.Metadata["provenance"] = in.Provenance
	}

	b := &bundle.Bundle{
		Manifest: manifest,
		Files: []bundle.File{{
			Name: in.Filename,
			Type: in.ArtifactType,
			Data: in.Body,
		}},
	}

	desc, err := bundle.Pack(ctx, target, b)
	if err != nil {
		return ocispec.Descriptor{}, "", fmt.Errorf("packing bundle: %w", err)
	}
	if err := target.Tag(ctx, desc, tag); err != nil {
		return ocispec.Descriptor{}, "", fmt.Errorf("tagging %s: %w", tag, err)
	}
	return desc, bodyDigest, nil
}

func dockerCredentials() (auth.CredentialFunc, error) {
	// NewStoreFromDocker reads ~/.docker/config.json and any helpers,
	// which is the same chain `docker login` writes to. CI runners
	// that have already done `docker login` get auth for free.
	store, err := credentials.NewStoreFromDocker(credentials.StoreOptions{})
	if err != nil {
		return nil, err
	}
	envCreds := func(_ context.Context, _ string) (auth.Credential, error) {
		// Per-registry env pair: GRCLI_REGISTRY_USERNAME + GRCLI_REGISTRY_PASSWORD
		// is the simplest CI override that doesn't require docker login.
		u := os.Getenv("GRCLI_REGISTRY_USERNAME")
		p := os.Getenv("GRCLI_REGISTRY_PASSWORD")
		if u != "" && p != "" {
			return auth.Credential{Username: u, Password: p}, nil
		}
		// Bearer token via GRCLI_REGISTRY_TOKEN — for registries that
		// take a raw bearer (e.g. some zot deployments).
		if t := os.Getenv("GRCLI_REGISTRY_TOKEN"); t != "" {
			return auth.Credential{AccessToken: t}, nil
		}
		return auth.EmptyCredential, nil
	}
	return func(ctx context.Context, registry string) (auth.Credential, error) {
		if c, err := envCreds(ctx, registry); err == nil && c != (auth.EmptyCredential) {
			return c, nil
		}
		return credentials.Credential(store)(ctx, registry)
	}, nil
}
