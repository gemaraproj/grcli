# grcli — agent orientation

Go CLI and **primary end-user surface** for grc.store: validates Gemara YAML, packs it into
signed OCI bundles with SLSA-shaped provenance, publishes to a hub, and verifies bundles.
Go module and repo: `github.com/gemaraproj/grcli`; releases publish to `ghcr.io/gemaraproj/grcli`.

`README.md` covers install (via `oras`), the full usage flow, and CI/trusted-publishing;
`CHANGELOG.md` tracks the pre-1.0 breaking changes. This file is the map — point there, don't duplicate.

> **Building new end-user tooling? Reuse this, don't fork it.** The `internal/` packages below
> are the intended reuse surface, and the wire types come from
> `github.com/revanite-io/grc-store-protocol`.

## Dev loop (Makefile)
- `make build` → `bin/grcli` · `make test` (`./...`) · `make lint` (golangci-lint) · `make vet`
- `make ci-local` — fmtcheck + vet + lint + tidycheck + testcov (the CI gate)

## Commands (`cmd/`)
`login`/`logout` (OIDC device flow, credential storage) · `validate` (YAML vs Gemara spec via
`cue vet`) · `publish` (pack + sign + push OCI bundle) · `verify` (cosign / Sigstore bundle;
zero-flag mode verifies against the hub-recorded signer identity) ·
`unpack` (verify signature fail-closed — `--no-verify`/`--source` skip — then extract to a directory
from OCI layout or registry; reuses verify's policy path) · `cat` (stream Gemara content to
stdout, no files — read-only companion to `unpack`) · `versions <ns>/<id>` (list
published versions). Registered in `cmd/root.go`; one file per command (`publish.go`, `verify.go`,
…). `unpack` and `cat` share the cache-checking fetch stage in `fetch.go` (`resolveBundle`) and
differ only in the last mile. (`regtoken.go` is an internal helper — `ensureRegistryToken()` — not
a user command.)

## Reuse surface (`internal/`)
`hub` (`/v1/bundles/sync`, `/v2/token`, discovery) · `registry` (OCI packing via `oras-go`) ·
`sign` (cosign shell-out, Sigstore bundle) · `provenance` (SLSA v1.0 predicate) · `source` (load/merge/verify YAML) · `refs` ·
`digest` (SHA256) · `cache` (immutable-tag disk cache — v2 stores the whole bundle: files +
`bundle.json`). Imports `grc-store-protocol` (discovery, syncapi, registrytoken, spdx).
**Auth is no longer here**: `internal/auth` (device flow, credential store, token resolution, GHA
OIDC) was deleted in favour of `github.com/gemaraproj/grc-store-clientkit/auth`, shared with
privateer-sdk. `cmd.grcliApp` (`cmd/root.go`) is the per-tool identity that keeps grcli's own
credential file and login hints — pass it to every clientkit auth call.

## Gotchas
- **External tools on PATH**: `cosign` ≥ 2.6.0 is needed **only** for key-based signing (`publish --cosign-key`) and key-based `verify --cosign-key`; `cue` for `validate`. **Keyless signing AND verifying are in-process — no cosign**, both via the embedded `sigstore-go`. Keyless `publish` resolves its identity (`SIGSTORE_ID_TOKEN` > GHA OIDC > browser) and signs through `grc-store-clientkit/keyless` + `clientkit/bundle` (DSSE in-toto signature referrer plus a signed SLSA provenance referrer) — the same code `pvtr` uses; override `GRC_STORE_FULCIO_URL`/`GRC_STORE_REKOR_URL` for a private Sigstore. The whole publish side (discovery, registry tokens, pack/push, sync) lives in clientkit; this repo keeps the read side (`internal/registry`, `internal/hub`) and the cosign shell-out for `--cosign-key`. When cosign IS used (key mode), grcli gates its bundle-format flag on the detected version (`internal/sign.BundleFormatArgs`: `--new-bundle-format` on 2.6–2.x, omitted ≥ 3.x, fail-fast below 2.6.0). The verify side mirrors the hub's `internal/sigverify`; the pinned trust root now comes from `grc-store-clientkit/trustroot` (rotate it there and re-tag the module — it is no longer vendored here), or point `GRCLI_TRUSTED_ROOT` at one. Catalog signatures are discovered as OCI referrers accepting BOTH signature stamp variants — `mediatype.CosignSignReferrer` (cosign 2.6.x) and `mediatype.SigstoreBundle` (cosign 3.x default) — since the stamp follows the signer's cosign major, not the artifact kind (the old "do not cross these" rule predated cosign 3.x; see the mediatype doc). OCI transport uses the `oras-go` **library**, not the `oras` CLI — `oras` is only needed to *install* grcli (see README), not to run it.
- **Signing is on by default**; `--no-sign` to opt out.
- **Caching**: remote `unpack`/`cat` fetches (and resolved references) are served from a global on-disk cache at `$GRCLI_CACHE` (default `os.UserCacheDir()/grcli`); a hit needs no network for the *content* (immutable tags → never stale). No GC yet. `--no-cache` per run; `cache-enabled: false` to disable durably. Caveat: a default `unpack` still hits the hub/registry to *verify* even on a content cache hit — `--no-verify` restores a fully offline hit.
- Config: flag > `GRCLI_*` env > user-global `$XDG_CONFIG_HOME/grcli/config.yaml` (→ `~/.config/grcli/config.yaml`) > default. **No per-project layer** — a repo-local `./.grcli.yaml` is not read (a committed file must not steer a publish/verify tool) and earns a migration warning. `--config <file>` bypasses the search. The cache toggle key is flat `cache-enabled` (not nested `cache.enabled`) because `$GRCLI_CACHE` shadows the `cache.*` viper namespace. Env prefix `GRCLI_*` (e.g. `GRCLI_TOKEN`, `GRCLI_GEMARA_SPEC_DIR`); registry and Sigstore overrides are the cross-tool `GRC_STORE_*` family (`GRCLI_REGISTRY_*` still read with a deprecation warning). `grcli verify`'s `--certificate-oidc-issuer` defaults to GitHub Actions. (Neither `./.grcli.yaml` nor `$HOME/.grcli.yaml` is read.)
- Credentials stored at `$XDG_DATA_HOME/grcli/credentials.json` (0600).
- CI publishing uses GitHub Actions OIDC (`ACTIONS_ID_TOKEN_REQUEST_URL/_TOKEN`); example at `examples/github-actions/publish.yml`.
- **grcli defaults to the *prod* hub** — for test publishing pass `--url https://hub.preview.grc.store` (or set `GRCLI_URL`).
