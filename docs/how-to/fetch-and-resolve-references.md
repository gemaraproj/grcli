# How to fetch an artifact and the artifacts it references

Scope: **Hub protocol**. Reference resolution also treats any `grc.store`
host as a placeholder for the hub you target (**grc.store**).

## Print it

```sh
grcli cat --repository <ns>/<id> --version <v> | yq '.title'
```

`cat` writes only the artifact's YAML to stdout; diagnostics go to stderr. It
does not verify the signature and does not resolve references.

## Write it to a directory

```sh
grcli unpack --repository <ns>/<id> --version <v> --output ./vendor/<id>
```

This verifies the signature first, then writes the artifact file and
`bundle.json`. Nothing is written if verification fails.

## Also pull what it references

```sh
grcli unpack --repository <ns>/<id> --version <v> --with-imports
grcli unpack --repository <ns>/<id> --version <v> --with-references
```

- `--with-imports` pulls the mapping references that the artifact's
  `imports` point at.
- `--with-references` pulls every entry in `metadata.mapping-references`,
  whatever points at it.

Each pulled artifact lands in
`references/<category>/<ns>/<id>@<version>/` (category is `imports`,
`extends`, `lexicon` or `reference`), with a summary in
`references/index.json`.

A reference is pulled only when its `url` names a hub artifact: the host is
`grc.store` (or a subdomain), or exactly the host of `--url`, and the path is
`/<ns>/<id>`. The version comes from the reference's `version` field. Other
URLs are external standards; grcli lists them as skipped.

Write references that grcli and the hub can both resolve:

```yaml
metadata:
  mapping-references:
    - id: base
      title: Base catalog
      version: "2.1.0"
      url: https://grc.store/acme/baseline
imports:
  - reference-id: base
```

`grcli validate` and `grcli publish` warn about grc.store URLs that are not
in this form.

**Pulled references are not signature-verified.** Only the primary artifact
is. A reference whose license differs from the primary's gets a warning.
Resolution is one level deep: a referenced artifact's own references are not
followed.

## Work offline

The first pull of a version is cached. Later `cat`/`unpack` of the same
version (or a reference to it) reads from the cache with no network for the
content. `unpack` still goes to the network to verify; add `--no-verify` for
a fully offline read of content you already trust.

- `--no-cache` skips the cache for one run.
- `cache-enabled: false` in the config file turns it off.
- `GRCLI_CACHE=<dir>` moves it.

## Read a local bundle

Both commands take `--source <oci-layout-dir>` instead of `--repository`, for
the output of `publish --dry-run`:

```sh
grcli cat --source grcli-out --version 0.1.0
```
