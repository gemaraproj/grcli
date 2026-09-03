# Changelog

Notable changes to `grcli`. This project is pre-1.0; while on `v0.x`, a breaking
change bumps the minor version.

## [Unreleased]

### Changed

- `publish` now runs on `grc-store-clientkit` (hub discovery, registry tokens,
  bundle pack/push, keyless signing, SLSA provenance) — the same code path
  `pvtr` uses, so the two tools cannot drift on what the hub expects.
- Keyless signing no longer requires GitHub Actions: `SIGSTORE_ID_TOKEN`
  (an OIDC token with audience `sigstore`) or an interactive browser sign-in
  work too. The signing identity is resolved *before* the push, so a missing
  identity never leaves unsigned bytes in the registry.
- A publish whose login does not own the target namespace now fails before
  packing (the hub grants pull-only tokens to non-owners) instead of at sync.
- Signed publishes attach a second OCI referrer carrying the SLSA provenance
  predicate (artifactType `application/vnd.grc-store.provenance.bundle.v0.3+json`).
- Registry credential overrides moved to the shared `GRC_STORE_REGISTRY_TOKEN`,
  `GRC_STORE_REGISTRY_USERNAME` / `GRC_STORE_REGISTRY_PASSWORD` names, and the
  Sigstore endpoint overrides to `GRC_STORE_FULCIO_URL` / `GRC_STORE_REKOR_URL`.
  **Deprecated:** the `GRCLI_REGISTRY_*` and `GRCLI_FULCIO_URL` /
  `GRCLI_REKOR_URL` spellings; `GRCLI_REGISTRY_*` still work for this release
  with a warning, the Sigstore ones are no longer read.
- go-gemara v0.9: a bundle carries exactly one source artifact. `cat` writes
  it verbatim (the multi-document stream is gone), and a cache entry written
  by an older grcli with several files is refused with a `--no-cache` hint.

## [0.1.0] - Unreleased

First release of `grcli` under `github.com/gemaraproj/grcli`, published as a
signed multi-platform OCI artifact at `ghcr.io/gemaraproj/grcli`.

### Added

- `login` / `logout` — OIDC device-flow sign-in to a hub, credentials stored
  at `$XDG_DATA_HOME/grcli/credentials.json`.
- `validate` — check Gemara YAML against the spec via `cue vet`.
- `publish` — pack an artifact plus SLSA-shaped provenance into an OCI bundle,
  sign it, push it, and notify the hub. `--license` (an SPDX expression) is
  required. Keyless signing runs in-process via `sigstore-go` using the
  GitHub Actions OIDC token; `cosign` is needed only for `--cosign-key`.
- `verify` — verify a bundle's Sigstore signature in-process; with no trust
  flags, against the signer identity the hub recorded at ingest.
- `unpack` — verify (fail-closed; `--no-verify` to skip) then extract a bundle
  to a directory from a registry or OCI layout.
- `cat` — stream an artifact's Gemara content to stdout without writing files.
- `versions <ns>/<id>` — list published versions.
- On-disk cache for remote fetches at `$GRCLI_CACHE`; `--no-cache` per run,
  `cache-enabled: false` to disable.
- Single user-global config at `$XDG_CONFIG_HOME/grcli/config.yaml`, with
  `GRCLI_*` env overrides and `--config <file>` to bypass.
- Reusable GitHub Actions workflow (`.github/workflows/publish-gemara.yml`)
  and install action (`.github/actions/install`).
