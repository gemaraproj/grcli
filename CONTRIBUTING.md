# Contributing to grcli

Thanks for helping out. This is a small Go CLI; the dev loop is the `Makefile`.

## Developer Certificate of Origin

Every commit must be signed off under the [DCO](https://developercertificate.org/).
A bot checks this on every pull request, so use `-s`:

```sh
git commit -s -m "fix: ..."
```

If you forget, `git commit --amend -s` (or `git rebase --signoff main`) adds the
trailer. The sign-off name and email must match the commit author.

## Dev loop

```sh
make build      # bin/grcli
make test       # go test ./...
make ci-local   # fmtcheck + vet + lint + tidycheck + testcov — what CI runs
```

`make lint` needs [golangci-lint](https://golangci-lint.run/) at the version
pinned in `.github/workflows/ci.yml`. `grcli validate` needs `cue` on `PATH`;
key-based signing (`--cosign-key`) needs `cosign` ≥ 2.6.0. Keyless signing and
all verification are in-process and need neither.

## Pull requests

- Keep PRs focused; one change per PR.
- Add or update a test for behaviour you change. Every command has a `_test.go`
  next to it in `cmd/`, and `cmd/integration_test.go` covers end-to-end flows.
- Note user-visible changes in `CHANGELOG.md`. This project is pre-1.0: while
  on `v0.x`, a breaking change bumps the minor version.
- Do not add secrets or tokens to workflows. Publishing uses the GitHub Actions
  OIDC token (`permissions: id-token: write`) and nothing else.

## Reporting security issues

See [SECURITY.md](SECURITY.md). Please do not open a public issue for a
vulnerability.
