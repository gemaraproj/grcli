// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/revanite-io/grc-store-protocol/mediatype"
	"github.com/stretchr/testify/require"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/registry"

	ckbundle "github.com/gemaraproj/grc-store-clientkit/bundle"
	"github.com/gemaraproj/grcli/internal/digest"
)

// pushBlob pushes raw bytes with the given media type and returns its descriptor.
func pushBlob(t *testing.T, store *memory.Store, mediaType string, data []byte) ocispec.Descriptor {
	t.Helper()
	desc := ocispec.Descriptor{
		MediaType: mediaType,
		Digest:    godigest.Digest(digest.Bytes(data)),
		Size:      int64(len(data)),
	}
	require.NoError(t, store.Push(context.Background(), desc, bytes.NewReader(data)))
	return desc
}

// pushManifest marshals and pushes an image manifest, returning its descriptor.
func pushManifest(t *testing.T, store *memory.Store, m ocispec.Manifest) ocispec.Descriptor {
	t.Helper()
	m.MediaType = ocispec.MediaTypeImageManifest
	data, err := json.Marshal(m)
	require.NoError(t, err)
	desc := ocispec.Descriptor{
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: m.ArtifactType,
		Digest:       godigest.Digest(digest.Bytes(data)),
		Size:         int64(len(data)),
	}
	require.NoError(t, store.Push(context.Background(), desc, bytes.NewReader(data)))
	return desc
}

// subjectManifest pushes a minimal artifact manifest to act as the signature's
// subject (the thing being verified).
func subjectManifest(t *testing.T, store *memory.Store) ocispec.Descriptor {
	t.Helper()
	config := pushBlob(t, store, "application/vnd.grc-store.test.config", []byte(`{}`))
	body := pushBlob(t, store, "application/vnd.grc-store.test.body", []byte("artifact-body"))
	return pushManifest(t, store, ocispec.Manifest{Config: config, Layers: []ocispec.Descriptor{body}})
}

// attachSignature pushes a signature referrer of subject with the given
// referrer artifactType and layer media type, carrying bundleBytes.
func attachSignature(t *testing.T, store *memory.Store, subject ocispec.Descriptor, artifactType, layerMediaType string, bundleBytes []byte) {
	t.Helper()
	config := pushBlob(t, store, artifactType, []byte(`{}`))
	layer := pushBlob(t, store, layerMediaType, bundleBytes)
	subjCopy := subject
	pushManifest(t, store, ocispec.Manifest{
		ArtifactType: artifactType,
		Config:       config,
		Layers:       []ocispec.Descriptor{layer},
		Subject:      &subjCopy,
	})
}

// TestDiscoverSignatureBundle_ReadsClientkitAttach is the producer↔consumer
// contract with the shared publisher: what grc-store-clientkit's AttachReferrer
// stamps (artifactType + layer mediatype.SigstoreBundle) is exactly what this
// discovery reads back. The write side used to live here; this pins that moving
// it did not change the bytes on the registry.
func TestDiscoverSignatureBundle_ReadsClientkitAttach(t *testing.T) {
	store := memory.New()
	subject := subjectManifest(t, store)
	want := []byte(`{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json","the":"bundle"}`)

	require.NoError(t, ckbundle.AttachReferrer(context.Background(), store, subject, mediatype.SigstoreBundle, want))
	// A provenance referrer beside it must NOT be picked up as the signature.
	require.NoError(t, ckbundle.AttachReferrer(context.Background(), store, subject, ckbundle.ProvenanceArtifactType, []byte(`{"the":"provenance"}`)))

	refs, err := registry.Referrers(context.Background(), store, subject, "")
	require.NoError(t, err)
	require.Len(t, refs, 2)

	got, err := discoverSignatureBundle(context.Background(), store, subject)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestDiscoverSignatureBundle_FindsCosignReferrer(t *testing.T) {
	store := memory.New()
	subject := subjectManifest(t, store)
	want := []byte(`{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json","the":"bundle"}`)
	// grcli's catalog signatures are attached with the cosign-sign artifactType,
	// with the v0.3 bundle blob as the layer.
	attachSignature(t, store, subject, mediatype.CosignSignReferrer, mediatype.SigstoreBundle, want)

	got, err := discoverSignatureBundle(context.Background(), store, subject)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestDiscoverSignatureBundle_UnsignedReturnsNil(t *testing.T) {
	store := memory.New()
	subject := subjectManifest(t, store)
	// No referrer attached.
	got, err := discoverSignatureBundle(context.Background(), store, subject)
	require.NoError(t, err)
	require.Nil(t, got, "no signature referrer → nil bundle (the verifier maps nil to ErrUnsigned)")
}

// TestDiscoverSignatureBundle_FindsSigstoreBundleArtifactType pins the cosign
// 3.x stamp variant: cosign 3.x signs with the bundle format by default and
// attaches the referrer with artifactType SigstoreBundle (not the 2.6.x-era
// CosignSignReferrer). Discovery must accept both — this exact miss (filtering
// on CosignSignReferrer only) made the first live cosign-3.x-signed catalog
// verify as "no signature attached" (2026-07-07). Supersedes the old
// "do not cross these" guard, whose premise predates cosign 3.x.
func TestDiscoverSignatureBundle_FindsSigstoreBundleArtifactType(t *testing.T) {
	store := memory.New()
	subject := subjectManifest(t, store)
	want := []byte(`{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json","the":"bundle"}`)
	attachSignature(t, store, subject, mediatype.SigstoreBundle, mediatype.SigstoreBundle, want)

	got, err := discoverSignatureBundle(context.Background(), store, subject)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

// TestDiscoverSignatureBundle_IgnoresUnrelatedArtifactType: a referrer that is
// neither signature stamp variant (e.g. an SBOM attachment) must not be
// mistaken for a signature.
func TestDiscoverSignatureBundle_IgnoresUnrelatedArtifactType(t *testing.T) {
	store := memory.New()
	subject := subjectManifest(t, store)
	attachSignature(t, store, subject, "application/spdx+json", "application/spdx+json", []byte(`{"sbom":"x"}`))

	got, err := discoverSignatureBundle(context.Background(), store, subject)
	require.NoError(t, err)
	require.Nil(t, got, "a non-signature referrer must not match signature discovery")
}

// TestDiscoverSignatureBundle_MalformedReferrerErrors confirms a cosign-typed
// referrer with no Sigstore bundle layer is a hard error (present-but-malformed),
// never silently treated as unsigned.
func TestDiscoverSignatureBundle_MalformedReferrerErrors(t *testing.T) {
	store := memory.New()
	subject := subjectManifest(t, store)
	// Right artifactType, but the layer is some other media type — no bundle.
	attachSignature(t, store, subject, mediatype.CosignSignReferrer, "application/octet-stream", []byte("not-a-bundle"))

	_, err := discoverSignatureBundle(context.Background(), store, subject)
	require.Error(t, err)
	require.Contains(t, err.Error(), "carries no")
}
