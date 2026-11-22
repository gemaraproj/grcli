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

// TestUnpack_TamperedCacheEntryIsRefusedAndDropped walks a rewritten cache
// entry through the real cache read: its own digest checks pass (meta.json was
// rewritten too), the pin to the verified manifest refuses it, and
// dropCachedPrimary removes it so the next run pulls afresh.
func TestUnpack_TamperedCacheEntryIsRefusedAndDropped(t *testing.T) {
	ctx := context.Background()
	c := tempCache(t)
	const url = "https://hub.invalid.test"

	store := memory.New()
	published := &bundle.Bundle{
		Manifest: bundle.Manifest{BundleVersion: "1.0"},
		Source:   bundle.File{Name: "controls.yaml", Data: []byte("id: real\n")},
	}
	desc, err := bundle.Pack(ctx, store, published)
	require.NoError(t, err)
	require.NoError(t, store.Tag(ctx, desc, desc.Digest.String()))

	tampered := &bundle.Bundle{
		Manifest: published.Manifest,
		Source:   bundle.File{Name: "controls.yaml", Data: []byte("id: evil\n")},
		Etag:     desc.Digest.Hex(),
	}
	putBundle(c, hostOf(url), "acme", "controls", "1.0.0", tampered, io.Discard)

	v := viper.New()
	v.Set(flagURL, url)
	v.Set(flagRepository, "acme/controls")
	v.Set(flagVersion, "1.0.0")
	v.Set(flagCacheEnabled, true)
	b, _, err := resolveBundle(ctx, v, io.Discard) // offline, so this is the cache hit
	require.NoError(t, err)
	require.Equal(t, "id: evil\n", string(b.Source.Data))

	require.ErrorContains(t, registry.CheckBundle(ctx, store, desc.Digest.String(), b), "content does not match")

	dropCachedPrimary(v, io.Discard)
	_, found, err := c.Get(hostOf(url), "acme", "controls", "1.0.0")
	require.NoError(t, err)
	require.False(t, found, "the tampered entry must be gone")
}
