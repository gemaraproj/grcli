# Architecture

grcli is a single Go binary of about 5,900 lines (without tests), built with
cobra and viper. It has no daemon, no state beyond a config file, a
credential file and a cache, and it shells out to only two external tools:
`cue` (for `validate`) and `cosign` (for the key-based paths).

## Layers

```
main.go ─ cmd/ ──────────────── one file per command, plus shared glue
            │   root.go          config loading, the clientkit App identity
            │   fetch.go         the cache-first fetch shared by cat and unpack
            │   regtoken.go      "make sure a registry token is exported"
            │   urldefault.go    the default hub URL and its conflict rule
            │   reflint.go       reference warnings shared by validate and publish
            │
          internal/ ──────────── no cobra/viper; testable in isolation
            source       load and merge input YAML, stamp metadata.version
            provenance   build the SLSA-shaped record
            registry     pack/push/pull bundles and signatures (oras-go)
            sign         keyless signing (sigstore-go) and cosign key signing
            sigverify    keyless verification (sigstore-go), identity pinning
            hub          discovery, /v2/token, /v1/catalogs, /v1/bundles/sync
            refs         parse mapping references, decide what to resolve, lint
            cache        on-disk cache of fetched bundles
            digest       sha256 helpers
```

`cmd/` owns every decision that depends on flags or the environment;
`internal/` packages take plain values. The `internal/` packages are meant to
be reused by other grc.store tools, but being `internal`, Go only lets code
inside this module import them.

## What grcli depends on

| Dependency | What grcli takes from it | Scope |
|---|---|---|
| [go-gemara](https://github.com/gemaraproj/go-gemara) | The bundle format (`bundle.Pack`/`Unpack`), Gemara types for reference parsing, multi-file catalog merging. | General |
| `grc-store-protocol` | Every type that crosses the wire to the hub: discovery document, `/v2/token` response, sync request and response, error envelope and codes, staging repository naming, canonical signer identity, reference URL parsing, SPDX parsing, media types, size limits. It holds contract only: nouns and rules, no client code. | Hub protocol |
| [grc-store-clientkit](https://github.com/gemaraproj/grc-store-clientkit) | `auth`: the device-flow login, the credential store, token resolution and refresh, the GitHub Actions OIDC token fetch. `trustroot`: the pinned Sigstore public-good `trusted_root.json`. Shared with privateer-sdk; grcli passes its own `auth.App{Name: "grcli", TokenEnv: "GRCLI_TOKEN", TokenFlag: "--token"}` so it keeps its own credential file and messages. | Hub protocol, Sigstore |
| sigstore-go | Keyless signing (Fulcio, Rekor) and verification. | Sigstore |
| oras-go | All registry I/O, OCI layouts, referrers. The `oras` CLI is used only to install grcli. | General |
| `cue` binary | Schema validation. | General |
| `cosign` binary | `publish --cosign-key`, `verify --cosign-key`. | Sigstore |
| The hub | Registry location, registry tokens, authorization, indexing, signer records, schema enforcement at publish. | Hub protocol |

The hub is the dependency that matters most and is the only one that is not
a library: grcli and the hub agree through `grc-store-protocol` and through
behaviour each side tests on its own. grcli also mirrors one piece of the hub
by copying rather than importing: `internal/sigverify` follows the hub's
verifier so that "verifies on the hub but not in grcli" can only come from a
policy difference.

## Data flow per command

### validate

```
files ─▶ read metadata.type / gemara-version ─▶ lint references ─▶ cue vet (registry module or --spec)
```

Entirely local apart from cue fetching the schema module.

### publish

```
files ─▶ source.Load ─▶ apply --version ─▶ lint refs
     ─▶ resolve target (derive <ns>/<id>; discover registry)
     ─▶ check --license
     ─▶ sign.Preflight ─▶ version pre-check (hub) ─▶ push token (hub /v2/token)
     ─▶ provenance.Build ─▶ registry.PushRemote to staging/<ns>/<id>
     ─▶ sign.Sign (keyless: Fulcio+Rekor, attach referrer | key: cosign)
     ─▶ hub.Sync(<ns>/<id>, tag)
```

Everything that can fail cheaply happens before the first byte is pushed, so
a bad license, a missing signing credential or a taken version never leaves
anything in the registry. The hub then re-checks everything that matters
(namespace, tag/version, signature, signer pin, licence, schema) at sync:
grcli's checks are for a fast, clear failure, the hub's are the enforcement.

### verify

```
flags ─▶ pick mode ─▶ discover registry ─▶ (zero-flag: hub signer record)
      ─▶ anonymous pull token ─▶ resolve tag to digest, find signature referrer
      ─▶ sigverify against digest + identity
```

### unpack and cat

```
flags ─▶ cache lookup ─hit─▶ bundle
                     └miss─▶ discover ─▶ anonymous pull token ─▶ pull ─▶ cache put ─▶ bundle
unpack: bundle ─▶ verify (as above) ─▶ write files + bundle.json ─▶ (--with-*) resolve references
cat:    bundle ─▶ write the artifact to stdout
```

References are resolved one level deep, each through the same cache, with a
fresh pull token per repository, and never signature-verified.

### login and logout

```
login:  discover (oidc_issuer, oidc_cli_client_id) ─▶ OIDC metadata ─▶ device flow ─▶ store
logout: discover issuer (or --issuer) ─▶ delete stored entry
```

## How a token reaches the registry

grcli never answers the registry's `WWW-Authenticate` challenge itself.
Before any registry call it asks the hub's `/v2/token` for a token scoped to
one repository and exports it as `GRCLI_REGISTRY_TOKEN`. The oras client's
credential function reads that variable on every request, and the key-mode
cosign subprocess gets it as `--registry-token`. A user-supplied
`GRCLI_REGISTRY_TOKEN` or username/password pair always wins, and with
neither the Docker credential store is the fallback.

Using a process-wide environment variable as the hand-off is why reference
resolution has to mint and overwrite the variable per repository, and why
`unpack` records whether the user set a credential before anything else
runs.

## Tests

Every command has a `_test.go` in `cmd/`; those that need a hub use
`httptest` fakes. `cmd/integration_test.go` runs
`publish --dry-run` into a local layout and `unpack` back out. No test
publishes to a real hub or registry. `internal/`
packages are unit-tested, with a sign-then-verify round trip in
`internal/sigverify` and a fuzz test for reference parsing. `validate` tests
skip without `cue` or network locally and fail in CI.
