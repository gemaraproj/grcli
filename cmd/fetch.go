// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/gemaraproj/go-gemara/bundle"
	"github.com/spf13/viper"

	"github.com/gemaraproj/grcli/internal/cache"
	"github.com/gemaraproj/grcli/internal/hub"
	"github.com/gemaraproj/grcli/internal/registry"
)

// resolveBundle fetches the primary artifact bundle from either a local OCI
// layout (--source) or the remote registry discovered from the hub (--url +
// --repository + --version). It is the shared fetch stage for `unpack` and
// `cat`: both run this identical resolve-and-cache
// pipeline and then diverge only in how they render the returned bundle.
//
// Remote fetches consult the on-disk cache (unless caching is disabled) keyed
// by the same (host, ns, id, version) coordinate reference resolution uses, so
// a primary and a reference to the same artifact share one entry. The cache is
// checked BEFORE hub discovery, so a cache hit needs no network at all
// (served offline). --source reads are local bytes and never cached.
//
// diag receives human-readable cache diagnostics (never artifact content), so a
// caller that emits machine-readable content on stdout — `cat` — must pass a
// separate stream (stderr). `unpack`, whose stdout is already a progress log,
// passes that log writer.
//
// The caller is responsible for having bound flags and suppressed the --url
// default (suppressDefaultURLIfExplicit) before calling.
func resolveBundle(ctx context.Context, v *viper.Viper, diag io.Writer) (b *bundle.Bundle, label string, err error) {
	source := v.GetString(flagSource)
	url := v.GetString(flagURL)
	repository := v.GetString(flagRepository)
	version := v.GetString(flagVersion)

	if version == "" {
		return nil, "", errors.New("--version is required")
	}
	switch {
	case source == "" && url == "":
		return nil, "", errors.New("either --source or --url is required")
	case source != "" && url != "":
		return nil, "", errors.New("--source is mutually exclusive with --url")
	}

	if source != "" {
		b, err = registry.UnpackLocal(ctx, source, version)
		return b, source, err
	}

	if repository == "" {
		return nil, "", errors.New("--repository is required when --url is set")
	}

	// Cache lookup FIRST — before any network. The coordinate mirrors reference
	// resolution: host from the hub --url, ns/id from the repository path.
	host := hostOf(url)
	ns, id := splitRepository(repository)
	var c *cache.Cache
	if cachingEnabled(v) && host != "" && ns != "" && id != "" {
		if cc, cerr := cache.Open(); cerr != nil {
			fmt.Fprintf(diag, "  ! cache unavailable, fetching without it: %v\n", cerr)
		} else {
			c = cc
		}
	}
	if c != nil {
		if e, found, gerr := c.Get(host, ns, id, version); gerr != nil {
			fmt.Fprintf(diag, "  ! cache: %v (re-fetching)\n", gerr)
		} else if found {
			cached, berr := bundleFromEntry(e)
			if berr != nil {
				fmt.Fprintf(diag, "  ! cache: %v (re-fetching)\n", berr)
			} else {
				// Served from cache; no discovery needed, so label from the
				// requested coordinate rather than the (unqueried) registry host.
				return cached, host + "/" + repository, nil
			}
		}
	}

	// Cache miss: discover the registry, mint a token (the registry requires
	// one even for public reads), and pull.
	d, derr := hub.Discover(ctx, url)
	if derr != nil {
		return nil, "", fmt.Errorf("hub discovery: %w", derr)
	}
	// Keep the advertised scheme: registryHost is the oras dial target and
	// newRemoteRepo derives PlainHTTP from it, so stripping http:// here would
	// force HTTPS against a plain-HTTP zot.
	registryHost := d.RegistryURL
	// Label with the requested hub coordinate — the SAME label a cache hit
	// prints — so repeated runs of one command read identically whether served
	// from cache or the registry. Fall back to the registry host only when the
	// hub host can't be parsed.
	label = host + "/" + repository
	if host == "" {
		label = registry.NormalizeRegistryHost(registryHost) + "/" + repository
	}

	if _, terr := ensureRegistryToken(ctx, url, "", repository, []string{"pull"}); terr != nil {
		return nil, "", fmt.Errorf("fetching registry pull token: %w", terr)
	}
	b, err = registry.UnpackRemote(ctx, registryHost, repository, version)
	if err != nil {
		return nil, "", err
	}
	if c != nil {
		putBundle(c, host, ns, id, version, b, diag)
	}
	return b, label, nil
}

// cachingEnabled reports whether the artifact cache should be used: the durable
// cache-enabled preference (default true) AND the absence of the
// per-invocation --no-cache flag. Either one off disables caching.
func cachingEnabled(v *viper.Viper) bool {
	return v.GetBool(flagCacheEnabled) && !v.GetBool(flagNoCache)
}

// bundleFromEntry reconstructs an in-memory bundle from a cache entry. The
// manifest bytes are the JSON writeBundle emits as bundle.json, so unmarshaling
// then re-marshaling reproduces byte-identical output. File.Type and the
// dormant Imports slot are not cached (neither renderer uses Type, and a bundle
// carrying Imports is never cached — see putBundle).
func bundleFromEntry(e *cache.Entry) (*bundle.Bundle, error) {
	b := &bundle.Bundle{Etag: e.ManifestDigest}
	for _, f := range e.Files {
		b.Files = append(b.Files, bundle.File{Name: f.Name, Data: f.Data})
	}
	if len(e.Manifest) > 0 {
		if err := json.Unmarshal(e.Manifest, &b.Manifest); err != nil {
			return nil, fmt.Errorf("decoding cached manifest: %w", err)
		}
	}
	return b, nil
}

// entryFromBundle builds a cache entry from a pulled bundle plus optional hub
// metadata (license, and the reference source URL). The manifest is stored as
// the exact bytes writeBundle emits as bundle.json, so a round trip reproduces
// byte-identical output. It does not persist anything.
func entryFromBundle(b *bundle.Bundle, license, sourceURL string) (cache.Entry, error) {
	e := cache.Entry{ManifestDigest: b.Etag, License: license, SourceURL: sourceURL}
	for _, f := range b.Files {
		e.Files = append(e.Files, cache.File{Name: f.Name, Data: f.Data})
	}
	if !b.Manifest.Empty() {
		mb, err := json.MarshalIndent(b.Manifest, "", "  ")
		if err != nil {
			return cache.Entry{}, fmt.Errorf("encoding manifest: %w", err)
		}
		e.Manifest = mb
	}
	return e, nil
}

// putBundle writes a freshly-pulled primary bundle to the cache. A cache write
// failure is non-fatal (the pull already succeeded). A bundle carrying the
// dormant Imports slot is not cached: the v2 entry format stores Files +
// manifest only, so caching such a bundle would silently drop the
// imports on the next hit — better to leave it uncached and re-pull.
func putBundle(c *cache.Cache, host, ns, id, version string, b *bundle.Bundle, diag io.Writer) {
	if len(b.Imports) > 0 {
		fmt.Fprintf(diag, "  ! not caching %s/%s@%s: bundle carries imports (not stored in cache)\n", ns, id, version)
		return
	}
	e, err := entryFromBundle(b, "", "")
	if err != nil {
		fmt.Fprintf(diag, "  ! cache write skipped (%v)\n", err)
		return
	}
	if err := c.Put(host, ns, id, version, e); err != nil {
		fmt.Fprintf(diag, "  ! cache write failed (continuing): %v\n", err)
	}
}
