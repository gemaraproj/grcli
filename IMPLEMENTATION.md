# Implementation plan: `grcli cat`, primary-artifact cache, and user-global config

Tracks the work for **ADR-0042** (`cat` + cache the primary/whole bundle) and **ADR-0043**
(user-global config file), both in `../grc.store-backend/docs/adr/`. This is grcli-only — no
backend, hub, or `grc-store-protocol` change. ADRs flip `Proposed → Accepted` on merge.

## Decisions locked in (see the ADRs for rationale)

- **`cat` streams Gemara content only** — the artifact file(s), which carry the Gemara
  `metadata:` block. No `bundle.json`/manifest/provenance via `cat` (no `--manifest` flag);
  bundle information is `unpack`'s job.
- **Cache stores the complete decoded bundle** — `bundle.json` (from `bundle.Manifest`) + every
  `bundle.Files` entry, each with a per-file content digest. Not the raw OCI layout, not the
  cosign signature — so `verify` still hits the network (verify-on-pull is deferred, ADR-0039).
- **`cat` and `unpack` share the fetch stage, diverge only at the last mile.** One helper does
  resolve-source → cache-check → fetch-on-miss → cache-put → return the in-memory bundle.
  `unpack` then writes the dir (`writeBundle`); `cat` then streams `Files`. Neither does the
  other's last mile.
- **References use the same full-bundle format via the registry (ADR-0042 decision 5, option
  (a)).** Reference resolution moves off `hub.GetVersionBody` onto a registry pull, so each
  referenced repo needs its own pull token (`ensureRegistryToken`) and a repo-path derivation
  from `{ns}/{id}`. One uniform cache format; the token/plumbing cost is accepted.
- **`--no-cache`** (hyphenated, existing flag) bypasses the cache on both commands.
  **`cache-enabled`** (config, default true) is the durable off switch. `$GRCLI_CACHE` location
  override is retained; there is no cache-*location* config key.
- **Config precedence via viper merge, not first-match** — flag > `GRCLI_*` env > project
  `./.grcli.yaml` > user-global `$XDG_CONFIG_HOME/grcli/config.yaml` > default. Fixes the phantom
  `config.yaml` path (today `loadConfig` searches `.grcli.yaml` in the XDG dir).

## Grounding (verified against current source)

- `registry.UnpackRemote(ctx, host, repo, tag) (*bundle.Bundle, error)` → `bundle.Unpack`; the
  bundle carries `Files []File`, `Manifest`, `Etag` (OCI manifest digest). `UnpackLocal` is the
  `--source` twin. (`internal/registry/registry.go`)
- `writeBundle` (`cmd/unpack.go`) builds `bundle.json` via `json.MarshalIndent(b.Manifest, …)` —
  reconstructed, so the cache can store the manifest and reproduce it.
- References today: `fetchReference` (`cmd/unpack.go`) uses `hub.GetVersionBody` +
  `hub.GetCatalog` (for license/manifest-digest). Option (a) replaces the body fetch with a
  registry pull.
- Cache today: `internal/cache/cache.go` — `Entry{ Body []byte; … }`, single `body.<ext>` +
  `meta.json`, `layoutVersion = "v1"`, host-namespaced `entryDir`. Needs the multi-file change.
- Config today: `loadConfig` (`cmd/root.go`) — `SetConfigName(".grcli")` + `AddConfigPath`,
  first-match-wins (`ReadInConfig`), `GRCLI` env prefix. Needs `MergeInConfig` + explicit paths.

## Phases

### Phase 1 — Cache `v2` multi-file entry format  *(independent; land first)*
`internal/cache/cache.go`, `internal/cache/cache_test.go`
- Replace single `Body` with a bundle entry: `Files []struct{Name, Digest string; Data []byte}`
  + `Manifest []byte` (the `bundle.json` bytes) + existing `ManifestDigest`/`License`/
  `SourceURL`/`Verified`.
- On disk: `meta.json` + `bundle.json` + `files/<name>`; per-file digest computed on `Put`,
  verified on `Get` (corruption → `found=false` + error, as today).
- Bump `layoutVersion` `v1` → `v2` (no migration — `v1` dirs are simply never read).
- Tests: multi-file round-trip, per-file corruption, host-namespacing preserved, `v1` ignored.

