# grcli — agent orientation

Go CLI and **primary end-user surface** for grc.store: validates Gemara YAML, packs it into
signed OCI bundles with SLSA-shaped provenance, publishes to a hub, and verifies bundles.
Go module: `github.com/revanite-io/grcli`.

`README.md` covers install (via `oras`), the full usage flow, and CI/trusted-publishing;
`CHANGELOG.md` tracks the pre-1.0 breaking changes. This file is the map — point there, don't duplicate.

> **Building new end-user tooling? Reuse this, don't fork it.** The `internal/` packages below
> are the intended reuse surface, and the wire types come from `../grc-store-protocol`. See the
> reuse map in `../CLAUDE.md`.

## Dev loop (Makefile)
- `make build` → `bin/grcli` · `make test` (`./...`) · `make lint` (golangci-lint) · `make vet`
- `make ci-local` — fmtcheck + vet + lint + testcov (the CI gate)

## Commands (`cmd/`)
`login`/`logout` (OIDC device flow, credential storage) · `validate` (YAML vs Gemara spec via
`cue vet`) · `publish` (pack + sign + push OCI bundle) · `verify` (cosign / Sigstore bundle;
zero-flag mode verifies against the hub-recorded signer identity, ADR-0045) ·
`unpack` (extract to a directory from OCI layout or registry) · `cat` (stream Gemara content to
stdout, no files — read-only companion to `unpack`, ADR-0042) · `versions <ns>/<id>` (list
published versions). Registered in `cmd/root.go`; one file per command (`publish.go`, `verify.go`,
…). `unpack` and `cat` share the cache-checking fetch stage in `fetch.go` (`resolveBundle`) and
differ only in the last mile. (`regtoken.go` is an internal helper — `ensureRegistryToken()` — not
a user command.)

## Reuse surface (`internal/`)
`auth` (RFC 8628 device flow, Keycloak + GitHub Actions OIDC — ADR-0032) · `hub` (`/v1/bundles/sync`,
`/v2/token`, discovery) · `registry` (OCI packing via `oras-go`) · `sign` (cosign shell-out, Sigstore
bundle) · `provenance` (SLSA v1.0 predicate) · `source` (load/merge/verify YAML) · `refs` ·
`digest` (SHA256) · `cache` (immutable-tag disk cache — v2 stores the whole bundle: files +
`bundle.json`, ADR-0042). Imports `grc-store-protocol` (discovery, syncapi, registrytoken, spdx).

## Gotchas
- **External tools on PATH**: `cosign` ≥ 3.x and `cue` (both shell-executed). Wrong cosign = signature failures. OCI transport uses the `oras-go` **library**, not the `oras` CLI — `oras` is only needed to *install* grcli (see README), not to run it.
- **Signing is on by default**; `--no-sign` to opt out.
- **Caching (ADR-0042)**: remote `unpack`/`cat` fetches (and resolved references) are served from a global on-disk cache at `$GRCLI_CACHE` (default `os.UserCacheDir()/grcli`); a hit needs no network (immutable tags → never stale). No GC yet. `--no-cache` per run; `cache-enabled: false` to disable durably.
- Config (ADR-0043, amended by ADR-0044): flag > `GRCLI_*` env > user-global `$XDG_CONFIG_HOME/grcli/config.yaml` (→ `~/.config/grcli/config.yaml`) > default. **No per-project layer** — a repo-local `./.grcli.yaml` is not read (a committed file must not steer a publish/verify tool) and earns a migration warning. `--config <file>` bypasses the search. The cache toggle key is flat `cache-enabled` (not nested `cache.enabled`) because `$GRCLI_CACHE` shadows the `cache.*` viper namespace. Env prefix `GRCLI_*` (e.g. `GRCLI_REGISTRY_TOKEN`, `GRCLI_GEMARA_SPEC_DIR`). `grcli verify`'s `--certificate-oidc-issuer` defaults to GitHub Actions (ADR-0044). (Neither `./.grcli.yaml` nor `$HOME/.grcli.yaml` is read.)
- Credentials stored at `$XDG_DATA_HOME/grcli/credentials.json` (0600).
- CI publishing uses GitHub Actions OIDC (`ACTIONS_ID_TOKEN_REQUEST_URL/_TOKEN`); example at `examples/github-actions/publish.yml`.
- **grcli defaults to the *prod* hub** — for test publishing use `../publish-fixtures/` (forces preview).
