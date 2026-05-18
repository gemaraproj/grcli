# grcli

> [!WARNING]
>
> This repo is a work in progress. Pre-built binaries are not yet
> published; install instructions below build from source.

A command-line tool for the GRC artifact registry at
[grc.store](https://grc.store). `grcli` validates Gemara YAML against
the spec, packs it into a signed OCI bundle with SLSA-shaped provenance,
publishes it to a registry, and verifies bundles you fetch back.

## Installation

Requires Go ≥ 1.25.

```sh
git clone git@github.com:revanite-io/grcli.git
cd grcli
make build      # produces ./bin/grcli
```

Move `./bin/grcli` somewhere on your `$PATH`, or run it from the
checkout. Pre-built binaries and container images will follow.

## Prerequisites

| What | Required for | How to obtain |
| --- | --- | --- |
| A grc.store account and bearer token | `publish` (hub sync step) | Sign up at [grc.store](https://grc.store) and copy your API token |
| A valid Gemara YAML artifact | every command except `verify` | See the [Gemara spec](https://github.com/gemaraproj/gemara) |
| `cosign` on `PATH` | `publish` (signing), `verify` | https://docs.sigstore.dev/cosign/installation/ |
| `cue` on `PATH` | `validate` | https://cuelang.org |
| A local checkout of the Gemara spec | `validate` | `git clone https://github.com/gemaraproj/gemara` |
| `docker login` to your registry | `publish`, `unpack` against a private registry | standard Docker credentials (or set `GRCLI_REGISTRY_*` env, see below) |

If you only want to inspect or validate bundles, you don't need a hub
token or registry credentials.

## The four subcommands

| Command | What it does |
| --- | --- |
| [`validate`](#validate) | Check a YAML file against the Gemara spec via `cue vet`. |
| [`publish`](#publish) | Pack one artifact + provenance into an OCI bundle, push it, optionally sign, and tell the hub. |
| [`unpack`](#unpack) | Pull a bundle from a registry (or a local layout) and write its files + manifest to disk. |
| [`verify`](#verify) | Verify a remote bundle's cosign signature against a known publisher policy. |

The natural workflow is `validate → publish → (consumer) verify →
unpack`.

### validate

```sh
git clone --branch v1.0.0 https://github.com/gemaraproj/gemara /tmp/gemara
grcli validate -f controls.yaml --spec /tmp/gemara
```

Validates `controls.yaml` (and any other `-f` files) against the Gemara
CUE schemas in the spec checkout. Use the branch that matches your
artifact's `metadata.gemara-version`. The spec path can also be set
via `GRCLI_GEMARA_SPEC_DIR`.

### publish

```sh
# One file → one artifact
grcli publish -f controls.yaml \
  --registry registry.grc.store \
  --hub-url https://grc.store \
  --token "$GRCSTORE_TOKEN"

# Multi-file ControlCatalog or GuidanceCatalog (other types must be one file)
grcli publish \
  -f controls/access.yaml \
  -f controls/vuln.yaml \
  --registry registry.grc.store \
  --hub-url https://grc.store \
  --token "$GRCSTORE_TOKEN"

# Dry-run: write the bundle to disk, no network at all
grcli publish -f controls.yaml --dry-run --output ./bundle-out
```

What happens, in order:

1. Loads the file(s) and verifies they describe **one** artifact (matching `metadata.id` and `metadata.type`). For `ControlCatalog` and `GuidanceCatalog`, multiple files are merged via `go-gemara`'s `LoadFiles`; other types accept exactly one file.
2. Generates a SLSA v1.0-shaped provenance record (builder identity, git ref, source-file digests, tool version) and embeds it under `metadata.provenance` in the bundle manifest.
3. Packs the artifact + provenance into a Gemara OCI bundle (`application/vnd.gemara.bundle.v1`) and pushes it to the configured registry as `<registry>/<repository>:<tag>`.
4. If `cosign` is on `PATH` and signing material is available, signs the pushed manifest. Keyless via OIDC when `GITHUB_ACTIONS=true`; otherwise via `--cosign-key`.
5. Calls `POST /v1/bundles/sync` on the hub so the index picks up the new version.

`--dry-run` writes an OCI image layout to `--output` and skips steps 3–5
entirely. `--registry`, `--hub-url`, and `--token` are not required under
`--dry-run`.

### unpack

```sh
# From a 'publish --dry-run' output
grcli unpack --source ./bundle-out --tag 1.0.0 --output ./unpacked

# From a remote registry
grcli unpack --registry registry.grc.store \
  --repository myorg/my-controls --tag 1.0.0 \
  --output ./unpacked
```

Writes the artifact file(s) and a `bundle.json` (containing the bundle
manifest and provenance) into `--output`. Imports, if any, are written
under `<output>/imports/`.

### verify

```sh
# Key-based: paired with publish's --cosign-key
grcli verify --registry registry.grc.store \
  --repository myorg/my-controls --tag 1.0.0 \
  --cosign-key /keys/myorg-cosign.pub

# Keyless: paired with publish's GitHub Actions OIDC flow
grcli verify --registry registry.grc.store \
  --repository myorg/my-controls --tag 1.0.0 \
  --certificate-identity   https://github.com/myorg/my-controls/.github/workflows/publish.yml@refs/heads/main \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

Shells out to `cosign verify` against the remote bundle. There is no
local-layout verify mode — cosign signatures live at the registry layer,
not in the bundle bytes.

The two trust shapes — a public key, or an `(identity, issuer)` tuple —
are also the policy shapes grc.store will eventually accept for
publisher registration. For now, share whichever pair you used with
anyone who needs to verify your bundles.

## Configuration

Flags, environment variables (prefix `GRCLI_`), and a YAML config file
all bind to the same keys. Search order: `--config <path>` →
`./.grcli.yaml` → `$XDG_CONFIG_HOME/grcli/config.yaml` →
`$HOME/.grcli.yaml`.

Note: `COSIGN_KEY` is the one env var that does **not** carry the
`GRCLI_` prefix — `grcli` deliberately matches cosign's own
convention so existing cosign users see it picked up.

### publish flags

| Flag | Env | Notes |
| --- | --- | --- |
| `-f, --file` | — | Repeatable; comma-separated also accepted |
| `--registry` | `GRCLI_REGISTRY` | OCI registry hostname (e.g. `registry.grc.store`) |
| `--repository` | `GRCLI_REPOSITORY` | Defaults to `<author.id>/<metadata.id>` slugified to `[a-z0-9._-]` |
| `--tag` | `GRCLI_TAG` | Defaults to `metadata.version` |
| `--hub-url` | `GRCLI_HUB_URL` | e.g. `https://grc.store`; omit to skip the hub sync step |
| `--token` | `GRCLI_TOKEN` | Bearer token for the hub sync call |
| `--dry-run` | `GRCLI_DRY_RUN` | Skip all network; write OCI layout to `--output` |
| `--output` | `GRCLI_OUTPUT` | Dry-run target dir (default `grcli-out`) |
| `--no-sign` | `GRCLI_NO_SIGN` | Skip cosign even when material is available |
| `--cosign-key` | `COSIGN_KEY` | Local cosign private key file |

### unpack flags

| Flag | Env | Notes |
| --- | --- | --- |
| `--source` | `GRCLI_SOURCE` | Local OCI layout dir (mutually exclusive with `--registry`) |
| `--registry` | `GRCLI_REGISTRY` | OCI registry hostname (mutually exclusive with `--source`) |
| `--repository` | `GRCLI_REPOSITORY` | Required when `--registry` is set |
| `--tag` | `GRCLI_TAG` | Required |
| `--output` | `GRCLI_OUTPUT` | Where to write extracted files (default `grcli-unpacked`) |

Remote-registry auth uses the Docker credential chain plus these
overrides:

| Env | Purpose |
| --- | --- |
| `GRCLI_REGISTRY_USERNAME` + `GRCLI_REGISTRY_PASSWORD` | Basic-auth username and password |
| `GRCLI_REGISTRY_TOKEN` | Raw bearer token (alternative to user/pass) |

The same envs apply to `publish` against private registries.

### validate flags

| Flag | Env | Notes |
| --- | --- | --- |
| `-f, --file` | — | Repeatable; comma-separated also accepted |
| `--spec` | `GRCLI_GEMARA_SPEC_DIR` | Path to a local Gemara CUE module checkout |

### verify flags

| Flag | Env | Notes |
| --- | --- | --- |
| `--registry` | `GRCLI_REGISTRY` | Required |
| `--repository` | `GRCLI_REPOSITORY` | Required |
| `--tag` | `GRCLI_TAG` | Required |
| `--cosign-key` | `COSIGN_KEY` | Public key file (mutually exclusive with keyless flags) |
| `--certificate-identity` | `GRCLI_CERTIFICATE_IDENTITY` | Expected signer identity (e.g. a GHA workflow URL) |
| `--certificate-oidc-issuer` | `GRCLI_CERTIFICATE_OIDC_ISSUER` | Expected OIDC issuer |

### Config file

Any flag can be set in `.grcli.yaml`. Example:

```yaml
registry: registry.grc.store
hub-url: https://grc.store
repository: myorg/my-controls
# token: ...   # better to pass via $GRCLI_TOKEN
```

Then:

```sh
grcli publish -f controls.yaml --token "$GRCSTORE_TOKEN"
```

## Publishing from GitHub Actions

`grcli` detects `GITHUB_ACTIONS=true` and uses Sigstore keyless signing
via the workflow's OIDC token. The workflow needs `id-token: write`
permission for that token to be issued.

```yaml
permissions:
  contents: read
  id-token: write   # required for cosign keyless

jobs:
  publish:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      # Build grcli from source; replace with a release fetch once available.
      - uses: actions/setup-go@v5
        with:
          go-version: '1.25'
      - name: Install grcli
        run: |
          git clone git@github.com:revanite-io/grcli.git /tmp/grcli
          cd /tmp/grcli && make build && sudo mv bin/grcli /usr/local/bin/

      - name: Install cosign
        uses: sigstore/cosign-installer@v3

      - name: Publish
        env:
          GRCLI_TOKEN: ${{ secrets.GRCSTORE_TOKEN }}
        run: |
          grcli publish -f controls.yaml \
            --registry registry.grc.store \
            --hub-url   https://grc.store
```

The cert identity that cosign records will be the URL of this workflow
(e.g. `https://github.com/<org>/<repo>/.github/workflows/publish.yml@refs/heads/main`);
share that, along with the issuer `https://token.actions.githubusercontent.com`,
as your verification policy.

## License

Source-available, not open-source. See [LICENSE](./LICENSE).
