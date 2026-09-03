// SPDX-License-Identifier: Apache-2.0

// Package registry is grcli's read side of the OCI registry: pull a
// Gemara bundle (remote or local layout) and discover its signature
// referrer. Packing, pushing and attaching referrers moved to
// grc-store-clientkit/bundle, shared with privateer-sdk.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gemaraproj/go-gemara/bundle"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/revanite-io/grc-store-protocol/limits"
	"github.com/revanite-io/grc-store-protocol/mediatype"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/retry"
)

// UnpackRemote pulls a Gemara bundle from <registry>/<repository>:<tag>.
// Auth flows through the Docker credential chain plus the GRCLI_REGISTRY_*
// env overrides (GRCLI_REGISTRY_TOKEN is what ensureRegistryToken exports).
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
// the given host + repo path. Shared by UnpackRemote and FetchSignatureBundle.
//
// registryHost may include an http:// or https:// scheme prefix —
// useful when the hub's discovery endpoint advertises a full URL via
// HUB_OCI_PUBLIC_URL. When http://, the resulting client
// uses plain-HTTP for the upstream registry traffic. When https:// or
// no scheme, TLS is used (oras-go's default).
func newRemoteRepo(registryHost, repository string) (*remote.Repository, error) {
	if registryHost == "" {
		return nil, errors.New("registry host is required (hub discovery returned none)")
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
// a URL with an http://[s]:// prefix. Returns the bare host (with any
// trailing slash trimmed) and a plainHTTP flag indicating whether the
// original scheme was plain HTTP. Internal entry point retained for
// the in-package call site in newRemoteRepo; external callers (cmd/)
// should use NormalizeRegistryHost which returns only the bare host.
func stripScheme(in string) (host string, plainHTTP bool) {
	switch {
	case strings.HasPrefix(in, "http://"):
		return strings.TrimRight(strings.TrimPrefix(in, "http://"), "/"), true
	case strings.HasPrefix(in, "https://"):
		return strings.TrimRight(strings.TrimPrefix(in, "https://"), "/"), false
	default:
		return strings.TrimRight(in, "/"), false
	}
}

// NormalizeRegistryHost takes a registry value that may be a bare host
// or a full URL (typically the registry_url advertised by a hub via
// its discovery endpoint) and returns a bare host suitable for
// use in an OCI reference (`<host>/<repo>:<tag>`). Strips any scheme
// and trailing slash. Exported for cmd/verify.go and cmd/publish.go,
// which need a bare-host string for cosign and for the user-printed
// reference; the push/unpack code paths inside this package go through
// newRemoteRepo and use stripScheme directly so PlainHTTP propagates
// to oras-go.
func NormalizeRegistryHost(in string) string {
	host, _ := stripScheme(in)
	return host
}

// maxSignatureBlobBytes caps the referrer manifest and bundle-layer reads
// during signature discovery. Both are tiny JSON blobs; the artifact's own
// content layers are never read here. Shares the wire-contract's ingest cap so
// grcli and the hub agree on what "too big to be a signature" means.
const maxSignatureBlobBytes = limits.MaxPluginBlobBytes

// FetchSignatureBundle resolves <registry>/<repository>:<tag> to its manifest
// and returns the raw Sigstore bundle bytes attached as an OCI referrer, plus
// the manifest digest the signature is bound to (the value the verifier's
// artifact-digest policy checks). It returns (nil, digest, nil) when no
// signature referrer is present — a nil bundle is the caller's ErrUnsigned
// signal, NOT an error; an error is reserved for a genuine transport/parse
// failure so the caller can fail closed (we cannot claim "unsigned" if we could
// not look).
//
// Discovery accepts BOTH referrer artifactTypes a cosign-signed catalog can
// carry, because the stamped type is a function of the SIGNER's cosign major
// version (field-confirmed against a live zot 2026-07-07):
//
//	cosign 2.6.x `sign --new-bundle-format` → mediatype.CosignSignReferrer
//	cosign 3.x   `sign` (bundle by default) → mediatype.SigstoreBundle
//
// The bundle BLOB inside is the identical v0.3 bundle either way. Publishers
// control their own cosign version, so filtering on a single type silently
// treats the other cohort's signed catalogs as unsigned (the earlier
// CosignSignReferrer-only filter did exactly that for cosign-3.x publishes).
// This supersedes grc-store-protocol/mediatype's "RULE — do not cross these",
// whose premise predates cosign 3.x.
//
// Auth flows through the same credential chain as UnpackRemote (the
// GRCLI_REGISTRY_TOKEN the caller minted via ensureRegistryToken is read by
// dockerCredentials), so no token needs threading through this signature.
func FetchSignatureBundle(ctx context.Context, registryHost, repository, tag string) (bundleJSON []byte, artifactDigest string, err error) {
	if tag == "" {
		return nil, "", errors.New("tag is required")
	}
	repo, err := newRemoteRepo(registryHost, repository)
	if err != nil {
		return nil, "", err
	}
	subject, err := repo.Resolve(ctx, tag)
	if err != nil {
		return nil, "", fmt.Errorf("resolving %s: %w", tag, err)
	}
	bundleJSON, err = discoverSignatureBundle(ctx, repo, subject)
	if err != nil {
		return nil, "", err
	}
	return bundleJSON, subject.Digest.String(), nil
}

// discoverSignatureBundle is the target-agnostic half of FetchSignatureBundle
// (split out so it is unit-testable against an in-memory oras store, mirroring
// the hub's ociref.SignatureBundle). It lists ALL referrers of subject and
// keeps those whose artifactType is either signature type (cosign 2.6.x stamps
// CosignSignReferrer, cosign 3.x stamps SigstoreBundle — see
// FetchSignatureBundle), returning the raw bytes of the SigstoreBundle layer
// inside the first match, or nil when none is present (unsigned). An error is
// reserved for a genuine transport/parse failure or a malformed referrer, so
// the caller fails closed.
func discoverSignatureBundle(ctx context.Context, target oras.ReadOnlyTarget, subject ocispec.Descriptor) ([]byte, error) {
	gs, ok := target.(content.ReadOnlyGraphStorage)
	if !ok {
		// A target that can't answer Predecessors can't have referrers
		// discovered → treat as unsigned (the verifier maps nil to ErrUnsigned).
		return nil, nil
	}
	// Empty artifactType = no server-side filter; referrer lists are tiny and
	// filtering client-side is what lets one pass accept both stamp variants.
	all, err := registry.Referrers(ctx, gs, subject, "")
	if err != nil {
		return nil, fmt.Errorf("listing signature referrers: %w", err)
	}
	var refs []ocispec.Descriptor
	for _, r := range all {
		if r.ArtifactType == mediatype.CosignSignReferrer || r.ArtifactType == mediatype.SigstoreBundle {
			refs = append(refs, r)
		}
	}
	if len(refs) == 0 {
		return nil, nil // unsigned — no signature referrer attached
	}
	// Use the first matching referrer: fetch its manifest, then return the layer
	// blob whose media type is the Sigstore bundle JSON.
	manifestBytes, err := fetchCapped(ctx, target, refs[0])
	if err != nil {
		return nil, fmt.Errorf("fetching signature manifest: %w", err)
	}
	var m ocispec.Manifest
	if uerr := json.Unmarshal(manifestBytes, &m); uerr != nil {
		return nil, fmt.Errorf("parsing signature manifest: %w", uerr)
	}
	for _, layer := range m.Layers {
		if layer.MediaType == mediatype.SigstoreBundle {
			blob, ferr := fetchCapped(ctx, target, layer)
			if ferr != nil {
				return nil, fmt.Errorf("fetching signature bundle: %w", ferr)
			}
			return blob, nil
		}
	}
	// A referrer with the cosign artifactType but no bundle layer is a malformed
	// signature, not "unsigned" — surface it rather than silently treating a
	// present signature as absent.
	return nil, fmt.Errorf("signature referrer %s carries no %s layer", refs[0].Digest, mediatype.SigstoreBundle)
}

// fetchCapped reads a descriptor's content with a small cap. Used only for the
// referrer manifest and the bundle-JSON layer — both tiny.
func fetchCapped(ctx context.Context, target oras.ReadOnlyTarget, desc ocispec.Descriptor) ([]byte, error) {
	rc, err := target.Fetch(ctx, desc)
	if err != nil {
		return nil, err
	}
	defer rc.Close() //nolint:errcheck
	return io.ReadAll(io.LimitReader(rc, maxSignatureBlobBytes))
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
