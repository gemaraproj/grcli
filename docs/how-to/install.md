# How to install grcli and check its signature

Scope: **General** (the binary is a public OCI artifact on GHCR).

grcli ships as a signed, multi-platform OCI artifact at
`ghcr.io/gemaraproj/grcli`, not as GitHub release assets. You need
[`oras`](https://oras.land) 1.3 or later to pull it. Pulling needs no token.

## On your machine

The release workflow signs each image keylessly. Resolve the tag to a digest
once, verify that digest with cosign, then pull and install the same digest,
so the bytes you install are the bytes you verified:

```sh
ref="ghcr.io/gemaraproj/grcli@$(oras resolve ghcr.io/gemaraproj/grcli:<tag>)"
cosign verify "$ref" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github.com/gemaraproj/grcli/.github/workflows/release.yml@'
# platforms: linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64
oras pull "$ref" --platform darwin/arm64
chmod +x grcli && sudo mv grcli /usr/local/bin/
grcli --version
```

Use a release tag for `<tag>`. `latest` also exists, but pinning is what
makes an install reproducible. Skipping `cosign verify` installs a binary
nobody has checked.

## In GitHub Actions

Either pull it yourself:

```yaml
- uses: oras-project/setup-oras@38de303aac69abb66f3e6255b7198bff35f323e3
- run: |
    oras pull ghcr.io/gemaraproj/grcli:<tag> --platform linux/amd64
    sudo install grcli /usr/local/bin/grcli
```

or use the composite action in this repository, which picks the platform from
the runner and can verify first (needs cosign on the runner for
`verify: "true"`):

```yaml
- uses: gemaraproj/grcli/.github/actions/install@<ref>
  with:
    version: <tag>
    verify: "true"
```

## What else you may need

| For | Install |
|---|---|
| `grcli validate` | `cue` (<https://cuelang.org>) |
| `publish --cosign-key`, `verify --cosign-key` | `cosign` 2.6.0 or later |
| Everything else | nothing: keyless signing and verifying run inside grcli |

## Build from source

```sh
git clone https://github.com/gemaraproj/grcli && cd grcli
make build        # writes bin/grcli, versioned by `git describe`
```
