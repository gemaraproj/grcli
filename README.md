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
| A grc.store account | `publish` (hub sync step) | Sign up at [grc.store](https://grc.store), then run `grcli login` to mint and store a bearer token via OIDC device flow (no manual copy-paste). The default `--url` is `https://hub.grc.store` — override only for private deployments. For CI: see "Publishing from GitHub Actions" below. |
| A valid Gemara YAML artifact | every command except `verify` | See the [Gemara spec](https://github.com/gemaraproj/gemara) |
| `cosign` on `PATH` | `publish` (signing), `verify` | https://docs.sigstore.dev/cosign/installation/ |
| `cue` on `PATH` | `validate` | https://cuelang.org |
| A local checkout of the Gemara spec | `validate` | `git clone https://github.com/gemaraproj/gemara` |
| `docker login` to your registry | `publish`, `unpack` against a private registry | standard Docker credentials (or set `GRCLI_REGISTRY_*` env, see below) |

If you only want to inspect or validate bundles, you don't need a hub
token or registry credentials.

## Subcommands

| Command | What it does |
| --- | --- |
| [`validate`](#validate) | Check a YAML file against the Gemara spec via `cue vet`. |
| [`login`](#login) | Sign in to a grc.store hub via OIDC device-authorization grant; stores tokens locally for `publish` to pick up. |
| [`publish`](#publish) | Pack one artifact + provenance into an OCI bundle, push it, optionally sign, and tell the hub. |
| [`logout`](#logout) | Forget locally-stored credentials for a hub. |
| [`unpack`](#unpack) | Pull a bundle from a registry (or a local layout) and write its files + manifest to disk. |
| [`verify`](#verify) | Verify a remote bundle's cosign signature against a known publisher policy. |

The natural workflow is `login → validate → publish → (consumer)
verify → unpack`.

### login

```sh
grcli login           # --url defaults to https://hub.grc.store
# Discovering https://hub.grc.store ...
#
# Open this URL in any browser to authorize:
#   https://auth.grc.store/realms/gemara/device?user_code=ABCD-EFGH
# Or visit https://auth.grc.store/realms/gemara/device and enter code:  ABCD-EFGH
# (code expires in 5m0s)
# Waiting for authorization...
#
# ✓ Signed in to https://auth.grc.store/realms/gemara
#   Token stored at /Users/you/.local/share/grcli/credentials.json (expires 2026-05-19T17:42:00Z)
```

Drives the OAuth 2.0 Device Authorization Grant (RFC 8628) against
the hub's configured Keycloak. The hub advertises its OIDC issuer +
CLI client_id via the well-known endpoint (ADR-0028); `grcli login`
self-configures from the single `--url`, which defaults to
`https://hub.grc.store` so a vanilla `grcli login` works out of the
box for the public deployment. Override the default with `--url <url>`
or `GRCLI_URL` when targeting a private hub.

Open the verification URL on any device with a browser — it does
NOT have to be the same machine grcli is running on. Useful when
publishing from an SSH'd shell, a container, or a CI sandbox where
no browser is available locally (note: for unattended CI, see
"Publishing from GitHub Actions" below — `grcli login` is for
interactive use).

Credentials are stored at `${XDG_DATA_HOME:-~/.local/share}/grcli/credentials.json`
with 0600 perms, keyed by OIDC issuer so multiple grc.store
deployments coexist. The refresh token is included; subsequent
`publish` calls refresh transparently when the access token is
within 60s of expiry.

### logout

```sh
grcli logout --url https://hub.grc.store
# ✓ Forgot credentials for https://auth.grc.store/realms/gemara

# Or directly by issuer (useful when the hub is unreachable):
grcli logout --issuer https://auth.grc.store/realms/gemara
```

Removes the local credential entry. The hub is not contacted; logout
is purely a local-file operation. Other hubs you've logged into are
untouched.

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
# Simplest path — after one `grcli login`, the stored token is picked
# up automatically and --url defaults to https://hub.grc.store. No
# flags needed beyond the file.
grcli publish controls.yaml

# Multi-file ControlCatalog or GuidanceCatalog (other types must be one file)
grcli publish controls/access.yaml controls/vuln.yaml

# Target a private hub instead of the default
grcli publish controls.yaml --url https://hub.example.internal

# Override the stored token explicitly (e.g. service-account token from
# CI's secrets store) — wins over `grcli login`'s stored credentials.
GRCLI_TOKEN="$GRCSTORE_TOKEN" grcli publish controls.yaml

# Older split --registry / --hub-url flags (deprecated; --url is preferred).
# Explicit --registry suppresses the default --url so no conflict fires.
grcli publish -f controls.yaml \
  --registry registry.grc.store \
  --hub-url https://hub.grc.store \
  --token "$GRCSTORE_TOKEN"

# Dry-run: write the bundle to disk, no network at all
grcli publish controls.yaml --dry-run --output ./bundle-out
```

Token resolution order at publish time: `--token` flag > `GRCLI_TOKEN`
env > credentials stored by `grcli login` (refreshed transparently if
within 60s of expiry) > error pointing you at `grcli login`.

Positional file args and `-f` / `--file` are mutually exclusive — pick
one form per invocation. Every flag in the table below also reads from
an env var (`GRCLI_*` prefix) and from `.grcli.yaml`.

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
| `-f, --file` | `GRCLI_FILE` | Repeatable; comma-separated also accepted. Or pass files positionally — not both. |
| `--url` | `GRCLI_URL` | grc.store base URL; discovers the registry from the hub's well-known endpoint (ADR-0026) |
| `--registry` | `GRCLI_REGISTRY` | OCI registry hostname (e.g. `registry.grc.store`) — deprecated, prefer `--url` |
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

The same envs apply to `publish`. **grc.store no longer accepts
anonymous writes**, so `publish` requires a credential: either run
`docker login <registry>` first, or set
`GRCLI_REGISTRY_USERNAME=gemara-publisher` +
`GRCLI_REGISTRY_PASSWORD=<shared publisher password from an admin>`.
Without one of these the registry returns `401 Unauthorized` on push.

When the `GRCLI_REGISTRY_*` envs are set, `grcli publish` forwards them
to the `cosign sign` step as well, so signing the pushed bundle works
over the same credential — no separate `docker login` needed. (If you
authenticate via `docker login` instead, cosign reads that chain
natively, so signing still works.)

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
