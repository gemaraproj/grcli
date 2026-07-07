# Changelog

Notable changes to `grcli`. This project is pre-1.0; while on `v0.x`, a breaking
change bumps the minor version.

## [Unreleased]

### Fixed

- **The cosign floor for `publish` signing is ≥ 2.6.0, not ≥ 2.4.0 as v0.4.1
  claimed.** cosign added `--new-bundle-format` to `verify` in 2.4.0 but to
  `sign` only in **2.6.0** (confirmed against the release tags' source), so on
  cosign 2.4.x–2.5.x v0.4.1 still died mid-publish on the raw
  `unknown flag: --new-bundle-format` its version gate was built to prevent —
  caught live by a CI publish pinned to cosign v2.5.2. The gate now fails fast
  below 2.6.0 with the corrected floor in the message; cosign ≥ 3.x is
  unaffected (the flag is omitted there entirely).

## [0.4.1] - 2026-07-05

### Fixed

- **`publish` signing no longer hard-codes `--new-bundle-format`, so it works
  across the whole supported cosign range instead of a narrow band.** grcli now
  detects the cosign version (`cosign version --json`) and selects the Sigstore
  bundle-format flag accordingly: it passes `--new-bundle-format` on cosign
  2.4.0–2.x (where the flag is first-class), and omits it on cosign ≥ 3.0.0
  (where the bundle format is already the default and the flag is deprecated).
  This removes the deprecation warning on every sign under cosign 3.x and makes
  grcli forward-compatible with cosign removing the flag. A cosign **below
  2.4.0** now fails fast, before any bytes are pushed, with a clear "needs cosign
  ≥ 2.4.0 — pin a newer cosign" message instead of surfacing cosign's raw
  `unknown flag: --new-bundle-format`. The stated cosign prerequisite drops from
  ≥ 3.x to **≥ 2.4.0**. The same version-gated helper backs the key-based
  `verify --cosign-key` shell-out, so sign and verify stay a matched pair.
  (Reported against v0.4.0 by the FINOS Common Cloud Controls release pipeline.)

## [0.4.0] - 2026-07-03

### Changed

- **Keyless `grcli verify` now verifies in-process — `cosign` is no longer a
  consumer prerequisite** (ADR-0046). Both keyless paths (zero-flag
  verify-by-coordinate and explicit `--certificate-identity`) verify with the
  embedded `sigstore-go` library and the same pinned trust root + policy the hub
  uses (Rekor inclusion, observer timestamps, SCTs required), enforcing the
  expected signer identity in the verification policy. The signature is
  discovered in-process as an OCI referrer of the artifact manifest, so the old
  `--registry-token` subprocess plumbing is gone from the keyless paths. The
  pinned Sigstore public-good `trusted_root.json` is embedded and refreshed with
  each release; override it via `GRCLI_TRUSTED_ROOT` / the `trusted-root` config
  key (a `trusted_root.json` path) for air-gapped or private-Sigstore
  deployments. Only key-based `verify --cosign-key` still shells out to
  `cosign` ≥ 3.x. Verification behavior and identity semantics are unchanged —
  the same bundles that verified before verify the same way now.

### Added

- **`GRCLI_TRUSTED_ROOT` / `trusted-root` config key** (ADR-0046) — overrides the
  embedded Sigstore trust root with a `trusted_root.json` read from disk, for
  air-gapped deployments or a private Sigstore instance. Unset, keyless verify
  uses grcli's pinned embedded public-good root.

### Changed — BREAKING

- **The per-project `./.grcli.yaml` config layer is removed (ADR-0044).** Config
  now resolves from `--flag` > `GRCLI_*` env > user-global
  `~/.config/grcli/config.yaml` > built-in default; the repo-local file is no
  longer read. A committed config file must not be able to steer where a
  publish/verify tool talks. **Migration:** move any settings from
  `./.grcli.yaml` to `~/.config/grcli/config.yaml` — a lingering `./.grcli.yaml`
  prints a warning until removed.

### Added

- **`grcli verify` gains zero-flag verify-by-coordinate** (ADR-0045). Run
  `grcli verify --repository <ns>/<id> --version <v>` with **no trust flags** and
  grcli fetches the catalog record from the hub, reads the keyless signer
  identity the hub verified and pinned at ingest, and verifies against it — so a
  consumer needs no prior knowledge of the publishing workflow. The identity, and
  that it came from the hub record, are printed before verification runs (trust
  in the hub is visible, never silent). The ref-stripped pin is matched with an
  anchored SAN regexp `'^<escaped workflow path>@'`, admitting any git ref of
  that exact workflow but nothing wider. If the hub has no recorded
  identity (an artifact predating hub-side verification), verify fails with a
  clear pointer to the explicit flags. Passing `--cosign-key` or
  `--certificate-identity` bypasses the hub lookup entirely — the independent,
  high-assurance path — unchanged (including ADR-0044's issuer default).
- **`grcli verify` defaults `--certificate-oidc-issuer` to
  `https://token.actions.githubusercontent.com`** (ADR-0044). Keyless
  verification of a GitHub-Actions-signed bundle then needs only
  `--certificate-identity`. Override the issuer via the flag, the
  `GRCLI_CERTIFICATE_OIDC_ISSUER` env, or the user-global config for GitHub
  Enterprise, another CI provider, or an OIDC proxy. verify still checks the
  issuer, so a wrong value fails closed (it rejects, never falsely accepts).

## [0.3.0] - 2026-07-02

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
