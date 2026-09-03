# Changelog

Notable changes to `grcli`. This project is pre-1.0; while on `v0.x`, a breaking
change bumps the minor version.

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
