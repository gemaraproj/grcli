# Command reference

Generated from the code on `main`, not from `--help` alone. Every flag can
also be set by environment variable (`GRCLI_` plus the flag name in upper
snake case) or by a key of the same name in the
[config file](configuration.md), except where noted. Precedence: flag, then
environment, then config file, then default.

**Exit status.** grcli exits `0` on success and `1` on any error, printing
`grcli: <error>` on stderr. There are no other exit codes.

**Global flags.**

| Flag | Meaning |
|---|---|
| `--config <file>` | Read this config file instead of `~/.config/grcli/config.yaml`. |
| `-v`, `--version` | Print the grcli version. |
| `-h`, `--help` | Help for any command. |

Commands: [`validate`](#validate) · [`publish`](#publish) ·
[`verify`](#verify) · [`unpack`](#unpack) · [`cat`](#cat) ·
[`versions`](#versions) · [`login`](#login) · [`logout`](#logout)

---

## validate

```
grcli validate -f <file>[,<file>…] [--gemara-version <v> | --spec <dir>]
```

Scope: **General**. Checks each file against the Gemara CUE schema with
`cue vet`. Needs `cue` on `PATH`.

| Flag | Default | Meaning |
|---|---|---|
| `-f`, `--file` | (required) | Files to check. Repeatable; commas also split. |
| `--gemara-version` | each file's `metadata.gemara-version`, else `latest` | Check every file against this release (`1.6.0`, `v1.6.0`, `latest`, `v1.latest`). |
| `--spec` | (none) | A local checkout of the Gemara CUE module, used instead of the registry. Env: `GRCLI_GEMARA_SPEC_DIR`. |

For each file grcli:

1. Reads `metadata.type` and `metadata.gemara-version`. `type` must be a bare
   identifier; anything else fails before cue runs, because it is spliced
   into the cue expression.
2. Picks the version: `--gemara-version`, else the file's, else `latest`. A
   version must be `MAJOR.MINOR.PATCH[-pre]`, `latest` or `vN.latest`.
3. Prints a warning line (`WARN <file>: …`) for each grc.store reference URL
   that is not in canonical form (see
   [references](formats-and-protocol.md#reference-urls)).
4. Runs `cue vet -d '#<Type>' github.com/gemaraproj/gemara@v<version> <file>`
   in a fresh temporary directory (so a `cue.mod` in your project cannot
   interfere), or `cue vet -d '#<Type>' . <file>` inside `--spec`. Each call
   times out after 30 seconds.

**Output** (stdout): cue's own messages, then one line per file:

```
OK   controls.yaml (#ControlCatalog, gemara v1.6.0)
FAIL bad.yaml (#ControlCatalog, gemara v1.6.0): cue vet exited with: exit status 1
```

With `--spec`, the label has no version: `(#ControlCatalog)`.

**Fails** if any file fails (`N of M file(s) failed validation`), if `cue` is
missing, or if `--spec` and `--gemara-version` are both passed as flags. If
the spec directory comes from the environment or config and a version is
also set there, the spec directory wins and a warning goes to stderr.

---

## publish

```
grcli publish [<file>…] [-f <file>…] --license <SPDX> [flags]
```

Scope: **Hub protocol**, **Sigstore**; keyless signing is **GitHub Actions**
only; `--dry-run` is **General**.

| Flag | Default | Meaning |
|---|---|---|
| `-f`, `--file` | (none) | Input files describing one artifact. Repeatable; commas split. Cannot be combined with positional files. |
| `--license` | (required) | Publication license, an SPDX expression. Canonicalised and stored as the manifest annotation `org.opencontainers.image.licenses`. |
| `--url` | `https://hub.grc.store` | Hub base URL. Used for discovery, the version check, registry tokens and the sync. |
| `--version` | `metadata.version` | Written into the packed body's `metadata.version` and used as the tag. |
| `--repository` | `slugify(author.id)/slugify(id)` | Served repository. The hub rejects a value that disagrees with the metadata (`422 catalog_coordinate_mismatch`). |
| `--token` | (none) | Hub bearer token. Env: `GRCLI_TOKEN`. In Actions, leave unset. |
| `--cosign-key` | (none) | cosign private key for key-based signing. Env: `COSIGN_KEY`, cosign's own name for it. Refused inside GitHub Actions. |
| `--no-sign` | `false` | Do not sign. |
| `--dry-run` | `false` | No network. Write an OCI image layout to `--output`. |
| `--output` | `grcli-out` | Layout directory for `--dry-run`. |

Steps, in order (each stops the run on failure):

1. Load the files (one artifact; `ControlCatalog`/`GuidanceCatalog` may merge
   several files, see [source loading](formats-and-protocol.md#source-loading)).
2. Apply `--version`, printing `note: --version X overrides metadata.version Y`
   on stderr when it changes the value.
3. Warn on stderr (`warning: …`) about non-canonical grc.store reference URLs.
4. Resolve the target: tag, repository, staging repository
   `staging/<repository>`; unless `--dry-run`, discover the registry from the
   hub.
5. Validate `--license`.
6. Unless `--dry-run`:
   - signing preflight (see [signing modes](#signing-modes));
   - `GET /v1/catalogs/<ns>/<id>/versions/<v>` on the hub: `200` stops with
     *already exists*, `410` with *was yanked*;
   - mint a registry push token for the staging repository (unless
     `GRCLI_REGISTRY_*` is set).
7. Build the provenance record and pack the bundle.
8. Push to `staging/<ns>/<id>:<version>`, or write the layout (`--dry-run`,
   which ends here).
9. Sign the pushed manifest digest.
10. `POST /v1/bundles/sync` naming the served repository and tag.

Publish does **not** run schema validation; the hub does that at sync.

**Output.** Dry run (stdout):

```
dry-run: wrote bundle to oci:grcli-out:0.1.0
  manifest digest: sha256:…
  body digest:     sha256:…
  artifact: ControlCatalog/example-controls
```

Real publish (stdout):

```
pushed <registry>/staging/<ns>/<id>:<v>
  manifest digest: sha256:…
signed (keyless)                       # or: signed (key) / signing skipped: --no-sign
hub indexed <ns>/<id>:<v> — 1 artifacts (1 new), types=ControlCatalog
```

With `--cosign-key`, cosign's own output appears between the `pushed` and
`signed` lines.

### Signing modes

| Condition | Mode | Needs |
|---|---|---|
| `--no-sign` | skipped | nothing |
| `GITHUB_ACTIONS=true` and `--cosign-key` set | error: refused | unset `COSIGN_KEY` for the step, or `--no-sign` |
| `GITHUB_ACTIONS=true` | keyless, in-process | `ACTIONS_ID_TOKEN_REQUEST_URL` and `_TOKEN` (from `id-token: write`) |
| otherwise, `--cosign-key` set | key, via `cosign sign` | cosign 2.6.0 or later |
| otherwise | error: `no signing material` | |

Inside GitHub Actions a key is refused, not ignored: Actions publishes sign keyless
with the workflow's identity, which is what the hub records and `verify` checks.

### Hub token resolution

For the push token and the sync, grcli resolves a hub bearer token in this
order: `--token` / `GRCLI_TOKEN` / a `token:` config key; then, inside
GitHub Actions, a fresh OIDC token for the hub's advertised `ci_audience`
(falling back to the `--url` value); then the stored `grcli login`
credentials for the hub's OIDC issuer, refreshed if expired.

---

## verify

```
grcli verify --repository <ns>/<id> --version <v> [trust flags]
```

Scope: **Hub protocol** (discovery always; signer lookup in zero-flag mode),
**Sigstore**.

| Flag | Default | Meaning |
|---|---|---|
| `--repository` | (required) | `<ns>/<id>`. |
| `--version` | (required) | The version (tag). |
| `--url` | `https://hub.grc.store` | Hub, for discovery and the signer lookup. |
| `--certificate-identity` | (none) | Exact expected certificate subject (SAN). Selects explicit keyless mode. |
| `--certificate-oidc-issuer` | GitHub Actions issuer, in explicit keyless mode | Expected OIDC issuer. In zero-flag mode a set issuer (flag, env or config) must match the hub record. The flag conflicts with `--cosign-key`; env or config does not. |
| `--cosign-key` | (none) | cosign public key. Selects key mode. Exclusive with the two flags above. |
| (config/env only) `trusted-root` / `GRCLI_TRUSTED_ROOT` | embedded root | Path to a `trusted_root.json`. |

Modes:

| Mode | Selected by | Identity checked | Runs |
|---|---|---|---|
| Zero-flag | no trust flags | the hub's recorded signer for `<ns>/<id>`: issuer exact, SAN matching `^<workflow-path>@` (any ref) | in-process |
| Explicit keyless | `--certificate-identity` | SAN exact, issuer exact | in-process |
| Key | `--cosign-key` | the key | `cosign verify` |

In-process verification requires a Rekor entry, an observed timestamp and an
SCT, against the embedded Sigstore public-good trust root (from
`grc-store-clientkit/trustroot`) or `GRCLI_TRUSTED_ROOT`.

**Output** (stdout):

```
verifying <registry>/<ns>/<id>:<v> (keyless identity from hub record: <identity>, issuer <issuer>)
verified: keyless:<issuer>#<workflow-path>
```

The first line's parenthesis reads `keyless identity=<SAN> issuer=<issuer>`
in explicit mode and `key=<path>` in key mode. Key mode prints cosign's
output instead of the `verified:` line.

Verify does not support a local layout: a signature exists only in a
registry.

---

## unpack

```
grcli unpack --repository <ns>/<id> --version <v> [flags]
grcli unpack --source <layout-dir> --version <v> [flags]
```

Scope: **Hub protocol** with `--url`; **General** with `--source`.

| Flag | Default | Meaning |
|---|---|---|
| `--version` | (required) | Version (tag). |
| `--repository` | (required with `--url`) | `<ns>/<id>`. |
| `--url` | `https://hub.grc.store` | Hub. Ignored when `--source` is given and `--url` is not. |
| `--source` | (none) | OCI image layout directory. Exclusive with an explicit `--url`. |
| `--output` | `grcli-unpacked` | Directory to write into. |
| `--no-verify` | `false` | Skip signature verification (warns). |
| `--certificate-identity`, `--certificate-oidc-issuer` | (none) | As for `verify`; replaces the hub lookup. |
| `--with-imports` | `false` | Also pull references targeted by `imports`. |
| `--with-references` | `false` | Also pull every mapping reference. Wins over `--with-imports`. |
| `--no-cache` | `false` | Do not read or write the cache this run. |

Steps:

1. Fetch the bundle (cache first, then registry; see
   [cache](formats-and-protocol.md#cache)).
2. Verify the signature as `verify` does (zero-flag or explicit keyless; key
   mode is not offered), unless `--no-verify` or `--source`. On failure,
   nothing is written and the output directory is not created.
3. Write the artifact file and `bundle.json` to `--output`. File names that
   would escape the directory are refused.
4. With `--with-*`, resolve references (see
   [references](formats-and-protocol.md#referencesindexjson)).

**Output** (stdout):

```
verifying signature (keyless identity from hub record: …)
verified: keyless:…
unpacked <hub-host>/<ns>/<id>:<v> → grcli-unpacked (1 files, 0 imports)
  - controls.yaml
  - bundle.json (bundle manifest)
```

Reference resolution adds lines such as `  - skip [reference] <url>: <reason>`,
`  ! license: … — review before reuse`, `  - references/index.json` and
`resolved N reference(s), skipped M`.

---

## cat

```
grcli cat --repository <ns>/<id> --version <v> [--file <name>]
grcli cat --source <layout-dir> --version <v> [--file <name>]
```

Scope: as `unpack`.

| Flag | Default | Meaning |
|---|---|---|
| `--version`, `--repository`, `--url`, `--source`, `--no-cache` | | As for `unpack`. |
| `--file` | (none) | Fail unless the artifact's file name is this. Read from the command line only, never from env or config. |

Writes the artifact's bytes to stdout, unchanged. Never writes
`bundle.json`, never verifies the signature, never resolves references.
Cache diagnostics and a note about omitted imports go to stderr.

---

## versions

```
grcli versions <ns>/<id> [--latest]
```

Alias: `releases`. Scope: **Hub protocol**. No login needed.

| Flag | Default | Meaning |
|---|---|---|
| `--url` | `https://hub.grc.store` | Hub. |
| `--latest` | `false` | Print only the latest version. |

Calls `GET /v1/catalogs/<ns>/<id>`. **Output:**

```
VERSION             PUSHED                       DIGEST
1.1.0 (latest)      2026-07-06T02:27:18.361705Z  sha256:b87a3a61f910
1.0.0               2026-06-22T13:06:34.340457Z  sha256:2e7f2af8d942
```

Rows are in the hub's order (newest first); digests are shortened to 12 hex
characters. `--latest` prints one bare version, or nothing (exit `0`) if the
catalog has no releases. Without `--latest`, no releases is an error.
`catalog not found` and `catalog was yanked` are errors.

---

## login

```
grcli login [--url <hub>]
```

Scope: **Hub protocol** (the hub must advertise `oidc_issuer` and
`oidc_cli_client_id`).

Runs the OAuth 2.0 device authorization grant (RFC 8628) against the hub's
OIDC issuer and stores the access and refresh tokens. **Output:**

```
Discovering https://hub.grc.store ...

Open this URL in any browser to authorize:
  https://auth.grc.store/realms/…/device?user_code=ABCD-EFGH
Or visit https://auth.grc.store/realms/…/device and enter code:  ABCD-EFGH
(code expires in 10m0s)
Waiting for authorization...

✓ Signed in to https://auth.grc.store/realms/…
  Token stored at ~/.local/share/grcli/credentials.json (expires 2026-…)
```

Credentials are keyed by issuer; logging in again replaces them.

---

## logout

```
grcli logout [--url <hub> | --issuer <issuer-url>]
```

Scope: **Hub protocol** with `--url`; local only with `--issuer`.

Deletes the stored credentials for the hub's issuer. Does not contact the
issuer or revoke anything. Pass `--issuer` when the hub is unreachable.
**Output:** `✓ Forgot credentials for <issuer>`.
