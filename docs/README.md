# grcli documentation

These docs describe grcli as the code on `main` behaves, not as it was meant to
behave. Where the two differ, the docs say so. They follow the
[Diátaxis](https://diataxis.fr) layout: four kinds of page, each for a
different need.

| You want to… | Read |
|---|---|
| Learn grcli by doing, start to finish | [Tutorials](#tutorials) |
| Get one specific job done | [How-to guides](#how-to-guides) |
| Look up a command, flag, setting or file format | [Reference](#reference) |
| Understand how grcli works and why it is built this way | [Explanation](#explanation) |

## Tutorials

- [Your first bundle, offline](tutorials/first-bundle-offline.md): write a
  Gemara catalog, validate it, pack it, and read it back, with no account and
  no network beyond the schema download.
- [Publish from GitHub Actions](tutorials/publish-from-github-actions.md):
  take the same catalog to a hub through trusted publishing, then verify and
  unpack what you published.

## How-to guides

- [Install grcli and check its signature](how-to/install.md)
- [Publish an artifact](how-to/publish.md)
- [Verify an artifact before you trust it](how-to/verify.md)
- [Fetch an artifact and the artifacts it references](how-to/fetch-and-resolve-references.md)
- [Point grcli at another hub, registry or Sigstore](how-to/use-another-hub.md)
- [Fix a failed publish](how-to/troubleshoot-publish.md)

## Reference

- [Commands](reference/commands.md): every command and flag, what it prints,
  and when it fails.
- [Configuration and environment](reference/configuration.md): the config
  file, every `GRCLI_*` variable, and the files grcli reads and writes.
- [Formats and protocol](reference/formats-and-protocol.md): the hub
  endpoints grcli calls, the bundle layout, the provenance record, the
  signature, the cache, and `references/index.json`.

## Explanation

- [Architecture](explanation/architecture.md): the packages, the data flow of
  each command, and what grcli depends on.
- [Signing and verification](explanation/signing-and-verification.md): what
  is signed, by whom, and what each verify mode trusts.
- [Design decisions](explanation/design-decisions.md): why grcli works the
  way it does, decision by decision.
- [What is general and what is grc.store-specific](explanation/portability.md):
  which parts of grcli would work against something other than a grc.store
  hub, and which would not.
- [Known gaps and surprises](explanation/known-gaps.md): behaviour that is
  undocumented elsewhere, dead, or likely to surprise a maintainer.

## Scope labels

Pages mark how far each behaviour reaches with these labels:

| Label | Means |
|---|---|
| **General** | Works with any Gemara file or any OCI registry. No hub needed. |
| **Hub protocol** | Needs a server that speaks the grc.store hub protocol (discovery document, `/v2/token`, `/v1/bundles/sync`, `/v1/catalogs`). The [grc.store hub](explanation/portability.md#what-the-hub-is) is the only implementation today. |
| **grc.store** | Hard-wired to the public grc.store deployment: a default URL, a host name, a URI. |
| **GitHub Actions** | Assumes GitHub Actions is the CI system and its OIDC token is the identity. |
| **Sigstore** | Assumes Sigstore (Fulcio and Rekor), by default the public-good instance. |

## Terms

- **Gemara**: the YAML format grcli publishes. Each file is one *artifact*
  (a control catalog, a threat catalog, a policy, an evaluation log, and so
  on) with a `metadata` block that names its `type`, `id`, `version`,
  `author` and `gemara-version`.
- **Bundle**: one artifact packed as an OCI image manifest, with a
  `bundle.json` manifest that carries the provenance record. Built by the
  `bundle` package of [go-gemara](https://github.com/gemaraproj/go-gemara).
- **Hub**: the server that indexes published bundles, mints registry tokens
  and decides who may publish where. grc.store runs one at `hub.grc.store`.
- **Coordinate**: `<namespace>/<id>@<version>`. It names one published
  version. The namespace is the publisher's account on the hub.
- **Staging repository**: `staging/<namespace>/<id>`, where grcli pushes a
  bundle before the hub checks it and copies it into the served repository
  `<namespace>/<id>`.
