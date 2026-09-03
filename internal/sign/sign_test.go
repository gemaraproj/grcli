// SPDX-License-Identifier: Apache-2.0

package sign

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCosignScript builds a /bin/sh body for a fake cosign that answers
// `cosign version[ --json]` with the given semver (Preflight probes the version
// via BundleFormatArgs), and — when argsFile != "" — appends every arg of any
// OTHER invocation to argsFile so a test can assert the exact sign/verify flags.
func fakeCosignScript(version, argsFile string) string {
	s := "#!/bin/sh\n" +
		"if [ \"$1\" = version ]; then printf '{\"gitVersion\":\"" + version + "\"}\\n'; exit 0; fi\n"
	if argsFile != "" {
		s += "for a in \"$@\"; do printf '%s\\n' \"$a\" >> " + argsFile + "; done\n"
	}
	return s + "exit 0\n"
}

// cosignOnPath puts a dummy cosign on PATH so the LookPath check passes and the
// version probe reports an in-band version. It doesn't record args.
func cosignOnPath(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cosign"), []byte(fakeCosignScript("v2.6.3", "")), 0o755); err != nil {
		t.Fatalf("write fake cosign: %v", err)
	}
	t.Setenv("PATH", dir)
}

// cosignAbsent points PATH at an empty dir so LookPath("cosign") fails.
func cosignAbsent(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

func TestPreflight(t *testing.T) {
	t.Run("--no-sign is the one allowed skip, even with nothing available", func(t *testing.T) {
		cosignAbsent(t)
		t.Setenv("GITHUB_ACTIONS", "")
		t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "")
		if err := Preflight(context.Background(), Options{Disabled: true}); err != nil {
			t.Fatalf("--no-sign must pass preflight, got %v", err)
		}
	})

	t.Run("CI keyless needs NO cosign on PATH", func(t *testing.T) {
		cosignAbsent(t)
		t.Setenv("GITHUB_ACTIONS", "true")
		t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "tok")
		if err := Preflight(context.Background(), Options{}); err != nil {
			t.Fatalf("keyless CI signing is in-process and must NOT require cosign, got %v", err)
		}
	})

	t.Run("--cosign-key without cosign fails closed", func(t *testing.T) {
		cosignAbsent(t)
		t.Setenv("GITHUB_ACTIONS", "")
		t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "")
		err := Preflight(context.Background(), Options{KeyPath: "/keys/x.key"})
		if err == nil || !strings.Contains(err.Error(), "cosign") {
			t.Fatalf("want a cosign-not-found error for --cosign-key, got %v", err)
		}
	})

	t.Run("CI with id-token passes (keyless)", func(t *testing.T) {
		cosignOnPath(t)
		t.Setenv("GITHUB_ACTIONS", "true")
		t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "tok")
		if err := Preflight(context.Background(), Options{}); err != nil {
			t.Fatalf("CI keyless should pass, got %v", err)
		}
	})

	t.Run("CI without id-token fails closed", func(t *testing.T) {
		cosignOnPath(t)
		t.Setenv("GITHUB_ACTIONS", "true")
		t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "")
		err := Preflight(context.Background(), Options{})
		if err == nil || !strings.Contains(err.Error(), "id-token") {
			t.Fatalf("want an id-token error, got %v", err)
		}
	})

	t.Run("local with --cosign-key passes", func(t *testing.T) {
		cosignOnPath(t)
		t.Setenv("GITHUB_ACTIONS", "")
		t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "")
		if err := Preflight(context.Background(), Options{KeyPath: "/keys/x.key"}); err != nil {
			t.Fatalf("local key should pass, got %v", err)
		}
	})

	t.Run("local with no key and no CI fails closed", func(t *testing.T) {
		cosignOnPath(t)
		t.Setenv("GITHUB_ACTIONS", "")
		t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "")
		err := Preflight(context.Background(), Options{})
		if err == nil || !strings.Contains(err.Error(), "signing material") {
			t.Fatalf("want a no-signing-material error, got %v", err)
		}
	})
}

// recordingCosign installs a fake cosign that reports the given version and
// appends the args of any non-version invocation (one per line) to a file,
// returning that file's path. Lets a test assert the exact flags grcli passes
// for a chosen cosign version, without a real registry.
func recordingCosign(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	if err := os.WriteFile(filepath.Join(dir, "cosign"), []byte(fakeCosignScript(version, argsFile)), 0o755); err != nil {
		t.Fatalf("write recording cosign: %v", err)
	}
	t.Setenv("PATH", dir)
	return argsFile
}

