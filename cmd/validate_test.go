// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// minimalValidPolicy is the smallest YAML body that satisfies the
// Gemara #Policy schema today. The roundtrip tests in integration_test.go
// use a much sparser fixture (policyYAML) that exercises grcli's own
// peek + bundle logic without requiring schema validity; validate's
// tests need schema-valid input.
const minimalValidPolicy = `metadata:
  id: min-policy
  type: Policy
  version: 1.0.0
  gemara-version: 0.20.0
  description: Minimal test policy
  author:
    id: tester
    name: Test Author
    type: Human
title: Minimal Test Policy
contacts:
  responsible:
    - name: Owner
  accountable:
    - name: Accountable
`

// The happy-path validate tests need both `cue` on PATH and a local
// Gemara spec checkout to vet against. CI environments without either
// skip; this is the same pattern grc.store-backend's drift check uses
// (GEMARA_SPEC_DIR env or sibling checkout).
//
// On the dev workstation the spec lives at ../gemara relative to this
// repo; we also honor GRCLI_GEMARA_SPEC_DIR for explicit overrides.
func findSpecDir(t *testing.T) string {
	t.Helper()
	if dir := os.Getenv("GRCLI_GEMARA_SPEC_DIR"); dir != "" {
		return dir
	}
	candidate, err := filepath.Abs("../../gemara")
	if err == nil {
		if info, statErr := os.Stat(filepath.Join(candidate, "cue.mod")); statErr == nil && info.IsDir() {
			return candidate
		}
	}
	t.Skip("no Gemara spec checkout available (set GRCLI_GEMARA_SPEC_DIR or check out ../gemara)")
	return ""
}

func requireCue(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("cue"); err != nil {
		t.Skip("cue binary not on PATH")
	}
}

func TestValidate_FlagValidation(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantSub string
	}{
		{
			name:    "no-files",
			args:    []string{"validate", "--spec", "/tmp/anything"},
			wantSub: "at least one --file is required",
		},
		{
			name:    "no-spec-no-env",
			args:    []string{"validate", "-f", "/tmp/x.yaml"},
			wantSub: "spec directory is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolatedWorkdir(t)
			out, err := runRootExpectErr(t, tc.args...)
			require.Error(t, err, "expected error, got: %s", out)
			require.Contains(t, err.Error(), tc.wantSub)
		})
	}
}

func TestValidate_SpecPathNotADirectory(t *testing.T) {
	workdir := isolatedWorkdir(t)
	notADir := filepath.Join(workdir, "not-a-dir")
	require.NoError(t, os.WriteFile(notADir, []byte("hi"), 0o600))

	_, err := runRootExpectErr(t, "validate", "-f", "anything.yaml", "--spec", notADir)
	require.Error(t, err)
	require.Contains(t, err.Error(), "is not a directory")
}

func TestValidate_HappyPath_Policy(t *testing.T) {
	requireCue(t)
	specDir := findSpecDir(t)

	workdir := isolatedWorkdir(t)
	input := writeTempFile(t, workdir, "policy.yaml", minimalValidPolicy)

	out := runRoot(t, "validate", "-f", input, "--spec", specDir)
	require.Contains(t, out, "OK")
	require.Contains(t, out, "#Policy")
}

func TestValidate_DetectsSchemaViolation(t *testing.T) {
	requireCue(t)
	specDir := findSpecDir(t)

	workdir := isolatedWorkdir(t)
	bad := minimalValidPolicy + "not-a-real-field: oops\n"
	input := writeTempFile(t, workdir, "bad.yaml", bad)

	out, err := runRootExpectErr(t, "validate", "-f", input, "--spec", specDir)
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed validation")
	require.Contains(t, out, "FAIL")
	require.Contains(t, out, "not-a-real-field")
}

func TestValidate_SpecFromEnv(t *testing.T) {
	requireCue(t)
	specDir := findSpecDir(t)

	workdir := isolatedWorkdir(t)
	t.Setenv("GRCLI_GEMARA_SPEC_DIR", specDir)
	input := writeTempFile(t, workdir, "policy.yaml", minimalValidPolicy)

	out := runRoot(t, "validate", "-f", input)
	require.Contains(t, out, "OK")
}

func TestValidate_MissingMetadataType(t *testing.T) {
	requireCue(t)
	specDir := findSpecDir(t)

	workdir := isolatedWorkdir(t)
	input := writeTempFile(t, workdir, "noo.yaml", "metadata: {}\n")

	out, err := runRootExpectErr(t, "validate", "-f", input, "--spec", specDir)
	require.Error(t, err)
	require.Contains(t, out, "metadata.type is missing")
}
