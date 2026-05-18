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
		if err := runCosign(ctx, "sign", "--yes", opts.Reference); err != nil {
			return nil, fmt.Errorf("cosign keyless sign: %w", err)
		}
		return &Result{Mode: ModeKeyless}, nil
	}

	if opts.KeyPath != "" {
		args := []string{"sign", "--yes", "--key", opts.KeyPath, opts.Reference}
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
