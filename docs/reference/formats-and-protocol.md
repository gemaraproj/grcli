# Formats and protocol reference

What grcli sends, receives, writes and reads. Shared wire types come from
[`grc-store-protocol`](https://github.com/revanite-io/grc-store-protocol)
(v0.28.1); the bundle format comes from
[`go-gemara`](https://github.com/gemaraproj/go-gemara) (v0.9.2).

## Hub endpoints grcli calls

Scope: **Hub protocol**. All paths are relative to `--url`.

| Method and path | Called by | Auth | grcli uses |
|---|---|---|---|
| `GET /.well-known/grc-store-configuration` | every hub command | none | `registry_url` (required), `oidc_issuer`, `oidc_cli_client_id`, `ci_audience`. Cached per URL for the life of the process. 10 s timeout. |
| `GET /v2/token?scope=repository:<repo>:<actions>` | `publish` (pull,push), `verify`/`unpack`/`cat` (pull) | Bearer for push; none for pull | `token` or `access_token` from the response. 10 s timeout. |
| `GET /v1/catalogs/<ns>/<id>/versions/<v>` | `publish` pre-flight | none | Status only: `200` taken, `404` free, `410` yanked. |
| `GET /v1/catalogs/<ns>/<id>` | `versions`, zero-flag `verify`/`unpack`, reference licence lookup | none | `latest_version`, `releases[]` (`version`, `manifest_digest`, `pushed_at`, `license`), `signer_identity`. |
| `POST /v1/bundles/sync` | `publish` | Bearer (required) | Request `{"repository": "<ns>/<id>", "tag": "<v>"}`; response `repository`, `tag`, `artifact_count`, `new_count`, `types`. Errors are `{"error": "<code>", "detail": "…"}`. |

Hub calls other than discovery and `/v2/token` time out after 3 minutes,
above the hub's 120-second sync deadline.

Read calls (`/v2/token` for pull, `/v1/catalogs/…`) are always anonymous,
even after `grcli login`. See
[known gaps](../explanation/known-gaps.md#reads-are-always-anonymous).

### Discovery document

```json
{
  "registry_url": "https://oci.grc.store",
  "hub_url": "https://hub.grc.store",
  "api_version": "v1",
  "oidc_issuer": "https://auth.grc.store/realms/…",
  "oidc_cli_client_id": "grcli",
  "ci_audience": "https://hub.grc.store"
}
```

The type also carries `ui_url` and settings flags (`private_only`,
`anonymous_reads`, …) that grcli ignores. A `registry_url` starting with
`http://` makes grcli use plain HTTP to the registry.

### Repositories

| Name | Form | Who writes |
|---|---|---|
| Served | `<ns>/<id>` | the hub only |
| Staging | `staging/<ns>/<id>` (`staging.Repository`) | the publisher, with a push token |

The namespace `staging` is reserved. Staged uploads are deleted by the hub
after it decides, and by the registry after about 24 hours.

## The bundle

Scope: **General** (any OCI 1.1 registry or image layout can store it).

An OCI image manifest (`PackManifestVersion1_1`):

| Part | Media type | Content |
|---|---|---|
| manifest `artifactType` | `application/vnd.gemara.bundle.v1` | |
| config | `application/vnd.gemara.manifest.v1+json` | the bundle manifest, below (`bundle.json` when unpacked) |
| one layer | `application/vnd.gemara.artifact.v1+yaml` | the artifact's YAML, with its file name as the title annotation |
| manifest annotation `org.opencontainers.image.licenses` | | the canonical SPDX licence |

Tag: `metadata.version`.

### Bundle manifest (`bundle.json`)

```json
{
  "bundle-version": "1.0",
  "gemara-version": "1.6.0",
  "metadata": { "provenance": { … } },
  "artifacts": [
    { "name": "controls.yaml", "type": "ControlCatalog", "id": "example-controls", "role": "artifact" }
  ]
}
```

### Provenance record

`metadata.provenance` is shaped like the SLSA v1.0 provenance predicate
(`https://slsa.dev/provenance/v1`) but is **not** an in-toto attestation:
it sits unsigned inside the bundle manifest. It is covered by the signature
only because the signature covers the manifest digest, which covers the
config blob.

| Field | Value |
|---|---|
| `buildDefinition.buildType` | `https://grc.store/grcli/buildtype/v0` (**grc.store**) |
| `buildDefinition.externalParameters.artifact` | `{type, id}` |
| `buildDefinition.externalParameters.target` | `{registry, repository, tag}`: the **served** repository, and an empty registry on a dry run |
| `buildDefinition.internalParameters` | the allow-listed `GITHUB_*`, `RUNNER_OS`, `CI` variables that are set |
| `buildDefinition.resolvedDependencies` | each input file as `file://<path as given>` with its sha256, plus `git+<origin-url>@<HEAD>` when the current directory is a git checkout (credentials stripped from the URL) |
| `runDetails.builder.id` | `<GITHUB_SERVER_URL>/<repo>/actions/runs/<run-id>` in Actions; `local://<user>@<hostname>` otherwise |
| `runDetails.builder.version` | `grcli`, `go`, `go-os`, `go-arch` |
| `runDetails.metadata` | `invocationId` (`<run-id>-<attempt>`), `startedOn`, `finishedOn` |
| `runDetails.byproducts` | the packed body's name and sha256 |

### Signature

Scope: **Sigstore**.

A Sigstore bundle v0.3 (`application/vnd.dev.sigstore.bundle.v0.3+json`),
attached as an OCI 1.1 referrer of the bundle manifest:

- referrer manifest `artifactType`: `application/vnd.dev.sigstore.bundle.v0.3+json`
  when grcli signs keylessly; cosign 2.6.x stamps `https://sigstore.dev/cosign/sign/v1`
  instead. grcli's verifier accepts both and uses the first it finds.
- one layer of the same media type holding the bundle JSON (read with a size
  cap from `limits.MaxPluginBlobBytes`).
- the signed content, for keyless: a DSSE envelope (`application/vnd.in-toto+json`)
  around an in-toto Statement v1 whose one subject is the manifest's sha256
  digest, with predicate type `https://sigstore.dev/cosign/sign/v1` and an
  empty predicate. This matches what `cosign sign` produces.

The canonical signer identity (`grc-store-protocol/identity`):
`keyless:<issuer>#<SAN without @ref>`, for example
`keyless:https://token.actions.githubusercontent.com#https://github.com/acme/cat/.github/workflows/publish.yml`.

## Source loading

`publish` accepts several files only for `ControlCatalog` and
`GuidanceCatalog`, merged with go-gemara's `LoadFiles` and re-marshalled to
`control-catalog.yaml` / `guidance-catalog.yaml`. All files must share
`metadata.type` and `metadata.id`. A single file is packed byte for byte
under its base name (unless `--version` rewrites it).

## Reference URLs

A `metadata.mapping-references[].url` names a hub artifact when its host is
`grc.store`, a subdomain of it, or the host of `--url`, and
`grc-store-protocol/refurl` can read `/<ns>/<id>` from its path (it also
accepts `/v1/catalogs/<ns>/<id>`, `/search/<ns>/<id>` and mixed case). The
canonical form, the only one that earns no warning, is
`https://grc.store/<ns>/<id>` with the version in the `version` field.

## `references/index.json`

Written by `unpack --with-*` when at least one reference was pulled. An
array of:

```json
{
  "category": "imports",
  "namespace": "acme",
  "catalog_id": "baseline",
  "version": "2.1.0",
  "source_url": "https://grc.store/acme/baseline",
  "manifest_digest": "sha256:…",
  "content_digest": "sha256:…",
  "license": "Apache-2.0",
  "verified": false,
  "path": "references/imports/acme/baseline@2.1.0"
}
```

`content_digest` is the sha256 of the reference's `bundle.json` when it has
one, else of its single file. `verified` is always `false` today.

## Cache

```
<cache root>/v2/<hub host>/<ns>/<id>/<version>/
  meta.json        names, digests, manifest digest, license, flags
  bundle.json      the bundle manifest
  files/0          the artifact file
```

- Keyed by the **hub** host from `--url`, not the registry host.
- Path segments have `/`, `\` and `..` replaced with `_`.
- Each blob's sha256 is recorded in `meta.json` and checked on every read; a
  mismatch is treated as a miss and re-fetched.
- `meta.json` is written last, so a half-written entry reads as a miss.
- Bundles carrying imports are not cached. `v1` entries are ignored.
- Nothing is ever evicted.
