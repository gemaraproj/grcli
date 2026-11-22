# How to publish an artifact

Scope: **Hub protocol**, **GitHub Actions**, **Sigstore**.

## Check where it will land

grcli does not ask where to publish. It derives the coordinate from the file:

```
<namespace>/<id>  =  slugify(metadata.author.id) / slugify(metadata.id)
version           =  metadata.version   (or --version)
```

`slugify` replaces each run of characters outside `[a-zA-Z0-9._-]` with
`-`, trims `-`, `_` and `.` from both ends, and lowercases. `author.id:
TAG-SC`, `id: cnsc` lands at `tag-sc/cnsc`.

You must own that namespace, or your repository must be its trusted CI
publisher. If the file inherited `author.id` from someone else's catalog,
the namespace is theirs: change `author.id`.

Run a dry run to see the bundle grcli would build:

```sh
grcli publish -f controls.yaml --license Apache-2.0 --dry-run
```

## Publish from GitHub Actions (the supported path)

1. Have a namespace admin register your repository (`owner/repo`,
   optionally pinned to one ref such as `refs/heads/main`) as a trusted CI
   publisher for the namespace. This is done on the hub, not in your
   repository.
2. Give the job `permissions: id-token: write`. Add no secrets.
3. Run `grcli publish -f <file> --license <SPDX> --url <hub>`.

You can copy `examples/github-actions/publish.yml`, or call the reusable
workflow:

```yaml
jobs:
  publish:
    permissions:
      contents: read
      id-token: write
    uses: gemaraproj/grcli/.github/workflows/publish-gemara.yml@<ref>
    with:
      files: controls.yaml          # whitespace-separated; one publish per file
      license: Apache-2.0
      grcli-version: <release>      # the default, v0.1.0, predates staging
```

## Publish several files as one catalog

A `ControlCatalog` or `GuidanceCatalog` may be split across files that share
`metadata.type` and `metadata.id`. Pass them all; grcli merges them with
go-gemara and publishes one artifact named `control-catalog.yaml` or
`guidance-catalog.yaml`:

```sh
grcli publish -f part1.yaml -f part2.yaml --license Apache-2.0
```

Any other type takes exactly one file. `version` and `author.id` come from
the first file only.

## Set the version from the pipeline

```sh
grcli publish -f controls.yaml --license Apache-2.0 --version "${GITHUB_REF_NAME#v}"
```

grcli writes the value into the packed file's `metadata.version`, so tag and
body always agree. It re-encodes the YAML to do this (two-space indent);
comments and key order survive.

## Choose a license

`--license` takes an SPDX expression and is required. grcli refuses an
unknown identifier or a malformed expression before any network call, and
stores the canonical spelling as the manifest annotation
`org.opencontainers.image.licenses`. For terms with no SPDX identifier use a
`LicenseRef-<name>` token.

## Publishing from a laptop

`grcli login` followed by `grcli publish --cosign-key <key>` pushes a
key-signed bundle, but **a grc.store hub refuses it at sync**: the hub
accepts only keyless signatures today, and refuses unsigned bundles too, so
`--no-sign` fails the same way. Locally, use `--dry-run`. See
[known gaps](../explanation/known-gaps.md#local-publishing-cannot-succeed-against-grcstore).

## Pick the hub

`--url` defaults to `https://hub.grc.store`. Set `--url` (or `GRCLI_URL`) for
any other hub, such as `https://hub.preview.grc.store` for testing.
