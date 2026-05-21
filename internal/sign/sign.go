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
	// If empty and not in CI, we skip with a reason.
	KeyPath string
	// Reference is the full <registry>/<repository>:<tag> to sign.
	Reference string
	// PlainHTTP signals the registry speaks plain HTTP (a local dev zot),
	// so cosign needs --allow-http-registry to push the signature instead
	// of defaulting to HTTPS. Off for production HTTPS registries.
	PlainHTTP bool
}

// Sign attempts to attach a cosign signature to the pushed manifest.
//
// Decision tree:
//
//	--no-sign           → ModeSkipped, no error
//	cosign not on PATH  → ModeSkipped, no error  (with a reason)
//	GITHUB_ACTIONS=true → ModeKeyless via OIDC
//	KeyPath != ""       → ModeKey
//	otherwise           → ModeSkipped, no error  (with a reason)
//
// Signing failure (cosign returns nonzero) is an error — once we've
// decided to sign, the caller almost certainly wants to know it broke.
func Sign(ctx context.Context, opts Options) (*Result, error) {
	if opts.Disabled {
		return &Result{Mode: ModeSkipped, Reason: "--no-sign"}, nil
	}
	if opts.Reference == "" {
		return nil, errors.New("sign: empty reference")
	}
	if _, err := exec.LookPath("cosign"); err != nil {
		return &Result{Mode: ModeSkipped, Reason: "cosign not on PATH"}, nil
	}

	if os.Getenv("GITHUB_ACTIONS") == "true" {
		// Keyless: cosign reads the GHA OIDC token from the runtime
		// env (ACTIONS_ID_TOKEN_REQUEST_TOKEN / _URL). The workflow
		// must set `permissions: id-token: write` for that to work,
		// which we surface in the warning below if the token vars
		// aren't present.
		if os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN") == "" {
			return &Result{Mode: ModeSkipped,
				Reason: "GITHUB_ACTIONS=true but ACTIONS_ID_TOKEN_REQUEST_TOKEN unset — set `permissions: id-token: write` in the workflow",
			}, nil
		}
		args := append([]string{"sign", "--yes"}, registryFlags(opts)...)
		args = append(args, opts.Reference)
		if err := runCosign(ctx, args...); err != nil {
			return nil, fmt.Errorf("cosign keyless sign: %w", err)
		}
		return &Result{Mode: ModeKeyless}, nil
	}

	if opts.KeyPath != "" {
		args := append([]string{"sign", "--yes", "--key", opts.KeyPath}, registryFlags(opts)...)
		args = append(args, opts.Reference)
		if err := runCosign(ctx, args...); err != nil {
			return nil, fmt.Errorf("cosign key sign: %w", err)
		}
		return &Result{Mode: ModeKey}, nil
	}

	return &Result{Mode: ModeSkipped,
		Reason: "no signing material — pass --cosign-key for local signing or run in GitHub Actions with id-token: write",
	}, nil
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
