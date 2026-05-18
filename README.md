# grcli

A small command-line tool that prepares a Gemara artifact bundle (with provenance
metadata, optional cosign signature) and publishes it to the GRC artifact
registry at [grc.store](https://grc.store).

Designed for two callers:

- **Local** — `grcli publish -f my-catalog.yaml --registry registry.grc.store --token $GRCSTORE_TOKEN`
- **CI** (e.g. GitHub Actions) — same flags, picks up OIDC for keyless signing automatically when `GITHUB_ACTIONS=true`

## Quick start

```sh
# One file → one artifact
grcli publish -f controls.yaml \
  --registry registry.grc.store \
  --hub-url https://grc.store \
  --token "$GRCSTORE_TOKEN"

# Multi-file catalog (control + guidance catalogs only; LoadFiles merges them)
grcli publish \
  -f controls/access.yaml \
  -f controls/vuln.yaml \
  -f controls/governance.yaml \
  --registry registry.grc.store \
  --hub-url https://grc.store \
  --token "$GRCSTORE_TOKEN"

# Inspect what would be pushed, no network calls
grcli publish -f controls.yaml --dry-run --output ./bundle-out
```

## What it does

1. Loads the provided file(s) and verifies they describe **one** artifact (matching `metadata.id` and `metadata.type`). Multi-file inputs are merged via `go-gemara`'s `LoadFiles` for `ControlCatalog` and `GuidanceCatalog`; other types accept exactly one file.
2. Generates a SLSA v1.0-shaped provenance JSON record (builder identity, source ref, file digests, tool version) and embeds it in the bundle's OCI config blob.
3. Packs the artifact + provenance into a Gemara OCI bundle (`application/vnd.gemara.bundle.v1`) and pushes it to the configured registry.
4. If `cosign` is on `PATH` and credentials are available (keyless OIDC in CI, key file locally), signs the pushed manifest.
5. Calls `POST /v1/bundles/sync` on the hub so the registry indexes the new version.

`--dry-run` writes an OCI image layout to `--output` instead of touching any network.

## Configuration

Flags, environment variables (prefix `GRCLI_`), and a YAML config file all bind to the same keys. Search order: `--config <path>` → `./.grcli.yaml` → `$XDG_CONFIG_HOME/grcli/config.yaml` → `$HOME/.grcli.yaml`.

| Flag             | Env                    | Notes                                            |
| ---------------- | ---------------------- | ------------------------------------------------ |
| `-f, --file`     | —                      | Repeatable; required unless `--config` provides  |
| `--registry`     | `GRCLI_REGISTRY`       | OCI registry hostname (e.g. `registry.grc.store`)|
| `--repository`   | `GRCLI_REPOSITORY`     | Defaults to `<author.id>/<metadata.id>` (slugified) |
| `--tag`          | `GRCLI_TAG`            | Defaults to `metadata.version`                   |
| `--hub-url`      | `GRCLI_HUB_URL`        | e.g. `https://grc.store`                         |
| `--token`        | `GRCLI_TOKEN`          | Bearer for hub sync; never written to disk       |
| `--dry-run`      | `GRCLI_DRY_RUN`        | Skip all network                                 |
| `--output`       | `GRCLI_OUTPUT`         | `--dry-run` target dir (default `./grcli-out`)   |
| `--no-sign`      | `GRCLI_NO_SIGN`        | Skip cosign even when available                  |
| `--cosign-key`   | `COSIGN_KEY`           | Local key file path                              |

## License

Source-available, not open-source. See [LICENSE](./LICENSE).
