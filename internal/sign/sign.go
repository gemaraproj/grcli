// SPDX-License-Identifier: Apache-2.0

// Package sign signs a pushed artifact as a separate step after push.
//
// Keyless signing (the CI trusted-publishing path) runs IN-PROCESS via
// sigstore-go — the same library internal/sigverify uses to verify — so
// publishing needs no cosign (symmetric to the in-process verify). See keyless.go. Key-based signing (--cosign-key) still shells out to
// cosign, the one remaining path that needs it on PATH.
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

	"github.com/gemaraproj/grcli/internal/registry"
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
// produces.
//
// It is EXPORTED so the verify side (cmd/verify.go) references the same constant
// — a bundle-signed artifact is verified with `cosign verify --new-bundle-format`
// and does NOT verify against the legacy `.sig` path (and vice versa), so sign
// and verify MUST stay a matched pair. Sharing one constant makes that structural,
// not coincidental.
//
// The flag is NOT passed unconditionally — it only exists on a bounded band of
// cosign versions. Callers select it via BundleFormatArgs, which gates on the
// detected cosign version. See that function for the rationale.
const FlagNewBundleFormat = "--new-bundle-format"

// minBundleFormatCosign is the oldest cosign whose SIGN command understands
// --new-bundle-format: cosign added it to `verify` in 2.4.0 but to `sign`
// only in 2.6.0 (checked against the release tags' options/sign.go — 2.4.x
// and 2.5.x abort with `unknown flag: --new-bundle-format`). Since this
// helper feeds the sign path, the floor is the sign flag's, not verify's.
const minBundleFormatCosign = "v2.6.0"

// bundleDefaultCosign is the cosign version at which the Sigstore bundle format
// became the DEFAULT and --new-bundle-format was deprecated (cosign 3.0.0). At
// or above this the flag is redundant, prints a deprecation warning on every
// invocation, and is slated for removal — so we omit it and rely on the default.
const bundleDefaultCosign = "v3.0.0"

// BundleFormatArgs returns the cosign CLI flags that select grc.store's Sigstore
// bundle signature format for the cosign currently on PATH, gating on its
// version so grcli works across the whole supported cosign range instead of the
// narrow 2.4.0–2.6.x band the flag was hard-coded for:
//
//	cosign < 2.6.0          → error   (flag doesn't exist on `sign`; fail fast with a
//	                                    clear message instead of cosign's raw `unknown flag`)
//	2.6.0 ≤ cosign < 3.0.0  → ["--new-bundle-format"]  (flag is first-class here)
//	cosign ≥ 3.0.0          → nil     (bundle format is the default; passing the
//	                                    deprecated flag only warns and will break
//	                                    when cosign removes it)
//	version undeterminable   → error   (fail closed — guessing wrong silently
//	                                    produces a format the verify side rejects)
//
// Both the sign path and the key-based verify shell-out call this, so a
// bundle-signed artifact is always verified as a bundle: the two stay a matched
// pair by construction, not convention.
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

// detectCosignVersion returns the canonical, v-prefixed semver reported by the
// cosign on PATH. It prefers `cosign version --json` (stable since cosign 2.x)
// and falls back to scraping the `GitVersion:` line of the human-readable
// output. It fails CLOSED: an unparseable version — a source `devel` build, a
// pseudo-version, a truncated string — is an error, because selecting the wrong
// signature format silently produces a signature the verify side won't accept.
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

// cosignVersionString returns cosign's self-reported version string (e.g.
// "v3.0.6"), preferring the machine-readable `--json` form and falling back to
// the GitVersion: line of plain `cosign version`.
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

// Result is what Sign returns to the caller for logging.
type Result struct {
	Mode   Mode
	Reason string // populated when Mode == ModeSkipped
}

