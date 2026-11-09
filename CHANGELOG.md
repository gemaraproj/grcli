# Changelog

Notable changes to `grcli`. This project is pre-1.0; while on `v0.x`, a breaking
change bumps the minor version.

## Unreleased

### Changed

- **Breaking:** `publish` pushes every bundle and its signature to the staging
  repository `staging/<namespace>/<name>` and then syncs `<namespace>/<name>`
  (the `staging` package of grc-store-protocol v0.28.1 names the repository). There is no fallback to pushing
  the served repository, so this release cannot publish to a hub from before
  the staging cutover, and older releases cannot publish to a hub after it
  (their first blob push gets `401 UNAUTHORIZED`).
- The hub client's timeout rises from 60 seconds to 3 minutes, above the
  hub's 120-second catalog sync deadline.
- A refused push names the staging repository and the namespace you need to
  own, and a refused sync shows the hub's error code and says whether to
  publish again.

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
- `validate` and `publish` warn about a `mapping-references[].url` that points
  at grc.store but is not the canonical `https://grc.store/<ns>/<id>` form, or
  about an `imports`/`extends`/`lexicon` reference with no url. Warnings only;
  nothing is rewritten or refused.
- `unpack --with-references` / `--with-imports` decide whether a url names a
  hub artifact with the shared `grc-store-protocol/refurl` rule, the same one
  the hub index and the web UI follow: the hub API path, the legacy
  `/search/<ns>/<id>` form, and mixed-case segments (slugified) all resolve.
  A version suffix on `https://grc.store/<ns>/<id>` does not; the version
  belongs in the reference's `version` field.
- On-disk cache for remote fetches at `$GRCLI_CACHE`; `--no-cache` per run,
  `cache-enabled: false` to disable.
- Single user-global config at `$XDG_CONFIG_HOME/grcli/config.yaml`, with
  `GRCLI_*` env overrides and `--config <file>` to bypass.
- Reusable GitHub Actions workflow (`.github/workflows/publish-gemara.yml`)
  and install action (`.github/actions/install`).
- `validate` resolves the Gemara schemas from the CUE central registry
  (`github.com/gemaraproj/gemara`) at the version in each file's
  `metadata.gemara-version` (else the latest), so no spec checkout is needed.
  `--gemara-version` / `GRCLI_GEMARA_VERSION` pins one version for every file;
  `--spec` / `GRCLI_GEMARA_SPEC_DIR` validates against a local checkout
  instead. `metadata.type` and `metadata.gemara-version` must be a plain type
  name and a plain version; anything else fails before `cue` runs. `OK`/`FAIL`
  lines now include the Gemara version, e.g. `(#Policy, gemara v1.6.0)`.
  Each `cue vet` invocation has a 30-second timeout. If a local spec comes
  from the environment or config and a version is also set there, the local
  spec wins with a warning on stderr that the version is ignored.
