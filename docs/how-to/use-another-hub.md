# How to point grcli at another hub, registry or Sigstore

Scope: this page is about what is *configurable*. What cannot be changed is
listed in [portability](../explanation/portability.md).

## Another hub

Every command that talks to a hub takes `--url`. Set it once instead:

```sh
export GRCLI_URL=https://hub.example.com
```

or in `~/.config/grcli/config.yaml`:

```yaml
url: https://hub.example.com
```

The hub must serve `/.well-known/grc-store-configuration` with a
`registry_url`. grcli finds the registry, the OIDC issuer for `login` and the
CI audience from that document. A `registry_url` with `http://` makes grcli
talk plain HTTP to the registry, for a local or air-gapped mirror.

Log in once per hub (per OIDC issuer, strictly: two hubs that share an issuer
share one login):

```sh
grcli login --url https://hub.example.com
```

## Supply registry credentials yourself

grcli normally asks the hub for a short-lived registry token. To use your own
credential instead, set it in the environment (never in a config file):

```sh
export GRCLI_REGISTRY_TOKEN=<bearer>
# or
export GRCLI_REGISTRY_USERNAME=<user> GRCLI_REGISTRY_PASSWORD=<password>
```

With neither set and the hub not asked (an empty `--url`), grcli falls back
to your Docker credential store (`~/.docker/config.json` and its helpers).

## A private Sigstore

| What | Set |
|---|---|
| Fulcio for keyless signing | `GRCLI_FULCIO_URL` |
| Rekor for keyless signing | `GRCLI_REKOR_URL` |
| Trust root for verifying | `GRCLI_TRUSTED_ROOT=/path/trusted_root.json` (or `trusted-root:` in config) |

The verifier still requires a certificate transparency timestamp, a Rekor
entry and an observed timestamp, whichever root it uses.

## Another CI provider's identity, for verifying

```sh
grcli verify … --certificate-identity <subject> --certificate-oidc-issuer https://gitlab.example.com
```

Signing is a different matter: keyless signing in grcli works only inside
GitHub Actions. See [portability](../explanation/portability.md).

## Another schema registry, for validate

`grcli validate` runs `cue`, which honours `CUE_REGISTRY` for a mirror of the
CUE central registry. For an unreleased or offline schema, point at a local
checkout of the Gemara CUE module:

```sh
grcli validate -f file.yaml --spec ../gemara
```