### Phase 2 — Shared cache-checking fetch + wire `unpack` to it  *(depends on P1)*
`cmd/unpack.go` (+ maybe a small helper file)
- Add `resolveBundle(ctx, v, src, url, repo, version) (*bundle.Bundle, error)`: source
  resolution → `cache.Get` → `UnpackRemote`/`UnpackLocal` on miss → `cache.Put` (remote only) →
  return bundle. `--source` skips cache but flows through the helper. `--no-cache` +
  `cache-enabled` gate caching inside the helper.
- `runUnpack` remote branch calls `resolveBundle` instead of `UnpackRemote` directly; last mile
  stays `writeBundle`. Extend `--no-cache` to cover the primary (today it only gates references).
- Tests: cache hit avoids network, `--no-cache` bypasses, `--source` uncached, corrupt entry
  re-fetches.

### Phase 3 — `grcli cat` command  *(depends on P2)*
new `cmd/cat.go`, register in `cmd/root.go`
- Flags: `--source` / `--url` + `--repository` / `--version` (reuse `unpack`'s selectors and
  `suppressDefaultURLIfExplicit`), `--file <name>`, `--no-cache`. No `--output`, no `--with-*`,
  no `--manifest`. Mint an anonymous pull token for `--url` reads (as `unpack` does).
- Fetch via `resolveBundle`; last mile: single file verbatim, multi-file as `---` YAML stream,
  `--file` selects one.
- Tests: `cat_test.go` (single, multi-file stream, `--file`, `--source` path, cache hit) +
  integration coverage à la `cmd/integration_test.go`.

### Phase 4 — Reference resolution onto the registry/full-bundle path  *(depends on P1)*
`cmd/unpack.go` (`fetchReference`, `resolveReferences`)
- Replace the `hub.GetVersionBody` fetch with a registry pull for each reference: derive the
  repository from `{ns}/{id}`, `ensureRegistryToken(..., []string{"pull"})` per referenced repo,
  `UnpackRemote`, store as a `v2` entry. Keep the license/manifest-digest recording and the
  license-mismatch warning. Reference *output* under `references/<category>/…` is unchanged.
- Tests: reference cache hit/miss, per-repo token minted, license warning preserved. Also close
  the pre-existing zero-coverage gap on `resolveReferences`/`fetchReference` surfaced in Phase 1
  QA — this path had no tests through Phase 1 and must not land Phase 4 untested.

### Phase 5 — User-global config  *(independent; can run in parallel with P1–P4)*
`cmd/root.go` (`loadConfig`), all command RunEs
- Switch first-match to explicit merge: read user-global
  `$XDG_CONFIG_HOME/grcli/config.yaml` (fallback `$HOME/.config/grcli/config.yaml`), then
  `MergeInConfig` the project `./.grcli.yaml` on top; `--config` still selects a single file.
  Fixes the phantom `config.yaml` path.
- Bind `cache-enabled` (default true); shared `cachingEnabled(v)` helper =
  `cache-enabled && !--no-cache`, consumed by `resolveBundle` and the reference path.
- Tests: precedence (flag > env > project > global > default), `cache-enabled:false` ⇒ no cache
  I/O, merge (global honored when project file present).

### Phase 6 — Docs + ADR status
- `README.md`: `cat` command, cache behavior (`$GRCLI_CACHE`, `--no-cache`, unbounded/no-GC),
  config file + precedence.
- `CLAUDE.md`: add `cat` to Commands; correct the config section (`config.yaml` is the real
  user-global path; document precedence). Note the cache now covers the primary.
- `CHANGELOG.md`: breaking — `unpack` now consults a cache for the primary; `.grcli.yaml`
  precedence change (global file now merges under project).
- Flip ADR-0042 / ADR-0043 to `Accepted`.

## Sequencing & PRs
- Critical path: **P1 → P2 → P3**. **P4** depends on P1. **P5** is fully independent.
- Suggested PRs: (1) cache v2, (2) shared fetch + unpack + cat, (3) reference migration,
  (4) config. Or bundle 1–3 if reviewed together.
- Gate every PR on `make ci-local` (fmtcheck + vet + lint + testcov).

## Risks / watch-items
- **Behavior breaks (accepted, ~no users):** `unpack` primary now cached; `.grcli.yaml` no
  longer shadows the home file.
- **Unbounded cache** grows faster now (primary + full-bundle references). `grcli cache clean` /
  eviction remains the named ADR-0039 follow-up — out of scope here but more pressing.
- **Per-reference pull tokens (Phase 4)** add hub round-trips when resolving many references;
  watch latency on large dependency sets.
- **`verify` is not cache-served** — intentional; raw-OCI-layout storage is the documented
  upgrade path if offline verify is ever needed.
