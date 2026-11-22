# Design decisions

Why grcli works the way it does. Each section gives the decision and grcli's
reason for it.

## One `--url`, and a discovery document

**Decision.** grcli takes one hub URL and learns everything else from
`GET /.well-known/grc-store-configuration`: the registry, the OIDC issuer and
client for `login`, and the audience for CI tokens.

**Why.** Earlier versions took `--registry` and `--hub-url` on every call,
which made users learn that one product is internally two services. A
discovery document lets them type one URL, and lets an operator move the
registry, or run a private mirror on plain HTTP, without every user
reconfiguring. It follows the `openid-configuration` naming convention.

## The hub mints every registry token

**Decision.** The registry trusts bearer tokens signed by the hub.
grcli asks the hub's `/v2/token` for a token scoped to one repository before
each registry operation, instead of holding registry credentials.

**Why.** Authorization lives in one place: the hub knows who owns which
namespace. Pull is granted widely, push only to a namespace's owners and
trusted publishers, delete to nobody, so a published version cannot be
removed through the registry.

## Trusted publishing from CI, with no secret

**Decision.** In GitHub Actions grcli presents the workflow's own OIDC token
to the hub. A namespace admin binds `owner/repo` (optionally one ref) to the
namespace. There is no API key.

**Why.** A stored secret leaks and has to be rotated; a workflow token is
minted per run and names exactly what is running. An earlier design ran the
exchange through the identity server and still needed a client secret in CI;
it was dropped for direct validation in the hub.

## The coordinate comes from the artifact

**Decision.** `<namespace>/<id>` is `slugify(metadata.author.id)` /
`slugify(metadata.id)`, and the tag is always `metadata.version`. `publish`
has no `--tag`; `--version` rewrites the body so the two stay equal.

**Why.** The hub indexes the body, so a tag or repository that disagrees with
the body splits what the registry holds from what the hub shows. The hub
refuses a tag that differs from `metadata.version`
(`422 tag_version_mismatch`) and a repository that differs from the author
(`422 catalog_coordinate_mismatch`); grcli derives both so they cannot
differ. The hub authorizes the namespace it is about to write, derived from
the body, not the one the request names.

## Versions are immutable

**Decision.** `publish` checks `GET /v1/catalogs/<ns>/<id>/versions/<v>`
before pushing and stops if the version exists or was yanked.

**Why.** Consumers cache by version forever. The registry would accept an
overwrite before the hub could refuse it, so grcli checks first. A yanked
version stays taken.

## A licence is required, and grcli is the strict end

**Decision.** `--license` is mandatory and must be a valid SPDX expression of
known identifiers. It travels as the standard OCI annotation
`org.opencontainers.image.licenses`.

**Why.** A compliance catalog with unclear reuse terms is a liability for
its consumers. grcli rejects typos; the hub rejects only a missing or
malformed value and stores well-formed unknown identifiers, so a hub on an
older SPDX list never blocks a newer publisher.

## Publish through a staging repository

**Decision.** grcli pushes to `staging/<ns>/<id>` and asks the hub to sync
`<ns>/<id>`. The hub checks the staged bundle (namespace, version,
signature, signer, licence, schema) and copies an accepted one into the
served repository. Nobody pushes a served repository. There is no fallback.

**Why.** Before staging, a publish wrote straight to the served repository,
so bytes the hub then refused were still pullable. Staging makes "refused"
mean "never served". Staging is a separate registry store that deletes
anything older than about 24 hours, so an abandoned publish cleans itself
up.

## The hub validates the schema; grcli's `validate` is a preview

**Decision.** `grcli validate` checks a file against the Gemara release it
declares, fetched from the CUE central registry. `publish` does not validate;
the hub does, at sync, against the same declared release (the newest patch of
that minor line), and refuses lines older than 1.5.

**Why.** Holding a body to the release its author declared is predictable for
publishers and lets them adopt a new Gemara release before the hub moves.
Running the same check in grcli lets authors see failures before CI.

## The hub verifies signatures at ingest and pins the signer

