# How to fix a failed publish

Scope: **Hub protocol**. Codes in the last section come from the hub, via
the shared error catalogue in `grc-store-protocol/apierror`.

`grcli publish` fails at one of five stages. The message tells you which.

## 1. Before any network call

| Message starts with | Fix |
|---|---|
| `a publication license is required` | Add `--license <SPDX expression>`. |
| `invalid --license … unknown SPDX id` | Check the id at <https://spdx.org/licenses>, or use `LicenseRef-<name>`. |
| `could not determine tag — metadata.version is required` | Set `metadata.version` or pass `--version`. |
| `could not determine --repository` | Set `metadata.author.id` and `metadata.id`. |
| `… declares type … but … declares …` | All files given to one publish must have the same `metadata.type` and `metadata.id`. |
| `artifact type "…" does not support multi-file merge` | Only `ControlCatalog` and `GuidanceCatalog` merge. Publish one file. |
| `repository "…" must be <namespace>/<name>` | `--repository` (or the derived one) must have exactly a namespace and a name, and the namespace must not be `staging`. |
| `no signing material` | Outside GitHub Actions there is no keyless signing. Run in Actions with `id-token: write`. |
| `GITHUB_ACTIONS=true but ACTIONS_ID_TOKEN_REQUEST_TOKEN is unset` | Add `permissions: id-token: write` to the job. |

## 2. Version check

| Message | Fix |
|---|---|
| `<repo>:<v> already exists — versions are immutable` | Bump the version. |
| `<repo>:<v> was yanked and cannot be republished` | Bump the version. A yanked version stays taken. |

## 3. Registry token and push

| Message | Fix |
|---|---|
| `registry push needs a hub login to mint a push token: no token available` | In Actions: check `id-token: write`. Elsewhere: `grcli login`. |
| `push: the registry refused the push to staging repository staging/<ns>/<id> (401)` | You do not own `<ns>` (or your repo is not its trusted publisher), or the hub predates the staging cutover. |
| `401 UNAUTHORIZED` at the first blob push, naming `<ns>/<id>` (no `staging/`) | Your grcli predates the staging cutover; the hub no longer accepts pushes to served repositories. Upgrade grcli. |

## 4. Signing

| Message | Fix |
|---|---|
| `keyless sign: … GitHub Actions OIDC token endpoint returned …` | The runner could not mint a Sigstore token. Retry; check `id-token: write`. |
| `attaching signature to registry` | The push token expired or lost its grant between push and sign. Run publish again. |

## 5. Hub sync

The sync error names the hub's code in parentheses, for example
`refused acme/cat:1.0.0 (422 catalog_unsigned): …`, and ends with what to do.
After any refusal nothing was published; you fix the cause and run
`grcli publish` again (not just the sync).

| Status / code | Cause | Fix |
|---|---|---|
| `401` | Login missing or expired, or the CI token's audience is wrong. | `grcli login`, or check `id-token: write`. |
| `403` | Your principal may not publish to the derived namespace. | Register the repository as trusted publisher for that namespace, or change `metadata.author.id`. |
| `422 catalog_coordinate_mismatch` | `--repository` disagrees with `slugify(metadata.author.id)`. | Drop `--repository`, or change `author.id`. |
| `422 tag_version_mismatch` | Tag differs from `metadata.version`. | Does not happen with grcli unless the body was edited between steps. |
| `422 catalog_unsigned` | Published with `--no-sign`. | The hub accepts only signed catalogs; publish from Actions. |
| `422 catalog_verification_failed` | Signature does not verify, or is key-based (the hub accepts only keyless). | Publish keyless from Actions. |
| `422 catalog_signer_mismatch` | A different workflow signed earlier versions of this coordinate, and the hub pinned that one. | Publish from the pinned workflow, or have a namespace admin reset the signer pin. |
| `422 license_required` | No licence annotation reached the hub. | Should not happen with grcli; check you are not publishing with another tool. |
| `422 schema_invalid` | The body fails the Gemara release it declares. | Run `grcli validate` with the same `gemara-version`. |
| `422 gemara_version_unresolved` | `metadata.gemara-version` is not `MAJOR.MINOR.PATCH`. | Quote a full version, e.g. `"1.6.0"`. |
| `422 gemara_version_unsupported` | The declared Gemara line is older than the hub's minimum (`1.5`). | Move the artifact to Gemara 1.5 or later. |
| `fetch_failed` | The hub could not read the staged bundle (the push did not finish, or staging removed it before the sync). | Publish again. |
| `429`, `5xx` | Rate limit or hub fault. The staged upload is kept. | Publish again in a moment. |

## The publish "worked" but the artifact is not where you expected

The coordinate comes from `metadata.author.id` and `metadata.id`, not from
the namespace you meant. Run with `--dry-run`, unpack the layout it writes
with `grcli unpack --source <layout> --version <v>`, and read
`metadata.provenance.buildDefinition.externalParameters.target.repository`
in the `bundle.json` it writes to see the derived value.
