# Configuration and environment reference

## Where settings come from

Highest precedence first:

1. A flag on the command line.
2. An environment variable, `GRCLI_` plus the setting's name in upper snake
   case (`--cache-enabled` → `GRCLI_CACHE_ENABLED`, `--url` → `GRCLI_URL`).
3. The config file.
4. The built-in default.

There is one config file, read from:

- `--config <file>` if given (that file only), else
- `$XDG_CONFIG_HOME/grcli/config.yaml`, else
- `~/.config/grcli/config.yaml`.

A missing file is fine. An unreadable or malformed one produces a warning on
stderr and the command carries on with flags and environment.

grcli does **not** read `./.grcli.yaml`, `~/.grcli.yaml`, or
`$XDG_CONFIG_HOME/grcli/.grcli.yaml`. If any of those exists it prints
`grcli: warning: ignoring config <path> — grcli reads only <global path>`.
To use a file kept in a repository, pass it with `--config`.

## Config file keys

The file is YAML. Any flag name is accepted as a key, and applies to every
command that has a flag of that name (so `url:` sets the hub for all of
them). The keys that mean something on their own:

| Key | Env | Default | Effect |
|---|---|---|---|
| `url` | `GRCLI_URL` | `https://hub.grc.store` | Hub base URL for every command. |
| `cache-enabled` | `GRCLI_CACHE_ENABLED` | `true` | `false` acts like `--no-cache` on every run. Flat on purpose: a nested `cache.enabled` would be hidden by `GRCLI_CACHE`. |
| `certificate-oidc-issuer` | `GRCLI_CERTIFICATE_OIDC_ISSUER` | GitHub Actions issuer | Issuer for explicit keyless verify. In zero-flag `verify` and `unpack` a set issuer must match the issuer the hub recorded. Key mode ignores it here; only the `--certificate-oidc-issuer` flag conflicts with `--cosign-key`. |
| `trusted-root` | `GRCLI_TRUSTED_ROOT` | embedded | Path to a Sigstore `trusted_root.json` for verifying. Has no flag. |
| `gemara-version` | `GRCLI_GEMARA_VERSION` | per file | Schema release for `validate`. |
| `spec` | `GRCLI_SPEC` (or `GRCLI_GEMARA_SPEC_DIR`) | none | Local Gemara CUE module for `validate`. |
| `token` | `GRCLI_TOKEN` | none | Hub bearer token. Works from the file, but a token belongs in the environment. |
| `file`, `license`, `repository`, `version`, … | | | Per-command flag values. Useful in a file passed with `--config` as a publish manifest. |

Example `~/.config/grcli/config.yaml`:

```yaml
url: https://hub.preview.grc.store
cache-enabled: true
```

## Environment-only variables

These are read directly, never from the config file.

| Variable | Used by | Effect |
|---|---|---|
| `GRCLI_REGISTRY_TOKEN` | all registry access | Bearer token for the registry. If set, grcli does not ask the hub for one. grcli also sets it itself after minting one. |
| `GRCLI_REGISTRY_USERNAME`, `GRCLI_REGISTRY_PASSWORD` | all registry access | Basic auth for the registry; wins over the token. |
| `GRCLI_CACHE` | `cat`, `unpack` | Cache directory. |
| `GRCLI_FULCIO_URL` | keyless `publish` | Fulcio. Default `https://fulcio.sigstore.dev`. |
| `GRCLI_REKOR_URL` | keyless `publish` | Rekor. Default `https://rekor.sigstore.dev`. |
| `GRCLI_GEMARA_SPEC_DIR` | `validate` | Same as `--spec`. |
| `COSIGN_KEY` | `publish` | Same as `--cosign-key`. |
| `CUE_REGISTRY` | `validate` (read by cue) | CUE module registry mirror. |
| `XDG_CONFIG_HOME`, `XDG_DATA_HOME` | all | Base directories for config and credentials. |
| `GITHUB_ACTIONS` | `publish` | `true` selects keyless signing and the CI token path. |
| `ACTIONS_ID_TOKEN_REQUEST_URL`, `ACTIONS_ID_TOKEN_REQUEST_TOKEN` | `publish` | Set by GitHub when the job has `id-token: write`. |
| `GITHUB_SERVER_URL`, `GITHUB_REPOSITORY`, `GITHUB_RUN_ID`, `GITHUB_RUN_ATTEMPT`, `GITHUB_WORKFLOW`, `GITHUB_REF`, `GITHUB_SHA`, `GITHUB_ACTOR`, `RUNNER_OS`, `CI` | `publish` | Copied into the provenance record. |
| `USER` / `USERNAME` | `publish` outside Actions | Recorded in the provenance builder id. |

## Files grcli reads and writes

| Path | What | Mode |
|---|---|---|
| `~/.config/grcli/config.yaml` | config (read only) | |
| `${XDG_DATA_HOME:-~/.local/share}/grcli/credentials.json` | `login` tokens, one entry per OIDC issuer | `0600` |
| `${GRCLI_CACHE:-<user cache dir>/grcli}/v2/…` | fetched bundles (see [cache](formats-and-protocol.md#cache)) | `0644` |
| `./grcli-out/` (or `--output`) | `publish --dry-run` OCI layout | |
| `./grcli-unpacked/` (or `--output`) | `unpack` output | |
| `~/.docker/config.json` and its helpers | registry credentials, as a fallback | read only |

`<user cache dir>` is `~/Library/Caches` on macOS, `$XDG_CACHE_HOME` or
`~/.cache` on Linux, `%LocalAppData%` on Windows.
