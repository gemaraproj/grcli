# Changelog

Notable changes to `grcli`. This project is pre-1.0; while on `v0.x`, a breaking
change bumps the minor version.

## [Unreleased]

### Added

- **`grcli cat`** — prints an artifact's Gemara content to stdout without writing
  files, the read-only companion to `unpack`. Emits the artifact file(s) only
  (never `bundle.json`/manifest/provenance); a single-file bundle prints verbatim,
  a multi-file bundle as a `---`-separated YAML stream (`--file <name>` selects
  one). Diagnostics go to stderr, so `grcli cat … | yq …` stays pipe-clean.
- **On-disk artifact cache for remote fetches.** A remote (`--url`) `unpack`/`cat`
  of a `namespace/id/version` stores the whole bundle at `$GRCLI_CACHE` (default
  `os.UserCacheDir()/grcli`); repeat fetches — and references to the same
  coordinate — are served offline (immutable tags make a hit always fresh).
  `--no-cache` bypasses it for one run; no eviction/GC yet.
- **User-global config file** `$XDG_CONFIG_HOME/grcli/config.yaml`
  (`~/.config/grcli/config.yaml`), merged **under** the per-project `./.grcli.yaml`.
  New key `cache-enabled: false` (`GRCLI_CACHE_ENABLED`) durably disables the cache.

### Changed — BREAKING

- **`unpack` now consults the cache for the primary artifact.** A remote `unpack`
  that previously always hit the network now serves a warm coordinate from the
  cache (and skips discovery entirely on a hit). Use `--no-cache` for the old
  always-fresh behavior.
- **Resolved references are now written as a directory, not a flat file.**
  `--with-imports`/`--with-references` previously wrote each reference as
  `references/<category>/<ns>/<id>@<version>.json` (the hub's JSON projection); it
  is now a directory `references/<category>/<ns>/<id>@<version>/` containing the
  artifact's original YAML file(s) + `bundle.json`, and `references/index.json`'s
  `path` points at that directory.
- **Config precedence changed and `$HOME/.grcli.yaml` is dropped.** Config is now
  layered (flag > `GRCLI_*` env > project `./.grcli.yaml` > user-global
  `config.yaml` > default) with the project file merged over the global one,
  replacing first-match-wins. The old home-root dotfile `~/.grcli.yaml` is no
  longer read — **move it to `~/.config/grcli/config.yaml`.** (The previously
  *advertised-but-nonfunctional* `$XDG_CONFIG_HOME/grcli/config.yaml` now works.)

- **Catalog signatures now use the Sigstore bundle format** — `grcli` signs (and
  verifies) with cosign's `--new-bundle-format`, attaching the signature as an OCI
  1.1 referrer of the bundle, instead of the legacy tag-based `sha256-….sig`. This
  converges grc.store on one signature format (the hub's plugin path already uses
  it).
  - **Migration:** a catalog signed by this version will **not** verify with an
    older `grcli verify`, and a catalog signed by an older `grcli` will **not**
    verify with this version. **Re-publish existing catalogs to re-sign them in the
    bundle format.**
  - To verify a catalog manually, use `cosign verify --new-bundle-format …` (not
    bare `cosign verify`).
  - **New requirement:** `cosign` >= 3.x on `PATH`.

### Internal

- Adopt the shared `github.com/revanite-io/grc-store-protocol` module for the
  discovery / sync / registry-token wire types (no behavior change; wire-identical).
