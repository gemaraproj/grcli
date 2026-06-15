# Changelog

Notable changes to `grcli`. This project is pre-1.0; while on `v0.x`, a breaking
change bumps the minor version.

## [Unreleased]

### Changed — BREAKING

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
