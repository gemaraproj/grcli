# Known gaps and surprises

Things a maintainer should know that the code does not make obvious: where
the docs and the behaviour disagree, code nothing uses, and behaviour that
is easy to trip over. Found while writing these docs from the code on
`main`. Each item says where to look.

## Behaviour that is easy to trip over

### Local publishing cannot succeed against grc.store

`grcli publish --cosign-key <key>` (key-signed) and `--no-sign` (unsigned)
both push to staging and then fail at sync: the grc.store hub accepts only keyless (certificate) signatures and
refuses unsigned catalogs (`catalog_verification_failed`,
`catalog_unsigned`). The only publish path that reaches a grc.store hub is
keyless in GitHub Actions. `grcli login` is consequently useful today only
for minting a push token that leads to a refused sync.

### No release carries the staging change

`publish` pushes to `staging/<ns>/<id>` only from commit `113ffc9`
(CHANGELOG "Unreleased"). That commit is tagged `v0.3.0-rc.1`, but no image
was published for it: `ghcr.io/gemaraproj/grcli` holds only `v0.1.0`,
`v0.2.0` and `latest`, and both releases push to the served repository,
which a cut-over hub refuses with `401`. `v0.1.0` is also the pin in
`examples/github-actions/publish.yml` and the reusable workflow's
`grcli-version` default, and the README's install examples pull `latest`.

The CHANGELOG heads `v0.1.0` as "Unreleased" and has no `v0.2.0` entry.

The repository still carries tags from before the history was reset on
2026-09-03: `v0.1.1`, `v0.1.2`, `v0.2.1` to `v0.2.3`, and `v0.3.0` to
`v0.7.0` point at commits that are not on `main`. `v0.3.0` already exists
as one of them, so the next minor release cannot use that name without
first deleting the old tag.

### `--repository` cannot change the coordinate

A `--repository` that differs from the coordinate derived from the file
pushes to staging and is then refused at sync with
`422 catalog_coordinate_mismatch`, because the hub authorizes the namespace
it derives from the body.

### Help text that is out of date

- `verify --help` says key mode needs "cosign >= 3.x"; the code accepts 2.6.0
  and up.
- The cosign version error says "install a released cosign ≥ 2.4.0"; the
  floor is 2.6.0 (`internal/sign/sign.go`, `detectCosignVersion`).
- `publish --no-sign` says "skip cosign signing"; keyless signing does not
  use cosign.
- `unpack --help` renders `--with-imports imports`, as if the flag took a
  value: the back-quoted word in its usage string is read by cobra as a
  value name.
- `cat --help` shows an example picking "one file out of several" with
  `--file`; a bundle now holds exactly one artifact file.

## Reads are always anonymous

`verify`, `unpack`, `cat` and `versions` never send the `grcli login` token.
Pull tokens are minted with an empty bearer (`ensureRegistryToken(…, "", …)`
in `cmd/fetch.go`, `cmd/verify.go`, `cmd/unpack.go`), and catalog lookups use
`hub.New(url, "")`. On a hub with anonymous reads turned off, or for a
private namespace, every read fails, logged in or not.

## Surprising behaviour

- **`publish` does not validate the schema.** The hub does, at sync, after
  grcli has pushed and signed. A schema error therefore costs a push, a
  Fulcio certificate and a Rekor entry before it is reported. Run
  `grcli validate` first in CI.
- **Global config keys reach every command.** Viper binds flags by name,
  and one viper instance serves all commands. A `version:` or `repository:`
  key in `~/.config/grcli/config.yaml` applies to every `publish`,
  `verify`, `unpack` and `cat`; a `token:` key is used as a hub bearer
  token. Only `cat --file` is protected from this.
- **Local provenance names your machine.** Outside GitHub Actions the
  builder id is `local://<user>@<hostname>`, and input files are recorded as
  `file://<path as typed>`, which for an absolute path includes your home
  directory. Both are inside the signed, immutable bundle.
- **Git provenance comes from the working directory**, not the directory of
  the input file.
- **Merged catalogs take `version` and `author.id` from the first file**, and
  the merged body is re-marshalled, losing comments and key order.
- **Signing happens after the push.** A signing failure leaves an unsigned
  bundle in staging until the hub or the 24-hour retention removes it. The
  pre-flight check makes this rare, not impossible.
- **Credentials are keyed by OIDC issuer**, not hub URL, so two hubs that
  share a login server share one login.
- **The cache keys on the hub host from `--url`.** `hub.grc.store` and
  `https://hub.grc.store/` share entries; two URLs for the same hub on
  different hosts do not. Path components are sanitised by replacing `/`,
  `\` and `..` with `_`, so two coordinates that differ only there would
  share an entry. Nothing is ever evicted.
- **Zero-flag verify checks only the certificate SAN.** For a publish made
  through a shared reusable workflow, the SAN names that workflow, not the
  calling repository; the hub records the caller separately, but grcli does
  not check it.
- **`unpack --source` with `--with-*`** prints that resolution needs a hub
  and writes the primary only; it is not an error.

## Dead or vestigial code

- `hub.Client.GetVersionBody` (`internal/hub/hub.go`): called only by tests.
- `cache.Cache.Root` (`internal/cache/cache.go`): never called.
- `cache.Entry.Verified` and `references/index.json`'s `verified`: always
  `false`; kept for a verify-on-pull pass that does not exist.
- Bundle `Imports`: grcli never packs imports, but `unpack` writes them to
  `imports/`, `cat` warns about them, and the cache refuses bundles that have
  them. Only a bundle built by another tool can carry them.
- `sign.FlagNewBundleFormat` is exported "so the verify side references the
  same constant"; the verify side calls `BundleFormatArgs` instead.
- `internal/sign/keyless.go` has its own GitHub Actions OIDC token fetch,
  next to the one in `grc-store-clientkit/auth` that `publish` uses for the
  hub token.
- Comments that describe retired behaviour: `mergeFileSources` mentions
  `.grcli.yaml` setting `file:`; `cat.go` mentions `file:` in `.grcli.yaml`;
  `PushRemote` says `--tag is required`, a flag that no longer exists.