**Decision.** The hub refuses unsigned or invalidly signed catalogs, records
the canonical signer identity, pins it per coordinate on first publish, and
serves it to consumers.

**Why.** Client-side verification alone was unusable: a consumer had to
already know which workflow published an artifact. Recording the signer at
ingest gives every consumer a name to check against, and the pin stops a
different workflow from publishing later versions. Catalogs and plugins now
share one trust model.

## Zero-flag verify, and an independent mode

**Decision.** `grcli verify` with no trust flags checks against the hub's
recorded signer. `--certificate-identity` bypasses the hub.

**Why.** The hub is trusted to say *which* identity, never for the
cryptography, and the identity it uses is printed before verification so
the trust is visible. Users who do not want even that pass the identity
themselves.

## Sign and verify in-process

**Decision.** Keyless signing and all keyless verification use sigstore-go
inside grcli, with the Sigstore root pinned in the binary. cosign is needed
only for `--cosign-key`.

**Why.** Shelling out to cosign tied grcli to cosign's flag changes across
versions, and made every user install a second tool. A pinned root keeps
verification offline-capable and deterministic. grcli's verifier mirrors the
hub's so the two cannot disagree by accident; it differs on purpose in
pinning the identity, because grcli, unlike the hub, knows whom to expect.

## `unpack` verifies by default

**Decision.** `unpack` verifies the signature before writing anything and
fails closed. `--no-verify` opts out loudly.

**Why.** Verification that does not gate the write is theatre. `verify`
stays for the one job `unpack` cannot do: check a coordinate without
downloading it.

## Resolve references at the consumer, never bundle them

**Decision.** A bundle holds one artifact. `unpack --with-imports` /
`--with-references` fetches referenced artifacts as themselves. A
`grc.store` host in a reference URL means "the hub I am talking to".
References to other hosts are never fetched.

**Why.** Bundling dependencies would mix licences and put one publisher's
signature over another's content. Treating `grc.store` as a placeholder lets
one authored reference work against production, preview or a mirror. Never
calling a host named in untrusted metadata keeps `unpack` from becoming a
request-forgery tool.

## References are not signature-verified yet

**Decision.** Resolved references are pulled and cached, but not verified;
they are marked `verified: false`.

**Why.** Deferred to ship resolution first. Verify-on-pull is a separate
decision still to be made.

## `cat`, and a cache of whole bundles

**Decision.** `cat` streams the artifact to stdout. `cat` and `unpack` share
a disk cache of decoded bundles keyed by hub host, namespace, id and version.

**Why.** Published versions never change, so a cached copy is never stale
and repeated reads need no network. The cache holds the decoded bundle, not
the raw OCI layout, so it serves both commands; the cost is that the
signature is not cached, so `unpack` still goes online to verify.

## One config file, and no per-project config

**Decision.** grcli reads only `~/.config/grcli/config.yaml` (or a file passed
with `--config`). A `./.grcli.yaml` in the working directory is ignored with
a warning.

**Why.** A config file committed to a repository could silently point a
publish or verify at another hub or registry for anyone who cloned it. A
file passed explicitly with `--config` is a deliberate act, so a checked-in
publish manifest is still possible. `cache-enabled` is a flat key because
`GRCLI_CACHE` would otherwise hide a nested `cache.enabled`.

## One shared module for the wire contract

**Decision.** Types that cross the wire live in `grc-store-protocol`, an open
module the hub, grcli and privateer all import. It contains nouns and rules,
never client code.

**Why.** Three implementations of the same JSON drift. Third-party producers
need the contract open. Client logic (auth, signing, pushing) is left to each
tool so the module stays small; shared client code went to
`grc-store-clientkit` instead.

## Authentication lives in clientkit

**Decision.** Device-flow login, the credential store, token refresh, the
GitHub Actions token fetch and the pinned Sigstore root come from
`grc-store-clientkit`, shared with privateer-sdk.

**Why.** grcli and privateer had near-identical copies, and the trust root was
vendored three times; rotating it meant three edits.
