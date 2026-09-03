# Security Policy

`grcli` signs, publishes and verifies compliance artifacts. A flaw in its
signing or verification path can let a tampered artifact pass as trusted, so
we take reports seriously and welcome responsible disclosure.

## Reporting a vulnerability

**Please do not file public GitHub issues for security vulnerabilities.**

Open a private advisory at
https://github.com/gemaraproj/grcli/security/advisories/new. It gives us a
private space to work with you and integrates with CVE issuance.

Please include:

- A description of the issue and the affected path (`publish`, `verify`,
  `unpack`/`cat`, the cache, a workflow under `.github/`, …).
- Reproduction steps or a proof of concept where possible.
- The release tag or commit you tested against.

## What to expect

- We aim to acknowledge new reports within **3 business days** and give a
  triage assessment within **10 business days**.
- For accepted reports we coordinate a fix and disclosure timeline with you.
  The default embargo is **90 days** from the initial report.
- We credit reporters in the published advisory unless asked not to.

## Supported versions

`grcli` is pre-1.0. Security fixes land on `main` and ship in the next tagged
release; only the latest release is supported.

## Scope

In scope: everything in this repository, including the workflows and the
reusable publish workflow / install action under `.github/`.

Out of scope: the hub service, the OCI registry, and Sigstore infrastructure,
which are separate projects with their own policies.
