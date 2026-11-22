// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"io"
	"testing"

	"github.com/gemaraproj/go-gemara/bundle"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
	"oras.land/oras-go/v2/content/memory"

	"github.com/gemaraproj/grcli/internal/cache"
	"github.com/gemaraproj/grcli/internal/registry"
)

// TestPlanUnpackVerify pins the pre-network gating decision: remote
// unpack verifies by default, --no-verify opts out, and a local --source layout
// can never be verified (and that reason wins even when --no-verify is also set).
func TestPlanUnpackVerify(t *testing.T) {
	cases := []struct {
		name     string
		source   string
		noVerify bool
		want     unpackVerifyPlan
	}{
		{"remote default verifies", "", false, unpackVerify},
		{"no-verify opts out", "", true, unpackSkipNoVerify},
		{"source cannot be verified", "./layout", false, unpackSkipSource},
		{"source wins over no-verify", "./layout", true, unpackSkipSource},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := planUnpackVerify(tc.source, tc.noVerify); got != tc.want {
				t.Fatalf("planUnpackVerify(%q, %v) = %d, want %d", tc.source, tc.noVerify, got, tc.want)
			}
		})
	}
}

// TestUnpackReusesVerifyTrustFlags confirms unpack registers the keyless trust
// flags it shares with `grcli verify`, so an unpack can assert its own identity
// (bypassing the hub) exactly as verify does. A missing flag here would make
// verifyBeforeUnpack silently fall back to zero-flag mode and ignore the
// caller's asserted identity.
func TestUnpackReusesVerifyTrustFlags(t *testing.T) {
	cmd := newUnpackCmd(viper.New())
	for _, name := range []string{flagNoVerify, flagCertIdentity, flagCertOIDCIssuer} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("unpack is missing the --%s flag", name)
		}
	}
}

// TestPinVerified walks a cache entry whose bytes differ from the verified
// manifest through the real cache read (the cache's own digest checks pass)
// and into pinVerified, which unpack calls after the signature check: content
// that does not match is refused and dropped, while a registry error refuses
// without touching the cache.
func TestPinVerified(t *testing.T) {
	ctx := context.Background()
	const url = "https://hub.invalid.test"

	store := memory.New()
	published := &bundle.Bundle{
		Manifest: bundle.Manifest{BundleVersion: "1.0"},
		Source:   bundle.File{Name: "controls.yaml", Data: []byte("id: real\n")},
	}
	desc, err := bundle.Pack(ctx, store, published)
	require.NoError(t, err)
	require.NoError(t, store.Tag(ctx, desc, desc.Digest.String()))

	seed := func(t *testing.T) (*cache.Cache, *viper.Viper, *bundle.Bundle) {
		t.Helper()
		c := tempCache(t)
		putBundle(c, hostOf(url), "acme", "controls", "1.0.0", &bundle.Bundle{
			Manifest: published.Manifest,
			Source:   bundle.File{Name: "controls.yaml", Data: []byte("id: other\n")},
			Etag:     desc.Digest.Hex(),
		}, io.Discard)
		v := viper.New()
		v.Set(flagURL, url)
		v.Set(flagRepository, "acme/controls")
		v.Set(flagVersion, "1.0.0")
		v.Set(flagCacheEnabled, true)
		b, _, err := resolveBundle(ctx, v, io.Discard) // offline, so this is the cache hit
		require.NoError(t, err)
		require.Equal(t, "id: other\n", string(b.Source.Data))
		return c, v, b
	}
	cached := func(t *testing.T, c *cache.Cache) bool {
		t.Helper()
		_, found, err := c.Get(hostOf(url), "acme", "controls", "1.0.0")
		require.NoError(t, err)
		return found
	}

	t.Run("mismatch is refused and dropped", func(t *testing.T) {
		c, v, b := seed(t)
		err := pinVerified(ctx, v, store, desc.Digest.String(), b, io.Discard)
		require.ErrorIs(t, err, registry.ErrContentMismatch)
		require.ErrorContains(t, err, "refusing to unpack")
		require.False(t, cached(t, c), "the mismatched entry must be gone")
	})

	t.Run("registry error is refused and keeps the entry", func(t *testing.T) {
		c, v, b := seed(t)
		err := pinVerified(ctx, v, memory.New(), desc.Digest.String(), b, io.Discard)
		require.Error(t, err)
		require.NotErrorIs(t, err, registry.ErrContentMismatch)
		require.True(t, cached(t, c), "a transport failure must not drop the entry")
	})

	t.Run("matching content passes", func(t *testing.T) {
		v := viper.New()
		good := &bundle.Bundle{Source: published.Source}
		require.NoError(t, pinVerified(ctx, v, store, desc.Digest.String(), good, io.Discard))
		require.Equal(t, "1.0", good.Manifest.BundleVersion)
	})
}
