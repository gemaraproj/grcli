// SPDX-License-Identifier: LicenseRef-Revanite-Proprietary

// Package sign drives optional cosign signing as a separate step after
// push. The integration is a shell-out: we don't vendor sigstore into
// grcli — the surface area is too large for a feature whose verifier
// half doesn't exist on the hub yet. cosign-on-PATH is the contract;
// CI runners and most developer environments have it.
package sign

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
)

// Mode reports how sign() resolved its trust material.
type Mode string

const (
	ModeKeyless Mode = "keyless"
	ModeKey     Mode = "key"
	ModeSkipped Mode = "skipped"
)

// FlagNewBundleFormat makes cosign store the signature as a Sigstore **bundle**
// (media type application/vnd.dev.sigstore.bundle.v0.3+json) attached as an OCI
// 1.1 referrer of the manifest, instead of the legacy tag-based `sha256-….sig`.
// This converges grc.store on one signature format across artifact types: it is
// the format the hub's plugin verifier already expects and that pvtr already
// produces (ADR-0034 dec. 7, ADR-0035).
//
// It is EXPORTED so the verify side (cmd/verify.go) references the same constant
// — a bundle-signed artifact is verified with `cosign verify --new-bundle-format`
// and does NOT verify against the legacy `.sig` path (and vice versa), so sign
// and verify MUST stay a matched pair. Sharing one constant makes that structural,
// not coincidental.
const FlagNewBundleFormat = "--new-bundle-format"

// Result is what Sign returns to the caller for logging.
type Result struct {
	Mode   Mode
	Reason string // populated when Mode == ModeSkipped
}

// Options carries the user-facing knobs.
type Options struct {
	// Disabled is set by --no-sign; when true we never invoke cosign.
	Disabled bool
	// KeyPath is the cosign key file path; equivalent to cosign sign --key.
	// If empty and not in CI, signing fails (the publish errors) unless
	// Disabled (--no-sign) is set.
	KeyPath string
	// Reference is the full <registry>/<repository>:<tag> to sign.
	Reference string
	// PlainHTTP signals the registry speaks plain HTTP (a local dev zot),
	// so cosign needs --allow-http-registry to push the signature instead
	// of defaulting to HTTPS. Off for production HTTPS registries.
	PlainHTTP bool
}

// Preflight reports whether a subsequent Sign call will be able to
// produce a signature — WITHOUT running cosign — so callers can fail
// before pushing rather than orphan unsigned bytes in the registry.
//
// It fails CLOSED: anything short of "we can sign" is an error, because
// an unsigned artifact has no verifiable provenance and the hub does not
// reject it on ingest. The single deliberate exception is --no-sign.
//
//	--no-sign            → ok (publishing unsigned is an explicit choice)
//	cosign not on PATH   → error
//	GITHUB_ACTIONS=true   → ok if id-token is available, else error
//	KeyPath != ""        → ok
//	otherwise            → error (no signing material)
func Preflight(opts Options) error {
	if opts.Disabled {
		return nil
	}
	if _, err := exec.LookPath("cosign"); err != nil {
		return errors.New("cosign not found on PATH — install it " +
			"(e.g. the sigstore/cosign-installer step in CI) so the publish can be signed, " +
			"or pass --no-sign to publish without provenance")
	}
	switch {
	case os.Getenv("GITHUB_ACTIONS") == "true":
		// Keyless: cosign reads the GHA OIDC token from the runtime env
		// (ACTIONS_ID_TOKEN_REQUEST_TOKEN / _URL), which requires
		// `permissions: id-token: write` on the workflow.
		if os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN") == "" {
			return errors.New("GITHUB_ACTIONS=true but ACTIONS_ID_TOKEN_REQUEST_TOKEN is unset — " +
				"add `permissions: id-token: write` to the workflow for keyless signing, or pass --no-sign")
		}
		return nil
	case opts.KeyPath != "":
		return nil
	default:
		return errors.New("no signing material — pass --cosign-key (or COSIGN_KEY) for local signing, " +
			"run in GitHub Actions with `permissions: id-token: write` for keyless signing, " +
			"or pass --no-sign to publish without provenance")
	}
}

// Sign attaches a cosign signature to the pushed manifest. It fails
// CLOSED — the only path that returns ModeSkipped is --no-sign; every
// other inability to sign (no cosign, no key/CI material, cosign error)
// is an error, so a publish never silently downgrades to unsigned.
//
// Decision tree:
//
//	--no-sign            → ModeSkipped, no error
//	cosign not on PATH   → error
//	GITHUB_ACTIONS=true   → ModeKeyless via OIDC (error if id-token missing)
//	KeyPath != ""        → ModeKey
//	otherwise            → error (no signing material)
//
// Callers should run Preflight before pushing; Sign repeats the same
// checks as a backstop because it runs after the bytes are already in
// the registry.
func Sign(ctx context.Context, opts Options) (*Result, error) {
	if opts.Disabled {
		return &Result{Mode: ModeSkipped, Reason: "--no-sign"}, nil
	}
	if opts.Reference == "" {
		return nil, errors.New("sign: empty reference")
	}
	if err := Preflight(opts); err != nil {
		return nil, fmt.Errorf("sign: %w", err)
	}

	// Preflight guarantees cosign is present and (GHA-with-id-token OR a
	// key) is available. Prefer keyless in CI, mirroring the old order.
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		args := append([]string{"sign", "--yes", FlagNewBundleFormat}, registryFlags(opts)...)
		args = append(args, opts.Reference)
		if err := runCosign(ctx, args...); err != nil {
			return nil, fmt.Errorf("cosign keyless sign: %w", err)
		}
		return &Result{Mode: ModeKeyless}, nil
	}
	args := append([]string{"sign", "--yes", FlagNewBundleFormat, "--key", opts.KeyPath}, registryFlags(opts)...)
	args = append(args, opts.Reference)
	if err := runCosign(ctx, args...); err != nil {
		return nil, fmt.Errorf("cosign key sign: %w", err)
	}
	return &Result{Mode: ModeKey}, nil
}

func runCosign(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "cosign", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// registryCredArgs returns cosign registry-auth flags derived from the
// same GRCLI_REGISTRY_* env vars grcli's oras push honors (see
// internal/registry.dockerCredentials), or nil when none are set.
//
// Why this is needed: the cosign subprocess has its own credential
// chain (the Docker config) and does NOT read GRCLI_REGISTRY_*. Now
// that the registry rejects anonymous writes, an env-var-only publish
// would push the bundle and then 401 when cosign pushes the signature
// to the same repository. Forwarding the creds makes the env-var path a
// complete publish flow; `docker login` remains a valid alternative
// (cosign reads it natively, so we forward nothing and rely on the
// chain in that case).
//
// Precedence mirrors dockerCredentials: username+password first, then a
// raw bearer token.
func registryCredArgs() []string {
	if u, p := os.Getenv("GRCLI_REGISTRY_USERNAME"), os.Getenv("GRCLI_REGISTRY_PASSWORD"); u != "" && p != "" {
		return []string{"--registry-username", u, "--registry-password", p}
	}
	if t := os.Getenv("GRCLI_REGISTRY_TOKEN"); t != "" {
		return []string{"--registry-token", t}
	}
	return nil
}

// registryFlags is the full set of cosign registry-auth/transport flags
// for a sign run: the credential args plus --allow-http-registry when the
// target is a plain-HTTP (local dev) registry.
func registryFlags(opts Options) []string {
	args := registryCredArgs()
	if opts.PlainHTTP {
		args = append(args, "--allow-http-registry")
	}
	return args
}