// TestSignBundleFormatByCosignVersion pins that grcli selects the Sigstore
// bundle-as-referrer format correctly across the cosign range on the
// KEY-based path — the only path that still shells out to cosign (keyless
// moved in-process, so it no longer invokes cosign at all). It passes
// --new-bundle-format on cosign 2.6–2.x and omits it on ≥ 3.0.0 (where the
// bundle format is the default and the flag is deprecated).
func TestSignBundleFormatByCosignVersion(t *testing.T) {
	cases := []struct {
		name     string
		version  string
		wantFlag bool
	}{
		{"2.6.x band passes the flag", "v2.6.3", true},
		{"3.x omits the deprecated flag", "v3.0.6", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			argsFile := recordingCosign(t, tc.version)
			t.Setenv("GITHUB_ACTIONS", "")
			t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "")
			keyPath := filepath.Join(t.TempDir(), "cosign.key")
			if err := os.WriteFile(keyPath, []byte("x"), 0o600); err != nil {
				t.Fatalf("write key: %v", err)
			}
			if _, err := Sign(context.Background(), Options{Reference: "reg/repo:1", KeyPath: keyPath}); err != nil {
				t.Fatalf("sign: %v", err)
			}
			assertBundleFlag(t, argsFile, tc.wantFlag)
		})
	}
}

func assertBundleFlag(t *testing.T, argsFile string, want bool) {
	t.Helper()
	got, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	if has := strings.Contains(string(got), "--new-bundle-format"); has != want {
		t.Errorf("--new-bundle-format present=%v, want %v; sign args:\n%s", has, want, got)
	}
}

// TestBundleFormatArgsRejectsOutOfRangeCosign pins the fail-fast behavior: a
// cosign too old for the flag, or one whose version can't be parsed, is a clear
// grcli error rather than cosign's raw `unknown flag` surfacing after a push.
func TestBundleFormatArgsRejectsOutOfRangeCosign(t *testing.T) {
	t.Run("too old names the required version", func(t *testing.T) {
		recordingCosign(t, "v2.2.0")
		_, err := BundleFormatArgs(context.Background())
		if err == nil || !strings.Contains(err.Error(), "2.6.0") {
			t.Fatalf("want a too-old error naming cosign 2.6.0, got %v", err)
		}
	})

	// Regression pin for the 2.4.x–2.5.x dead zone: those cosigns accept
	// --new-bundle-format on `verify` but NOT on `sign` (the flag reached
	// `sign` only in 2.6.0), so they must be rejected up front rather than
	// die mid-publish on cosign's raw `unknown flag`. Caught live by a
	// GitHub Actions publish pinned to cosign v2.5.2 (2026-07-07).
	t.Run("2.5.x is in the sign-flag dead zone", func(t *testing.T) {
		recordingCosign(t, "v2.5.2")
		_, err := BundleFormatArgs(context.Background())
		if err == nil || !strings.Contains(err.Error(), "2.6.0") {
			t.Fatalf("want a too-old error naming cosign 2.6.0, got %v", err)
		}
	})

	t.Run("unparseable version fails closed", func(t *testing.T) {
		recordingCosign(t, "devel")
		_, err := BundleFormatArgs(context.Background())
		if err == nil || !strings.Contains(err.Error(), "determine the cosign version") {
			t.Fatalf("want an undeterminable-version error, got %v", err)
		}
	})
}

func TestSignFailsClosed(t *testing.T) {
	t.Run("--no-sign returns ModeSkipped without error", func(t *testing.T) {
		cosignAbsent(t)
		r, err := Sign(context.Background(), Options{Disabled: true, Reference: "reg/repo:1"})
		if err != nil {
			t.Fatalf("--no-sign should not error: %v", err)
		}
		if r.Mode != ModeSkipped {
			t.Errorf("Mode = %q, want skipped", r.Mode)
		}
	})

	t.Run("empty reference errors", func(t *testing.T) {
		if _, err := Sign(context.Background(), Options{}); err == nil {
			t.Fatal("want error for empty reference")
		}
	})

	t.Run("cannot sign is an error, never a silent unsigned publish", func(t *testing.T) {
		cosignAbsent(t)
		t.Setenv("GITHUB_ACTIONS", "")
		t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "")
		r, err := Sign(context.Background(), Options{Reference: "reg/repo:1"})
		if err == nil {
			t.Fatalf("want error when signing material/cosign is missing, got result %+v", r)
		}
		if !strings.Contains(err.Error(), "cosign") {
			t.Errorf("error = %v, want it to mention cosign", err)
		}
	})
}
