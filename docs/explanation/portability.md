# What is general and what is grc.store-specific

grcli was built as the command-line client of one service, grc.store, and
every use of it so far has been against a grc.store hub. This page sorts what
grcli does into what would work anywhere, what needs a server that speaks the
grc.store hub protocol, and what is tied to grc.store itself, GitHub or
Sigstore. It is written from the code on `main`.

## What the hub is

"The hub" is the server behind `--url`. The grc.store deployment runs
**Semaphore**, Revanite's hub service, at `hub.grc.store`, with a
[zot](https://zotregistry.dev) OCI registry beside it at `oci.grc.store` and
Keycloak as its login server. Its source is not public. grcli talks to it
through:

- the open wire types in `grc-store-protocol`, and
- the five endpoints listed in
  [the protocol reference](../reference/formats-and-protocol.md#hub-endpoints-grcli-calls).

Any server that serves those endpoints with those types would work with
grcli. None other exists today.

## Feature by feature

| Feature | Scope | Notes |
|---|---|---|
| `validate` | **General** | Needs only `cue` and the Gemara module on the CUE registry (or a local checkout). |
| Bundle format (`publish --dry-run`, `unpack`/`cat --source`) | **General** | go-gemara's OCI bundle; any OCI layout or registry can hold it. |
| Reference lint warnings in `validate`/`publish` | **grc.store** | Warn about URLs on `grc.store` hosts and recommend `https://grc.store/<ns>/<id>`. Irrelevant to other hosts. |
| Coordinate derivation (`slugify(author.id)/slugify(id)`) | **Hub protocol** | A grc.store convention for namespaces; meaningless to a plain registry. |
| Licence gate | **General** | SPDX validation and the standard OCI licence annotation. |
| Provenance record | **General**, with a **grc.store** `buildType` URI (`https://grc.store/grcli/buildtype/v0`) and **GitHub Actions**-specific builder ids. |
| Registry discovery | **Hub protocol** | Every remote command needs the discovery document. There is no `--registry` flag. |
| Registry tokens | **Hub protocol** | grcli asks the hub's `/v2/token`. Manual `GRCLI_REGISTRY_*` or Docker credentials work but discovery is still required. |
| Staging push + sync | **Hub protocol** | `staging/<ns>/<id>`, then `POST /v1/bundles/sync`. |
| Version pre-check | **Hub protocol** | `GET /v1/catalogs/…/versions/…`. |
| `versions` | **Hub protocol** | `GET /v1/catalogs/<ns>/<id>`. |
| `login`/`logout` | **Hub protocol** + any OIDC provider with device flow | The hub names the issuer and client; Keycloak in practice. |
| CI authentication to the hub | **GitHub Actions** + **Hub protocol** | grcli only knows GitHub's OIDC token endpoint; the hub only validates GitHub's issuer. |
| Keyless signing | **GitHub Actions** + **Sigstore** | Fulcio/Rekor URLs are configurable; the OIDC token source is not. |
| Key signing (`--cosign-key`) | **Sigstore** (cosign) | Works with any registry, but a grc.store hub refuses key-signed catalogs. |
| Keyless verify, explicit identity | **Sigstore** + **Hub protocol** (discovery only) | Any issuer can be named; the root can be replaced. |
| Keyless verify, zero-flag | **Hub protocol** | Needs `signer_identity` from the hub. |
| `unpack`/`cat` from a registry | **Hub protocol** | Discovery, anonymous pull token. |
| Reference resolution | **Hub protocol** + **grc.store** | `grc.store` (any subdomain) is hard-wired as a placeholder for "the hub at `--url`". A self-hoster's own host also resolves, but only when it equals `--url`'s host exactly. |
| Cache | **General** | Keyed by the hub host, so hubs do not mix. |
| Default `--url` | **grc.store** | `https://hub.grc.store`. Override with `GRCLI_URL` or the `url:` config key. |
| Default verify issuer | **GitHub Actions** | `https://token.actions.githubusercontent.com`. |
| Trust root | **Sigstore** public-good by default | `GRCLI_TRUSTED_ROOT` replaces it. |
| Install channel | **General** | Signed OCI image on GHCR. |
| Command help and messages | **grc.store** | Short descriptions say "grc.store"; error hints mention `grcli login` and grc.store terms. |

## Hard-wired values

| Value | Where | Effect |
|---|---|---|
| `https://hub.grc.store` | `cmd/urldefault.go` | Default hub for every command. |
| `grc.store` and `*.grc.store` hosts | `grc-store-protocol/refurl.IsGrcStoreHost`, used by `internal/refs` | Reference placeholder host; lint target. |
| `https://grc.store/<ns>/<id>` | `internal/refs.Canonical` | The URL form lint recommends. |
| `https://grc.store/grcli/buildtype/v0` | `internal/provenance` | Provenance `buildType`. |
| `github.com/gemaraproj/gemara` | `cmd/validate.go` | Schema module (correct for any Gemara user). |
| `https://token.actions.githubusercontent.com` | `cmd/verify.go` | Default verify issuer. |
| `GITHUB_ACTIONS`, `ACTIONS_ID_TOKEN_REQUEST_*` | `internal/sign`, clientkit `auth` | The only keyless/CI path. |
| `https://fulcio.sigstore.dev`, `https://rekor.sigstore.dev` | `internal/sign/keyless.go` | Default signing services (overridable). |
| Embedded `trusted_root.json` | `grc-store-clientkit/trustroot` | Default verify root (overridable). |
| `staging` namespace | `grc-store-protocol/staging` | Reserved name; the push target. |

## Scenarios

### 1. A Gemara toolkit with no server

Works today: write, `validate`, `publish --dry-run`, and read the layout back
with `unpack`/`cat --source`. What is missing is signing a local layout
(signatures need a registry) and pushing or pulling to a registry without a
hub.

### 2. Any OCI registry, no hub (GHCR, Harbor, a plain zot)

Does not work. Every remote command starts with hub discovery, and `publish`
always pushes to `staging/…` and then calls `/v1/bundles/sync`. To support
this, grcli would need a registry-only mode: a `--registry` target, a push to
the named repository with no staging or sync, and verify/unpack that take an
expected identity instead of looking one up. The pieces exist in
`internal/registry` and `internal/sigverify`; the command layer assumes a hub
throughout.

### 3. A self-hosted Semaphore hub

Works, with `--url` (or `GRCLI_URL`). Watch for:

- references written as `https://grc.store/…` resolve against your hub,
  which is what you want only if you mirror those artifacts;
- references to your own host resolve only if it is exactly `--url`'s host;
- lint warnings still recommend the `grc.store` URL form;
- **reads are anonymous**, so a hub with anonymous reads off, or a private
  namespace, cannot be read by grcli (see
  [known gaps](known-gaps.md#reads-are-always-anonymous)).

### 4. A different hub implementation

Possible in principle: implement the discovery document, `/v2/token` with a
Distribution-token registry behind it, `/v1/catalogs/<ns>/<id>` (with
`releases[]` and `signer_identity`), `/v1/catalogs/<ns>/<id>/versions/<v>`
(status codes only), and `POST /v1/bundles/sync`, using the
`grc-store-protocol` types and error envelope, and honour the
`staging/<ns>/<id>` convention.

### 5. CI other than GitHub Actions

Publishing does not work: grcli can get a keyless signing token and a hub CI
token only from GitHub Actions, and the hub validates only GitHub's issuer.
A static `--token` plus `--cosign-key` would get past grcli but a grc.store
hub would refuse the key signature. Verifying artifacts signed elsewhere
works with `--certificate-identity` and `--certificate-oidc-issuer`.

### 6. Air-gapped

Verifying: set `GRCLI_TRUSTED_ROOT` and use a cache or a local mirror hub
(plain-HTTP registries are supported through discovery). Signing: set
`GRCLI_FULCIO_URL` and `GRCLI_REKOR_URL`, but the signing token still has to
come from GitHub Actions. Validating: `--spec` or `CUE_REGISTRY`.

## Where the seams are

If grcli is to be used outside grc.store, these are the places a change
would go, smallest first:

1. **Defaults and wording**: the default URL, help text, lint messages.
2. **Read authentication**: send the login token on pulls and catalog
   reads.
3. **Reference placeholder**: make the "this host means the target hub" rule
   configurable instead of fixed to `grc.store`.
4. **CI identity**: accept OIDC tokens from other CI systems for signing
   (sigstore-go supports them; grcli's token fetch does not).
5. **Registry-only mode**: publish/verify/unpack against a registry with no
   hub.
