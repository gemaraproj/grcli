# Changelog

Notable changes to `grcli`. This project is pre-1.0; while on `v0.x`, a breaking
change bumps the minor version.

## [Unreleased]

### Changed

- **Go module path renamed to `github.com/gemaraproj/grcli`.** This repo now
  supersedes `revanite-io/grcli` entirely; every workflow, example and doc
  points at `github.com/gemaraproj/grcli` / `ghcr.io/gemaraproj/grcli`.

## [0.6.0] - 2026-08-19

> **Live CI smoke PASSED 2026-08-19** — the gate this release was held behind.
> A real keyless publish ran from a runner with **no cosign installed**
> (`eddie-knight/security-baseline` → preview hub), and the zero-flag verify
> resolved the signature against the hub-recorded signer identity
> `keyless:…#https://github.com/eddie-knight/security-baseline/.github/workflows/publish.yaml`.
> That exercised the Fulcio, Rekor and GitHub-OIDC legs end to end for the
> first time — none of which can be reached offline.
>
> **The repo also moved orgs after v0.5.1**: v0.6.0+ publish to
> `ghcr.io/gemaraproj/grcli`; tags ≤ v0.5.1 remain at
> `ghcr.io/revanite-io/grcli` and are not re-published.

### Changed

- **Keyless publish signing runs IN-PROCESS via `sigstore-go`; `cosign` is no
  longer required for CI publishing (ADR-0049).** `grcli publish` in GitHub
  Actions now requests the OIDC token itself, obtains a Fulcio certificate,
  signs the manifest digest (a DSSE-wrapped in-toto statement, byte-shaped like
  `cosign sign --new-bundle-format`), logs it in Rekor, and attaches the bundle
  as an OCI referrer — all with the library grcli already uses to *verify*
  (ADR-0046), so publishing needs no external tools. This removes the cosign
  version-band fragility entirely (the `--new-bundle-format` gating, the 2.6.0
  floor, and the 2.4–2.5 dead-zone that broke publishing). `cosign` remains a
  prerequisite **only** for `--cosign-key` (key-based) signing and
  `verify --cosign-key`. Air-gapped/private-Sigstore signing: point
  `GRCLI_FULCIO_URL` / `GRCLI_REKOR_URL` at your instance.

### Fixed

- **In-process keyless signing could not attach its signature referrer at all.**
  The referrer manifest was packed with artifactType
  `https://sigstore.dev/cosign/sign/v1` — a URL, not an RFC 6838 media type — so
  `oras.PackManifest` refused it before any network I/O and every keyless
  publish died with `invalid artifactType format: … : invalid media type`. The
  referrer is now stamped `application/vnd.dev.sigstore.bundle.v0.3+json`, which
  is both the semantically correct type (grcli's signer emits a v0.3 bundle, so
  it follows the bundle-by-default signer line) and the maximally compatible one
  (hubs predating the both-types ingest fix accepted only that stamp). Signature
  *discovery* is unchanged and still accepts both stamp variants — cosign 2.6.x
  legitimately signs with the URL form; only the write side was ever broken.
  (Found by the first real keyless CI publish, 2026-08-19.)

## [0.5.0] - 2026-07-10

### Changed

- **BREAKING: `unpack` verifies the artifact's signature by default and fails
  closed (ADR-0048).** A remote (`--url`) unpack now discovers the Sigstore
  signature and verifies it in-process BEFORE writing anything — the same check
  as `grcli verify` (zero-flag against the identity the hub recorded at ingest,
  or `--certificate-identity` to assert the signer yourself and bypass the hub).
  An unsigned, mis-signed, or unverifiable artifact is refused and **no files are
  written**. Pass `--no-verify` to write without verifying (INSECURE); a local
  `--source` layout has no registry signature and is always written unverified.
  - *Migration:* scripts that unpacked unsigned/legacy content now fail until they
    pass `--no-verify` or the content is re-published signed (same migration class
    as the earlier signature-format cutovers).
  - *Offline note:* a cached unpack is no longer fully offline — verification
    contacts the hub/registry even on a content cache hit. Use `--no-verify` for
    the previous offline-from-cache behavior.

### Fixed

- **`verify` now discovers signatures attached by cosign 3.x.** The referrer
  artifactType a cosign-signed catalog carries depends on the signer's cosign
  major version: 2.6.x (`--new-bundle-format`) stamps
  `https://sigstore.dev/cosign/sign/v1`, while 3.x (bundle by default) stamps
  `application/vnd.dev.sigstore.bundle.v0.3+json` — the bundle inside is
  identical. grcli filtered on the 2.6.x value only, so a cosign-3.x-signed
  catalog verified as "no signature attached". Discovery now accepts both
  stamp variants. (Found by the first live zero-flag verify against preview,
  2026-07-07; supersedes the protocol's "do not cross these" mediatype rule,
  whose premise predates cosign 3.x.)
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
