# How to verify an artifact before you trust it

Scope: **Hub protocol** (to find the registry and, in zero-flag mode, the
signer), **Sigstore**.

`grcli verify` checks a published version's signature without downloading
its content. `grcli unpack` makes the same check before it writes anything,
so you need `verify` only when you want the answer without the files: a CI
gate, an admission check, or a spot check.

## Trust the hub to name the signer (zero-flag)

```sh
grcli verify --repository <namespace>/<id> --version <version>
```

grcli reads the signer the hub recorded for this coordinate when the first
signed version was published, prints it, and checks the signature against
it. The check accepts that workflow file on any branch or tag. The
cryptography is still checked against the Sigstore trust root built into
grcli; the hub only tells grcli *which* identity to expect.

## Name the signer yourself (independent)

```sh
grcli verify --repository <namespace>/<id> --version <version> \
  --certificate-identity https://github.com/<owner>/<repo>/.github/workflows/<file>.yml@refs/heads/main
```

This skips the hub's record. The identity must match the certificate's
subject exactly, including the `@<ref>` suffix. The issuer defaults to
GitHub Actions (`https://token.actions.githubusercontent.com`); pass
`--certificate-oidc-issuer` for GitHub Enterprise or another CI provider.

## Verify a key-signed artifact

```sh
grcli verify --repository <namespace>/<id> --version <version> --cosign-key cosign.pub
```

This is the only mode that runs `cosign` (2.6.0 or later on `PATH`).

## Gate unpack on a specific signer

`unpack` accepts the same `--certificate-identity` and
`--certificate-oidc-issuer` flags:

```sh
grcli unpack --repository <ns>/<id> --version <v> \
  --certificate-identity https://github.com/<owner>/<repo>/.github/workflows/publish.yml@refs/heads/main
```

`--no-verify` turns the check off and prints a warning. Use it only for
content you know to be unsigned.

## When it fails

| Message | Meaning |
|---|---|
| `has no signature attached in the registry` | Published unsigned, or signed by a grcli older than the Sigstore bundle format. Re-publish. |
| `hub has no recorded signer identity` | The hub never verified a signature for this coordinate. Pass `--certificate-identity` or `--cosign-key`. |
| `signature verification failed: … certificate identity …` | Signed, but not by the identity you (or the hub) named. |
| `--certificate-oidc-issuer requires --certificate-identity` | An issuer on its own means nothing; add the identity. |

## Use a private Sigstore

Point `GRCLI_TRUSTED_ROOT` (or the `trusted-root` config key) at a
`trusted_root.json`. See [use another hub](use-another-hub.md).
