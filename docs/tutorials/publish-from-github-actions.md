# Tutorial: publish from GitHub Actions

In this tutorial you publish the catalog from the
[offline tutorial](first-bundle-offline.md) to a hub, from a GitHub Actions
workflow, then verify and read back what you published.

Scope: **Hub protocol**, **GitHub Actions**, **Sigstore**. Publishing to a
grc.store hub works only from GitHub Actions today; see
[why](../explanation/signing-and-verification.md#why-publishing-needs-github-actions).

## Before you start

- A GitHub repository holding `controls.yaml`.
- A namespace on the hub that matches the catalog's `metadata.author.id`
  after slugifying (`example-org` in the sample). Change `author.id` to a
  namespace you own.
- That repository registered on the hub as a **trusted CI publisher** for the
  namespace. A namespace admin does this once, in the hub's web UI. It is the
  only credential setup there is: no secret, no token, no GitHub secret.

To practise without touching production, use the preview hub
(`https://hub.preview.grc.store`) if you have an account there.

## 1. Add the workflow

Save this as `.github/workflows/publish.yml`:

```yaml
name: Publish catalog
on:
  workflow_dispatch: {}

permissions:
  contents: read
  id-token: write   # the only credential grcli needs

jobs:
  publish:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          persist-credentials: false
      - uses: oras-project/setup-oras@38de303aac69abb66f3e6255b7198bff35f323e3
      - name: Install grcli
        run: |
          oras pull ghcr.io/gemaraproj/grcli:<release> --platform linux/amd64
          sudo install grcli /usr/local/bin/grcli
      - name: Publish
        run: grcli publish -f controls.yaml --license Apache-2.0 --url https://hub.grc.store
```

Replace `<release>` with a grcli release that pushes to the staging
repository. The published images `v0.1.0` and `v0.2.0` push to the served
repository directly, and a hub past the staging cutover refuses that with
`401 UNAUTHORIZED` at the first blob push. See
[known gaps](../explanation/known-gaps.md#no-release-carries-the-staging-change).

## 2. Run it

Start the workflow from the Actions tab. The publish step prints, in order:

```
pushed oci.grc.store/staging/example-org/example-controls:0.1.0
  manifest digest: sha256:…
signed (keyless)
hub indexed example-org/example-controls:0.1.0 — 1 artifacts (1 new), types=ControlCatalog
```

Here is what happened:

1. grcli asked the hub where its registry is
   (`/.well-known/grc-store-configuration`).
2. It checked that version `0.1.0` is not already published, because
   versions are immutable.
3. It asked GitHub for an OIDC token, gave it to the hub, and got back a
   registry token that may push to `staging/example-org/example-controls`.
4. It packed the bundle and pushed it to that staging repository.
5. It asked GitHub for a second OIDC token, this one for Sigstore, got a
   short-lived signing certificate from Fulcio, signed the manifest digest,
   logged the signature in Rekor, and attached it to the bundle in the
   registry.
6. It called `POST /v1/bundles/sync`. The hub verified the signature, pinned
   the signer (your workflow) to the coordinate, validated the catalog
   against its Gemara release, and copied the bundle into
   `example-org/example-controls`.

If a step failed, [fix a failed publish](../how-to/troubleshoot-publish.md)
maps each error to its cause.

## 3. Verify what you published

On your own machine:

```sh
grcli verify --repository example-org/example-controls --version 0.1.0
```

```
verifying oci.grc.store/example-org/example-controls:0.1.0 (keyless identity from hub record: keyless:https://token.actions.githubusercontent.com#https://github.com/<owner>/<repo>/.github/workflows/publish.yml, issuer https://token.actions.githubusercontent.com)
verified: keyless:https://token.actions.githubusercontent.com#https://github.com/<owner>/<repo>/.github/workflows/publish.yml
```

With no trust flags, grcli asked the hub which workflow signed this
coordinate and checked the signature against that workflow. To check without
trusting the hub's answer, name the workflow yourself:

```sh
grcli verify --repository example-org/example-controls --version 0.1.0 \
  --certificate-identity https://github.com/<owner>/<repo>/.github/workflows/publish.yml@refs/heads/main
```

## 4. Read it back

```sh
grcli versions example-org/example-controls
grcli cat --repository example-org/example-controls --version 0.1.0
grcli unpack --repository example-org/example-controls --version 0.1.0
```

`unpack` verifies the signature before it writes anything, the same check
`verify` makes. The first `cat` or `unpack` stores the bundle in grcli's
cache; the next read of the same version comes from disk.

## What you learned

- Trusted publishing: the workflow's OIDC token is the only credential.
- The bytes go to a staging repository and the hub decides whether to serve
  them.
- Anyone can verify a published version, with or without trusting the hub
  for the signer's name.