// Options carries the user-facing knobs.
type Options struct {
	// Disabled is set by --no-sign; when true we never sign.
	Disabled bool
	// KeyPath is the cosign key file path; equivalent to cosign sign --key.
	// Selects the key-based (cosign shell-out) path. Empty in CI, where the
	// keyless in-process path is used.
	KeyPath string
	// Reference is the full <registry>/<repository>:<tag> — used for display
	// and as the cosign key-mode target.
	Reference string
	// PlainHTTP signals the registry speaks plain HTTP (a local dev zot).
	// For key mode, cosign gets --allow-http-registry; for keyless, the
	// scheme in RegistryHost drives it.
	PlainHTTP bool

	// RegistryHost, Repository, and ManifestDigest are the coordinates the
	// keyless in-process path needs: it signs ManifestDigest and
	// attaches the bundle as an OCI referrer at RegistryHost/Repository. Unset
	// for key mode (cosign resolves the reference itself). RegistryHost keeps
	// any http(s):// scheme so the oras push targets the right transport.
	RegistryHost   string
	Repository     string
	ManifestDigest string
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
//	cosign out of range  → error (too old for the bundle format; see BundleFormatArgs)
//	GITHUB_ACTIONS=true   → ok if id-token is available, else error
//	KeyPath != ""        → ok
//	otherwise            → error (no signing material)
//
// The one thing it DOES run is `cosign version` (via BundleFormatArgs) — a
// cheap, side-effect-free probe — so an out-of-band cosign fails here, before
// any bytes are pushed, rather than after Sign shells out and cosign rejects the
// signature flag.
func Preflight(ctx context.Context, opts Options) error {
	if opts.Disabled {
		return nil
	}
	switch {
	case os.Getenv("GITHUB_ACTIONS") == "true":
		// Keyless in-process: NO cosign needed — grcli requests the
		// GHA OIDC token itself and signs via sigstore-go. Requires
		// `permissions: id-token: write` (which populates the request env).
		if os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN") == "" {
			return errors.New("GITHUB_ACTIONS=true but ACTIONS_ID_TOKEN_REQUEST_TOKEN is unset — " +
				"add `permissions: id-token: write` to the workflow for keyless signing, or pass --no-sign")
		}
		return nil
	case opts.KeyPath != "":
		// Key-based signing is the ONLY path that still shells out to cosign.
		if _, err := exec.LookPath("cosign"); err != nil {
			return errors.New("cosign not found on PATH — required only for --cosign-key (key-based) signing; " +
				"keyless CI signing needs no external tools. Install cosign, or pass --no-sign")
		}
		if _, err := BundleFormatArgs(ctx); err != nil {
			return err
		}
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
//	GITHUB_ACTIONS=true   → ModeKeyless, in-process via sigstore-go (error if id-token missing)
//	KeyPath != ""        → ModeKey, cosign shell-out (error if cosign absent)
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
	if err := Preflight(ctx, opts); err != nil {
		return nil, fmt.Errorf("sign: %w", err)
	}

	// Keyless in CI runs fully in-process: sign the manifest digest
	// via sigstore-go and attach the bundle as an OCI referrer — no cosign.
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		if opts.ManifestDigest == "" || opts.RegistryHost == "" || opts.Repository == "" {
			return nil, errors.New("sign: keyless signing needs the manifest digest, registry host, and repository")
		}
		bundleJSON, err := signKeylessInProcess(ctx, opts.ManifestDigest)
		if err != nil {
			return nil, fmt.Errorf("keyless sign: %w", err)
		}
		if err := registry.AttachSignatureReferrer(ctx, opts.RegistryHost, opts.Repository, opts.ManifestDigest, bundleJSON); err != nil {
			return nil, fmt.Errorf("attaching signature to registry: %w", err)
		}
		return &Result{Mode: ModeKeyless}, nil
	}

	// Key-based signing shells out to cosign (the one remaining cosign path).
	// Select the signature-format flag for the detected cosign (empty on
	// cosign ≥ 3.0.0). Preflight already validated the version.
	bundleArgs, err := BundleFormatArgs(ctx)
	if err != nil {
		return nil, fmt.Errorf("sign: %w", err)
	}
	args := append([]string{"sign", "--yes"}, bundleArgs...)
	args = append(args, "--key", opts.KeyPath)
	args = append(args, registryFlags(opts)...)
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
