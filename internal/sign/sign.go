// SPDX-License-Identifier: Apache-2.0

// Package sign is grcli's cosign shell-out for --cosign-key (key-based)
// signing, plus the cosign version gate `grcli verify` shares. Keyless
// signing is in-process via grc-store-clientkit/keyless and never touches
// this package.
package sign

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/mod/semver"
)

// Mode names how a publish was (or wasn't) signed, for status output.
type Mode string

const (
	ModeKeyless Mode = "keyless"
	ModeKey     Mode = "key"
	ModeSkipped Mode = "skipped"
)

// FlagNewBundleFormat is cosign 2.6.x's opt-in to the Sigstore bundle
// signature format; cosign 3.x emits it by default and rejects the flag.
const FlagNewBundleFormat = "--new-bundle-format"

const (
	minBundleFormatCosign = "v2.6.0"
	bundleDefaultCosign   = "v3.0.0"
)

// BundleFormatArgs returns the extra cosign args that select the bundle
// signature format for the installed cosign, or an error when that cosign
// cannot produce it.
func BundleFormatArgs(ctx context.Context) ([]string, error) {
	v, err := detectCosignVersion(ctx)
	if err != nil {
		return nil, err
	}
	switch {
	case semver.Compare(v, minBundleFormatCosign) < 0:
		return nil, fmt.Errorf("cosign %s is too old for grc.store's Sigstore bundle "+
			"signature format, which needs cosign ≥ 2.6.0 — pin a newer cosign "+
			"(e.g. sigstore/cosign-installer with a version ≥ v2.6.0), or pass "+
			"--no-sign to publish without provenance", v)
	case semver.Compare(v, bundleDefaultCosign) < 0:
		return []string{FlagNewBundleFormat}, nil
	default:
		return nil, nil
	}
}

func detectCosignVersion(ctx context.Context) (string, error) {
	raw, err := cosignVersionString(ctx)
	if err != nil {
		return "", err
	}
	v := raw
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	if !semver.IsValid(v) {
		return "", fmt.Errorf("could not determine the cosign version (got %q) — "+
			"install a released cosign ≥ 2.4.0 so grcli can select the correct "+
			"signature format, or pass --no-sign", raw)
	}
	return semver.Canonical(v), nil
}

func cosignVersionString(ctx context.Context) (string, error) {
	if out, err := exec.CommandContext(ctx, "cosign", "version", "--json").Output(); err == nil {
		var payload struct {
			GitVersion string `json:"gitVersion"`
		}
		if json.Unmarshal(out, &payload) == nil && payload.GitVersion != "" {
			return strings.TrimSpace(payload.GitVersion), nil
		}
	}
	out, err := exec.CommandContext(ctx, "cosign", "version").Output()
	if err != nil {
		return "", fmt.Errorf("running `cosign version`: %w", err)
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "GitVersion:"); ok {
			return strings.TrimSpace(rest), nil
		}
	}
	return "", errors.New("could not parse `cosign version` output for a GitVersion")
}

// KeyOptions drives one cosign key signing of a pushed reference.
type KeyOptions struct {
	KeyPath   string
	Reference string // <host>/<repository>:<tag>, bare host
	PlainHTTP bool
	// RegistryToken is the minted push token, passed as --registry-token.
	// Empty means cosign's own credential chain (plus the GRC_STORE_ /
	// GRCLI_ username+password env pair when set).
	RegistryToken string
}

// KeyPreflight fails fast, before any bytes are pushed, when cosign is
// missing or too old for key-based signing.
func KeyPreflight(ctx context.Context) error {
	if _, err := exec.LookPath("cosign"); err != nil {
		return errors.New("cosign not found on PATH — required only for --cosign-key (key-based) signing; " +
			"keyless signing needs no external tools. Install cosign, or pass --no-sign")
	}
	_, err := BundleFormatArgs(ctx)
	return err
}

// SignWithKey runs `cosign sign --key` against the reference.
func SignWithKey(ctx context.Context, opts KeyOptions) error {
	if opts.Reference == "" {
		return errors.New("sign: empty reference")
	}
	if opts.KeyPath == "" {
		return errors.New("sign: key path is required")
	}
	bundleArgs, err := BundleFormatArgs(ctx)
	if err != nil {
		return err
	}
	args := append([]string{"sign", "--yes"}, bundleArgs...)
	args = append(args, "--key", opts.KeyPath)
	args = append(args, registryFlags(opts)...)
	args = append(args, opts.Reference)
	cmd := exec.CommandContext(ctx, "cosign", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("cosign key sign: %w", err)
	}
	return nil
}

func registryFlags(opts KeyOptions) []string {
	var args []string
	switch {
	case opts.RegistryToken != "":
		args = []string{"--registry-token", opts.RegistryToken}
	default:
		u, p := os.Getenv("GRC_STORE_REGISTRY_USERNAME"), os.Getenv("GRC_STORE_REGISTRY_PASSWORD")
		if u == "" || p == "" {
			u, p = os.Getenv("GRCLI_REGISTRY_USERNAME"), os.Getenv("GRCLI_REGISTRY_PASSWORD")
		}
		if u != "" && p != "" {
			args = []string{"--registry-username", u, "--registry-password", p}
		}
	}
	if opts.PlainHTTP {
		args = append(args, "--allow-http-registry")
	}
	return args
}
