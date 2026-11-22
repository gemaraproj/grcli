# Signing and verification

grc.store publishes compliance documents, where provenance is the product: a
consumer needs to know not only that an artifact is intact but who published
it. This page explains what grcli signs, how, and what each kind of
verification does and does not prove.

## What is signed

grcli signs the **OCI manifest digest** of the bundle. The manifest names the
config blob (the bundle manifest with the provenance record) and the layer
(the artifact YAML) by digest, so one signature covers the YAML, the
provenance record and the licence annotation together.

The signature is a Sigstore bundle (v0.3) attached to the manifest as an OCI
1.1 *referrer*: a separate small manifest in the same repository whose
`subject` is the signed manifest. The signature is therefore not inside the
bundle. That is why `verify` cannot check a local layout from
`publish --dry-run`, and why `cat` and a cached `unpack` do not carry it.

## Keyless signing (the publish path)

Scope: **GitHub Actions**, **Sigstore**.

Inside a GitHub Actions job with `id-token: write`, grcli:

1. asks GitHub's OIDC endpoint for a token with audience `sigstore`;
2. makes an ephemeral key pair;
3. gets a short-lived certificate from Fulcio for that token. The
   certificate's subject (SAN) is the workflow file that ran, for example
   `https://github.com/acme/cat/.github/workflows/publish.yml@refs/heads/main`,
   and its issuer is `https://token.actions.githubusercontent.com`;
4. signs a DSSE-wrapped in-toto statement naming the manifest digest;
5. records the signature in the Rekor transparency log;
6. pushes the resulting bundle as a referrer.

All of this runs inside grcli via sigstore-go. No key exists before or after
the job, and no secret is stored anywhere. The signer *is* the workflow.

This is a different OIDC token from the one grcli shows the hub. The hub
token has the hub's `ci_audience`; Fulcio accepts only public OIDC issuers,
never the hub's own login server, so a `grcli login` token cannot sign.

### Why publishing needs GitHub Actions

Keyless signing needs an OIDC token Fulcio trusts, and grcli knows how to get
one in exactly one place: the GitHub Actions runtime. There is no browser
OIDC flow for Sigstore in grcli and no support for other CI providers'
tokens. The alternative, `--cosign-key`, produces a valid cosign signature,
but a grc.store hub refuses it at sync (`catalog_verification_failed`):
the hub's verifier accepts only certificate-based (keyless) signatures,
because it pins a signer *identity*, and a key-based scheme
(`key:sha256:<fingerprint>`) is defined but not wired up. An unsigned
publish (`--no-sign`) is refused too (`catalog_unsigned`). So against
grc.store, GitHub Actions is the only way to publish.

## What the hub does with the signature

At `POST /v1/bundles/sync` the hub:

1. verifies the signature against its own pinned Sigstore root (same Rekor,
   SCT and timestamp requirements as grcli), **without** checking who signed;
2. turns the certificate into a canonical identity,
   `keyless:<issuer>#<SAN with the @ref removed>`;
3. on the first signed publish of a coordinate, **pins** that identity
   (trust on first use). Later publishes signed by anyone else are refused
   with `catalog_signer_mismatch`. A namespace admin can reset the pin, which
   is kept as an audit record;
4. serves the pinned identity as `signer_identity` on
   `GET /v1/catalogs/<ns>/<id>`.

## Verifying

Scope: **Sigstore**, plus **Hub protocol** for the registry location and, in
zero-flag mode, the signer.

Every keyless verify in grcli checks three things:

- **Cryptography.** The certificate chains to Fulcio in the trust root, the
  signature is in Rekor, and there is a signed certificate timestamp and an
  observed timestamp. The trust root is the Sigstore public-good
  `trusted_root.json` built into grcli (from
  `grc-store-clientkit/trustroot`), or `GRCLI_TRUSTED_ROOT`.
- **Binding.** The signed statement names the digest the tag resolves to in
  the registry now.
- **Identity.** The certificate's issuer and SAN match what grcli expects.

The modes differ only in where the expected identity comes from:

| Mode | Expected identity | Trusts the hub for |
|---|---|---|
| Zero-flag | the hub's `signer_identity`; the SAN must start with that workflow path followed by `@` (any ref) | *which* identity to expect |
| `--certificate-identity` | exactly what you typed, `@ref` included | nothing (the registry location still comes from discovery) |
| `--cosign-key` | the key | nothing; runs cosign |

A compromised hub cannot forge a Fulcio certificate for a workflow it does
not control, so zero-flag mode does not trust the hub with the cryptography.
It does trust the hub to name the right workflow; anyone who will not
extend that trust passes `--certificate-identity`.

## What verification does not cover

- **Referenced artifacts.** `unpack --with-*` pulls them unverified;
  `references/index.json` records `"verified": false`.
- **`cat`.** Never verifies.
- **The caller repository of a reusable workflow.** When a publisher uses a
  shared reusable workflow (such as this repository's `publish-gemara.yml`),
  the certificate's SAN names the reusable workflow, not the repository that
  called it. The hub records the caller separately, but grcli's identity
  check uses only the SAN. See [known gaps](known-gaps.md).
- **Provenance truth.** The provenance record is signed as part of the
  bundle, but its content is what grcli on the publisher's machine wrote. It
  is not a SLSA attestation from a trusted builder.
