# grcli

A command-line tool for the GRC artifact registry at
[grc.store](https://grc.store). `grcli` validates Gemara YAML against the
spec, packs it into a signed OCI bundle with SLSA-shaped provenance,
publishes it to a registry, and verifies bundles you fetch back.

## Install or Upgrade

Binaries are published as a public, signed, multi-platform OCI artifact
at `ghcr.io/gemaraproj/grcli` (linux, macOS, and Windows on amd64 and
arm64). Pulling needs no token. You need [`oras`](https://oras.land) ≥
1.3 on `PATH`.

```sh
# platforms: linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64
oras pull ghcr.io/gemaraproj/grcli:latest --platform darwin/arm64
chmod +x grcli && sudo mv grcli /usr/local/bin/
```

In GitHub Actions:

```yaml
# v2: https://github.com/oras-project/setup-oras/releases/tag/v2.0.0
- uses: oras-project/setup-oras@38de303aac69abb66f3e6255b7198bff35f323e3
- run: |
    oras pull ghcr.io/gemaraproj/grcli:latest --platform linux/amd64
    sudo install grcli /usr/local/bin/grcli
```

Pin a release tag (`:v0.1.0`) instead of `latest` for reproducible
installs. To verify the signature before installing:

```sh
cosign verify ghcr.io/gemaraproj/grcli:latest \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github.com/gemaraproj/grcli/.github/workflows/release.yml@'
```

## Prerequisites

Some commands shell out to external tools:

- **`cosign` ≥ 2.6.0** on `PATH` — **only** for key-based signing
  (`publish --cosign-key`) and key-based `verify --cosign-key`. When cosign is
  used, grcli detects its version and adapts the Sigstore bundle-format flag
  (`--new-bundle-format` on 2.6–2.x, omitted on 3.x). **Keyless CI publishing and
  all keyless `verify`/`unpack` need no external tools** — grcli signs (ADR-0049)
  and verifies (ADR-0046) in-process against Sigstore, so the common path is just
  the `grcli` binary. https://docs.sigstore.dev/cosign/installation/
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
| `publish` | Pack an artifact + provenance into a signed OCI bundle, push it, and notify the hub. Requires `--license` (SPDX expression, ADR-0037). |
| `verify` | Verify a remote bundle's Sigstore signature (keyless: in-process, no cosign) — with no trust flags, against the signer identity the hub recorded at ingest. |
| `unpack` | Verify a remote bundle's signature (fail-closed; `--no-verify` to skip) then write its files + manifest to disk. |
| `cat` | Print an artifact's Gemara content to stdout (no files written) — for piping into `yq`. |
| `logout` | Forget locally-stored credentials. |

```sh
# Sign in (defaults to https://hub.grc.store)
grcli login

# Validate against a spec checkout matching your metadata.gemara-version
grcli validate -f controls.yaml --spec /path/to/gemara

# Publish — picks up the stored login token; signs by default.
# --license is REQUIRED (ADR-0037) and takes an SPDX expression; publish
# fails before any network call without it. Use your catalog's real terms.
# Locally you must also supply signing material: --cosign-key (below) or
# --no-sign. Keyless signing is CI-only — see "Signing" further down.
grcli publish -f controls.yaml --license Apache-2.0 --cosign-key cosign.key

# Verify a published bundle — zero-flag: uses the signer identity the hub
# recorded at ingest (prints it, and that it came from the hub, before verifying)
grcli verify --repository myorg/my-controls --version 1.0.0

# Or assert the identity yourself for an independent check (bypasses the hub
# lookup; issuer defaults to GitHub Actions)
grcli verify --repository myorg/my-controls --version 1.0.0 \
  --certificate-identity https://github.com/myorg/my-controls/.github/workflows/publish.yml@refs/heads/main

# Unpack a bundle to disk
grcli unpack --repository myorg/my-controls --version 1.0.0 --output ./unpacked

# Print an artifact's Gemara content to stdout (no files written)
grcli cat --repository myorg/my-controls --version 1.0.0 | yq '.title'
```

These default to the public hub at `https://hub.grc.store`; add `--url
<hub>` for a private deployment.

### Where a publish lands: the `--repository` default

`publish` does not ask you where to publish — **it derives the target from the
bundle's own metadata**:

```
<namespace>/<name>  =  slugify(metadata.author.id) / slugify(metadata.id)
```

`slugify` replaces every run of characters outside `[a-zA-Z0-9._-]` with a
single `-`, trims leading/trailing `-`, `_`, and `.`, and lowercases the
result. So a bundle with `author.id: TAG-SC` and `id: cnsc` publishes to
`tag-sc/cnsc` — regardless of which organization you are a member of.

**This is the usual cause of a 403 on publish.** Authorization is per
*namespace* (the part before the `/`), so you must own — or hold a trusted
publisher binding for — the namespace the metadata names, not the one you
meant. If a bundle inherits `author.id` from an upstream source, the derived
namespace belongs to that upstream.

**The fix is to change `metadata.author.id` in the bundle** (or have the
binding registered for the namespace the metadata actually names).

> **`--repository` is not a workaround for a 403.** It overrides only the
> **OCI push destination** — the hub still indexes the artifact under
> `slugify(metadata.author.id)`. Pointing it at a namespace that disagrees
> with the metadata splits the two apart: the blobs land in one repository
> while the index row is written under another. The publish *appears* to
> succeed, the artifact does not show up where you aimed it, and re-running
> fails with a digest conflict. Use `--repository` only when it agrees with
> what the metadata derives.

### Reading artifacts: `unpack` vs `cat`

`unpack` **verifies the artifact's signature before writing anything** and fails
closed — an unsigned or mis-signed artifact is refused and no files land on disk
(same check as `verify`; `--no-verify` opts out, `--source` layouts have no
signature to check). It then writes the artifact's files **and** its
`bundle.json` manifest (with provenance) into a directory. `cat` streams the
**Gemara content only** to
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
`os.UserCacheDir()/grcli`) and grows without bound (no GC yet). Note: a default
`unpack` still contacts the hub/registry to *verify* the signature even on a
content cache hit (ADR-0048); `--no-verify` restores a fully offline cache hit.

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
- `trusted-root` (`GRCLI_TRUSTED_ROOT`) — path to a `trusted_root.json` that
  overrides the embedded Sigstore public-good trust root for keyless `verify`
  (ADR-0046). For air-gapped deployments or a private Sigstore instance only;
  unset, grcli uses its pinned embedded root.

> **Registry credentials are env-only, never config keys**: set
> `GRCLI_REGISTRY_TOKEN` (or `GRCLI_REGISTRY_USERNAME` +
> `GRCLI_REGISTRY_PASSWORD`) in the environment. A `registry-token:` line in a
> config file is ignored. The cache *location* is likewise set only by
> `$GRCLI_CACHE`. (The cache toggle key is the flat `cache-enabled`, not
> `cache.enabled`, because `$GRCLI_CACHE` would otherwise shadow a nested
> `cache.*` key.)

Signing is required by default, and `publish` fails *before* pushing when it
can't sign, so nothing unsigned reaches the registry. What counts as signing
material depends on where you run (`internal/sign.Preflight`):

| Where | Signing material | cosign on `PATH`? |
|---|---|---|
| GitHub Actions (`GITHUB_ACTIONS=true`) | the runner's OIDC token — needs `permissions: id-token: write` | **no** — in-process, ADR-0049 |
| Anywhere else | `--cosign-key` (or `COSIGN_KEY`) | **yes**, ≥ 2.6.0 |
| Either, opting out | `--no-sign` | no |

So a **local** `publish` with neither a key nor `--no-sign` is refused up
front — keyless signing is a CI-only path, because it depends on the
workflow's OIDC identity:

```
grcli: no signing material — pass --cosign-key (or COSIGN_KEY) for local
signing, run in GitHub Actions with `permissions: id-token: write` for
keyless signing, or pass --no-sign to publish without provenance
```

Keyless `verify` runs **in-process** against Sigstore (ADR-0046): no `cosign`,
no version-skew caveats, just the `grcli` binary. It embeds the pinned Sigstore
public-good trust root, refreshed with each grcli release; for an air-gapped or
private-Sigstore deployment, point `GRCLI_TRUSTED_ROOT` (env, or the
`trusted-root` config key) at a `trusted_root.json` on disk. Catalog signatures
use the **Sigstore bundle format** (v0.3, attached as an OCI 1.1 referrer);
artifacts signed by an older grcli (the legacy `.sig` tag format) must be
re-published to re-sign. Only key-based `verify --cosign-key` still shells out
to `cosign` ≥ 2.6.0 — a niche publisher-shared-key path.

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
under — which is derived from the bundle's own metadata, **not** chosen
by the workflow (see [Where a publish lands](#where-a-publish-lands-the---repository-default);
check it before registering the binding). Until that binding exists the
hub returns 403; *adding a GitHub secret will not fix it.*

```yaml
permissions:
  contents: read
  id-token: write   # OIDC token: hub auth + keyless signing (in-process)
                    # this is the ONLY auth grcli needs in CI

jobs:
  publish:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      # v2: https://github.com/oras-project/setup-oras/releases/tag/v2.0.0
      - uses: oras-project/setup-oras@38de303aac69abb66f3e6255b7198bff35f323e3
      - run: |
          oras pull ghcr.io/gemaraproj/grcli:latest --platform linux/amd64
          sudo install grcli /usr/local/bin/grcli
      # No cosign step: grcli signs keyless in-process via sigstore-go
      # (ADR-0049), using the same OIDC identity that authorizes the push.
      - run: grcli publish -f controls.yaml --license Apache-2.0
        # --license is REQUIRED (ADR-0037) — set it to your catalog's real
        # terms; no `env:` block, no `with: token:`, no secrets — the
        # id-token: write above is what makes this work
```

The Fulcio certificate records the workflow URL as the signer identity
(`https://github.com/<org>/<repo>/.github/workflows/publish.yml@<ref>`).
The hub verifies that signature at ingest and records the (ref-stripped)
identity, so a consumer can run `grcli verify --repository … --version …`
with **no trust flags** and grcli will verify against the recorded identity
(printing it, and that it came from the hub, first — ADR-0045). For an
independent check that does not trust the hub as the identity source, a
consumer supplies `--certificate-identity` (the workflow URL above) with the
issuer `https://token.actions.githubusercontent.com` themselves.

## License

Licensed under the [Apache License, Version 2.0](LICENSE).
