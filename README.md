# grcli

A command-line tool for the GRC artifact registry at
[grc.store](https://grc.store). `grcli` validates Gemara YAML against the
spec, packs it into a signed OCI bundle with SLSA-shaped provenance,
publishes it to a registry, and verifies bundles you fetch back.

## Install or Upgrade

Binaries are published as a public, signed, multi-platform OCI artifact
at `ghcr.io/revanite-io/grcli` (linux, macOS, and Windows on amd64 and
arm64). Pulling needs no token. You need [`oras`](https://oras.land) ≥
1.3 on `PATH`.

```sh
# platforms: linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64
oras pull ghcr.io/revanite-io/grcli:latest --platform darwin/arm64
chmod +x grcli && sudo mv grcli /usr/local/bin/
```

In GitHub Actions:

```yaml
# v2: https://github.com/oras-project/setup-oras/releases/tag/v2.0.0
- uses: oras-project/setup-oras@38de303aac69abb66f3e6255b7198bff35f323e3
- run: |
    oras pull ghcr.io/revanite-io/grcli:latest --platform linux/amd64
    sudo install grcli /usr/local/bin/grcli
```

Pin a release tag (`:v0.1.0`) instead of `latest` for reproducible
installs. To verify the signature before installing:

```sh
cosign verify ghcr.io/revanite-io/grcli:latest \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github.com/revanite-io/grcli/.github/workflows/release.yml@'
```

## Prerequisites

Some commands shell out to external tools:

- **`cosign`** on `PATH` — `publish` (signing) and `verify`.
  https://docs.sigstore.dev/cosign/installation/
- **`cue`** on `PATH` — `validate`. https://cuelang.org
- **A Gemara spec checkout** — `validate`.
  `git clone https://github.com/gemaraproj/gemara`
- **A grc.store account** — `publish`. Run `grcli login` (OIDC device
  flow), or use trusted publishing in CI (see below).

If you only inspect or validate bundles, you need no account or registry
credentials.

> **CI note (read before adding any GitHub secret):** `grcli publish`
> in GitHub Actions authenticates via the workflow's GitHub OIDC token,
> not a stored secret. You do **not** need to set `GRCLI_TOKEN`, a PAT,
> or any `secrets.*` value. The only requirements are
> `permissions: id-token: write` on the job and a one-time trusted-
> publisher binding for `owner/repo` (and optionally a specific branch)
> on the hub. See [Publishing from GitHub Actions](#publishing-from-github-actions).

## Usage

Run `grcli <command> --help` for the full flag list. The typical flow is
`login → validate → publish`; consumers `verify → unpack`.

| Command | What it does |
| --- | --- |
| `login` | Sign in to a hub via OIDC device flow; stores tokens for `publish`. |
| `validate` | Check YAML against the Gemara spec via `cue vet`. |
| `publish` | Pack an artifact + provenance into a signed OCI bundle, push it, and notify the hub. |
| `verify` | Verify a remote bundle's cosign signature. |
| `unpack` | Pull a bundle and write its files + manifest to disk. |
| `cat` | Print an artifact's Gemara content to stdout (no files written) — for piping into `yq`. |
| `logout` | Forget locally-stored credentials. |

```sh
# Sign in (defaults to https://hub.grc.store)
grcli login

# Validate against a spec checkout matching your metadata.gemara-version
grcli validate -f controls.yaml --spec /path/to/gemara

# Publish — picks up the stored login token; signs by default
grcli publish -f controls.yaml

# Verify a published bundle (keyless; issuer defaults to GitHub Actions)
grcli verify --repository myorg/my-controls --version 1.0.0 \
  --certificate-identity https://github.com/myorg/my-controls/.github/workflows/publish.yml@refs/heads/main

# Unpack a bundle to disk
grcli unpack --repository myorg/my-controls --version 1.0.0 --output ./unpacked

# Print an artifact's Gemara content to stdout (no files written)
grcli cat --repository myorg/my-controls --version 1.0.0 | yq '.metadata.title'
```

These default to the public hub at `https://hub.grc.store`; add `--url
<hub>` for a private deployment.

### Reading artifacts: `unpack` vs `cat`

`unpack` writes an artifact's files **and** its `bundle.json` manifest (with
provenance) into a directory. `cat` streams the **Gemara content only** to
stdout — no manifest, no files on disk — so it pipes cleanly into `yq` (the
content is YAML; for `jq`, convert first with `yq -o=json`). A
single-file bundle prints verbatim; a multi-file bundle prints as a `---`
separated YAML stream (use `--file <name>` to pick one). With `--with-imports` /
`--with-references`, `unpack` also pulls the artifacts a bundle references into a
`references/<category>/<ns>/<id>@<version>/` directory tree plus a
`references/index.json` (`cat` is primary-only).

### Caching

Remote (`--url`) fetches are served from an on-disk cache: the first pull of a
given `namespace/id/version` is stored (the whole bundle — files + manifest),
and later `unpack`/`cat` of the same coordinate — or references to it — are
served from the cache with **no network at all**. grc.store tags are immutable,
so a cache hit can never be stale. The cache lives at `$GRCLI_CACHE` (default
`os.UserCacheDir()/grcli`) and grows without bound (no GC yet).

- `--no-cache` bypasses the cache for a single run (fresh pull, nothing stored).
- Set `cache-enabled: false` in config (below) to disable it durably.
- `--source` (local layout) reads are never cached.

### Configuration

`grcli` reads config from, highest precedence first: a `--flag`, a `GRCLI_*`
env var, and the user-global `$XDG_CONFIG_HOME/grcli/config.yaml` (falling back
to `~/.config/grcli/config.yaml`). There is **no per-project layer**: a
repo-local `./.grcli.yaml` is deliberately not read (ADR-0044) — a committed
file must not be able to steer where a publish/verify tool talks — and a present
one prints a migration warning until removed. `--config <file>` selects a single
file and bypasses the search.

Keys (env form in parentheses):

- `cache-enabled: true|false` (`GRCLI_CACHE_ENABLED`) — durable equivalent of
  `--no-cache` when `false`. Default `true`.
- `url` (`GRCLI_URL`) — the hub base URL. Config keys generally use the same
  name as their flag (env form: `GRCLI_` + the name upper-snaked); see
  `grcli <command> --help` for the flag list.
- `certificate-oidc-issuer` (`GRCLI_CERTIFICATE_OIDC_ISSUER`) — the OIDC issuer
  `grcli verify` expects for keyless verification. Defaults to
  `https://token.actions.githubusercontent.com`; set it only for GitHub
  Enterprise, another CI provider, or an OIDC proxy.

> **Registry credentials are env-only, never config keys**: set
> `GRCLI_REGISTRY_TOKEN` (or `GRCLI_REGISTRY_USERNAME` +
> `GRCLI_REGISTRY_PASSWORD`) in the environment. A `registry-token:` line in a
> config file is ignored. The cache *location* is likewise set only by
> `$GRCLI_CACHE`. (The cache toggle key is the flat `cache-enabled`, not
> `cache.enabled`, because `$GRCLI_CACHE` would otherwise shadow a nested
> `cache.*` key.)

Signing is required by default: if `cosign` isn't available, `publish`
fails *before* pushing, so nothing unsigned reaches the registry. Pass
`--no-sign` to deliberately opt out.

Catalog signatures use the **Sigstore bundle format** (cosign's
`--new-bundle-format`, attached as an OCI 1.1 referrer), which `grcli
verify` always requests — so to verify a catalog manually use `cosign
verify --new-bundle-format …`, not the bare `cosign verify` shown above
for the grcli binary. Artifacts signed by an older grcli (the legacy
`.sig` tag format) must be re-published to re-sign. Requires cosign ≥ 3.x.

## Publishing from GitHub Actions

**`grcli` in CI needs no GitHub secret, no `GRCLI_TOKEN`, no
`secrets.*` reference, no PAT.** Do not create one. Trusted publishing
(ADR-0032) means the workflow's GitHub OIDC token is the credential —
`grcli publish` mints it at runtime from the Actions OIDC endpoint
that `permissions: id-token: write` enables. The hub validates the
token's `iss` (GitHub) and `sub` (your repo/ref) against its
trusted-publisher bindings.

**One-time setup, done on the hub — not in your repo:** an org admin
adds your repo (`owner/repo`, optionally pinned to a ref) on the hub
as a Trusted CI publisher for the namespace your bundles publish
under (the `<author>/<name>` path). Until that binding exists the hub
returns 403; *adding a GitHub secret will not fix it.*

```yaml
permissions:
  contents: read
  id-token: write   # OIDC token: hub auth + cosign keyless signing
                    # this is the ONLY auth grcli needs in CI

jobs:
  publish:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      # v2: https://github.com/oras-project/setup-oras/releases/tag/v2.0.0
      - uses: oras-project/setup-oras@38de303aac69abb66f3e6255b7198bff35f323e3
      - run: |
          oras pull ghcr.io/revanite-io/grcli:latest --platform linux/amd64
          sudo install grcli /usr/local/bin/grcli
      - uses: sigstore/cosign-installer@v3
      - run: grcli publish -f controls.yaml
        # no `env:` block, no `with: token:`, no secrets — id-token: write
        # above is what makes this work
```

cosign records the workflow URL as the signer identity
(`https://github.com/<org>/<repo>/.github/workflows/publish.yml@<ref>`).
Share that with the issuer
`https://token.actions.githubusercontent.com` as the `grcli verify`
policy.

## License

Source-available, not open-source.
