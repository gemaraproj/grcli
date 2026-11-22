# Tutorial: your first bundle, offline

In this tutorial you write a small Gemara control catalog, check it against
the Gemara schema, pack it into a bundle on your own disk, and read it back.
You need no account and no hub. The only network access is `cue`
downloading the Gemara schema the first time.

Scope: **General**. Nothing here talks to a hub.

## Before you start

- grcli on your `PATH` ([install it](../how-to/install.md)).
- `cue` on your `PATH`, for `grcli validate` (<https://cuelang.org>).

Make an empty working directory and change into it.

## 1. Write a catalog

Save this as `controls.yaml`:

```yaml
title: Example Controls
metadata:
  id: example-controls
  type: ControlCatalog
  gemara-version: "1.6.0"
  version: "0.1.0"
  description: A tiny catalog used to try grcli.
  author:
    id: example-org
    name: Example Org
    type: Human
  applicability-groups:
    - id: all
      title: All repositories
      description: Applies to every repository.
groups:
  - id: EX
    title: Example group
    description: Controls for the tutorial.
controls:
  - id: EX-01
    title: Protect the default branch
    objective: Changes reach the default branch only through review.
    group: EX
    assessment-requirements:
      - id: EX-01.01
        text: The default branch requires a pull request before merging.
        applicability: ["all"]
```

Four `metadata` fields matter to grcli:

- `type` picks the schema to check against (`#ControlCatalog`).
- `gemara-version` picks the schema release.
- `version` becomes the published version and the OCI tag.
- `author.id` and `id` together decide where the catalog would be published:
  `example-org/example-controls`.

## 2. Validate it

```sh
grcli validate -f controls.yaml
```

You should see:

```
OK   controls.yaml (#ControlCatalog, gemara v1.6.0)
```

grcli ran `cue vet` against the Gemara module `github.com/gemaraproj/gemara`
at `v1.6.0`, fetched from the CUE central registry into cue's own cache.

Try breaking it. Change `group: EX` to `group: XX` and validate again. cue
reports that `XX` is not one of the catalog's `groups`, grcli prints a `FAIL`
line, and the command exits non-zero. Change it back.

## 3. Pack it into a bundle on disk

```sh
grcli publish -f controls.yaml --license Apache-2.0 --dry-run
```

```
dry-run: wrote bundle to oci:grcli-out:0.1.0
  manifest digest: sha256:7ca5…
  body digest:     sha256:74e7…
  artifact: ControlCatalog/example-controls
```

`--dry-run` builds exactly the bundle a real publish would push, but writes
it to an OCI image layout in `./grcli-out` instead of a registry. It skips
every network call, so it needs no login and does not sign.

`--license` is required even here. Leave it out and grcli refuses before
doing anything:

```
grcli: a publication license is required: pass --license with an SPDX expression …
```

Look inside `grcli-out`: it is a standard OCI layout (`oci-layout`,
`index.json`, `blobs/`). Any OCI tool can read it.

## 4. Read the bundle back

Write its contents to a directory:

```sh
grcli unpack --source grcli-out --version 0.1.0
```

```
  ! --source is a local OCI layout with no registry signature to verify; writing WITHOUT verification
unpacked grcli-out:0.1.0 → grcli-unpacked (1 files, 0 imports)
  - controls.yaml
  - bundle.json (bundle manifest)
```

`grcli-unpacked/controls.yaml` is your file, byte for byte.
`grcli-unpacked/bundle.json` is the bundle manifest. Open it: under
`metadata.provenance` is the provenance record grcli wrote, naming the input
file and its digest, the target repository, and the machine that built it.

The warning is expected. A signature lives in a registry, next to the bundle,
not inside it, so a local layout has nothing to verify.

Or print just the Gemara content to your terminal, without writing files:

```sh
grcli cat --source grcli-out --version 0.1.0
```

## 5. Override the version

A release pipeline often wants to set the version from outside the file:

```sh
grcli publish -f controls.yaml --license Apache-2.0 --version 0.2.0 --dry-run --output out2
```

```
note: --version 0.2.0 overrides metadata.version 0.1.0
dry-run: wrote bundle to oci:out2:0.2.0
```

grcli wrote `0.2.0` into the packed file's `metadata.version` too. The hub
refuses a bundle whose tag and `metadata.version` differ, so the two are
always changed together.

## What you learned

- `validate` checks a file against the Gemara release it declares.
- `publish --dry-run` builds the real bundle with no network.
- `unpack` and `cat` read a bundle from a local layout (or, with `--url`,
  from a hub).

Next: [publish the catalog from GitHub Actions](publish-from-github-actions.md).
