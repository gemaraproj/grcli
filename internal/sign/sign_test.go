// SPDX-License-Identifier: LicenseRef-Revanite-Proprietary

package sign

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cosignOnPath puts a dummy executable named "cosign" on PATH so the
// LookPath check passes. Preflight never runs it, so the contents don't
// matter — only that it's an executable file LookPath can find.
func cosignOnPath(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cosign"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
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
		if err := Preflight(Options{Disabled: true}); err != nil {
			t.Fatalf("--no-sign must pass preflight, got %v", err)
		}
	})

	t.Run("cosign not on PATH fails closed", func(t *testing.T) {
		cosignAbsent(t)
		t.Setenv("GITHUB_ACTIONS", "true")
		t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "tok") // material present, but no cosign
		err := Preflight(Options{})
		if err == nil || !strings.Contains(err.Error(), "cosign") {
			t.Fatalf("want a cosign-not-found error, got %v", err)
		}
	})

	t.Run("CI with id-token passes (keyless)", func(t *testing.T) {
		cosignOnPath(t)
		t.Setenv("GITHUB_ACTIONS", "true")
		t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "tok")
		if err := Preflight(Options{}); err != nil {
			t.Fatalf("CI keyless should pass, got %v", err)
		}
	})

	t.Run("CI without id-token fails closed", func(t *testing.T) {
		cosignOnPath(t)
		t.Setenv("GITHUB_ACTIONS", "true")
		t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "")
		err := Preflight(Options{})
		if err == nil || !strings.Contains(err.Error(), "id-token") {
			t.Fatalf("want an id-token error, got %v", err)
		}
	})

	t.Run("local with --cosign-key passes", func(t *testing.T) {
		cosignOnPath(t)
		t.Setenv("GITHUB_ACTIONS", "")
		t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "")
		if err := Preflight(Options{KeyPath: "/keys/x.key"}); err != nil {
			t.Fatalf("local key should pass, got %v", err)
		}
	})

	t.Run("local with no key and no CI fails closed", func(t *testing.T) {
		cosignOnPath(t)
		t.Setenv("GITHUB_ACTIONS", "")
		t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "")
		err := Preflight(Options{})
		if err == nil || !strings.Contains(err.Error(), "signing material") {
			t.Fatalf("want a no-signing-material error, got %v", err)
		}
	})
}

// recordingCosign installs a fake cosign that appends its args (one per line)
// to a file, and returns that file's path. Lets a test assert the exact flags
// grcli passes without a real registry.
func recordingCosign(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >> " + argsFile + "; done\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "cosign"), []byte(script), 0o755); err != nil {
		t.Fatalf("write recording cosign: %v", err)
	}
	t.Setenv("PATH", dir)
	return argsFile
}

// TestSignPassesNewBundleFormat pins that grcli signs with the Sigstore
// bundle-as-referrer format (ADR-0035) in BOTH the keyless and key paths — the
// format the hub's plugin verifier expects and pvtr already produces.
func TestSignPassesNewBundleFormat(t *testing.T) {
	t.Run("keyless", func(t *testing.T) {
		argsFile := recordingCosign(t)
		t.Setenv("GITHUB_ACTIONS", "true")
		t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "tok")
		if _, err := Sign(context.Background(), Options{Reference: "reg/repo:1"}); err != nil {
			t.Fatalf("sign: %v", err)
		}
		got, err := os.ReadFile(argsFile)
		if err != nil {
			t.Fatalf("read args: %v", err)
		}
		if !strings.Contains(string(got), "--new-bundle-format") {
			t.Errorf("keyless sign args missing --new-bundle-format; got:\n%s", got)
		}
	})

	t.Run("key", func(t *testing.T) {
		argsFile := recordingCosign(t)
		t.Setenv("GITHUB_ACTIONS", "")
		t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "")
		keyPath := filepath.Join(t.TempDir(), "cosign.key")
		if err := os.WriteFile(keyPath, []byte("x"), 0o600); err != nil {
			t.Fatalf("write key: %v", err)
		}
		if _, err := Sign(context.Background(), Options{Reference: "reg/repo:1", KeyPath: keyPath}); err != nil {
			t.Fatalf("sign: %v", err)
		}
		got, err := os.ReadFile(argsFile)
		if err != nil {
			t.Fatalf("read args: %v", err)
		}
		if !strings.Contains(string(got), "--new-bundle-format") {
			t.Errorf("key sign args missing --new-bundle-format; got:\n%s", got)
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
