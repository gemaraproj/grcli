// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

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

// skipOrFail skips a test whose external prerequisite is missing — except
// in CI, where a silent skip would leave validate's default path untested.
func skipOrFail(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("CI") != "" {
		t.Fatalf("required in CI: "+format, args...)
	}
	t.Skipf(format, args...)
}

// requireCue skips tests that shell out to cue when the binary is absent.
func requireCue(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("cue"); err != nil {
		skipOrFail(t, "cue binary not on PATH")
	}
}

// requireRegistry skips tests that resolve the Gemara module from the CUE
// central registry when it cannot be reached (offline dev, sandboxed CI).
func requireRegistry(t *testing.T) {
	t.Helper()
	requireCue(t)
	conn, err := net.DialTimeout("tcp", "registry.cue.works:443", 3*time.Second)
	if err != nil {
		skipOrFail(t, "CUE central registry unreachable: %v", err)
	}
	_ = conn.Close()
	// isolatedWorkdir points HOME at the test workdir; keep cue's module
	// cache out of it so tests can assert the workdir stays clean.
	cacheDir := t.TempDir()
	t.Setenv("CUE_CACHE_DIR", cacheDir)
	// cue extracts modules read-only (like Go's module cache), which
	// would fail TempDir's cleanup; make the tree writable again first.
	t.Cleanup(func() {
		_ = filepath.WalkDir(cacheDir, func(path string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				_ = os.Chmod(path, 0o700)
			}
			return nil
		})
	})
}

// writeLocalSpec lays down a throwaway CUE module standing in for a Gemara
// checkout, so the --spec path is exercised without the network or a clone.
func writeLocalSpec(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "cue.mod"), 0o755))
	writeTempFile(t, filepath.Join(dir, "cue.mod"), "module.cue",
		"module: \"example.com/localspec@v0\"\nlanguage: version: \"v0.9.0\"\n")
	writeTempFile(t, dir, "schema.cue",
		"package localspec\n\n#Policy: {\n\tmetadata: {...}\n\ttitle: string\n\tcontacts: {...}\n}\n")
	return dir
}

func TestValidate_FlagValidation(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantSub string
	}{
		{
			name:    "no-files",
			args:    []string{"validate"},
			wantSub: "at least one --file is required",
		},
		{
			name:    "spec-and-gemara-version",
			args:    []string{"validate", "-f", "/tmp/x.yaml", "--spec", "/tmp/anything", "--gemara-version", "1.6.0"},
			wantSub: "--spec and --gemara-version are mutually exclusive",
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

func TestGemaraVersion(t *testing.T) {
	cases := []struct {
		name     string
		override string
		fromMeta string
		want     string
	}{
		{name: "override-wins", override: "v1.5.0", fromMeta: "1.6.0", want: "v1.5.0"},
		{name: "override-gains-v-prefix", override: "1.5.0", want: "v1.5.0"},
		{name: "metadata-used", fromMeta: "1.6.0", want: "v1.6.0"},
		{name: "metadata-already-prefixed", fromMeta: "v1.6.0", want: "v1.6.0"},
		{name: "prerelease", fromMeta: "1.0.0-rc.2", want: "v1.0.0-rc.2"},
		{name: "fallback-latest", want: "latest"},
		{name: "explicit-latest", override: "latest", fromMeta: "1.6.0", want: "latest"},
		{name: "major-latest", override: "v1.latest", want: "v1.latest"},
		{name: "whitespace-trimmed", fromMeta: " 1.6.0 ", want: "v1.6.0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := gemaraVersion(tc.override, tc.fromMeta)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// A version that is not a plain module version must never reach cue: cue
// reads an argument with an invalid version as a file path or package
// qualifier, which would let an artifact name its own (permissive) schema.
func TestGemaraVersion_RejectsNonVersions(t *testing.T) {
	for _, bad := range []string{
		"/../../../evil.cue",
		"0.20.0:otherpkg",
		"1.0.0@v2",
		"1 .0.0",
		"1.0.0 --help",
		"-x",
		"V1.0.0",
		"Latest",
		"1.10",
		"1",
	} {
		t.Run(bad, func(t *testing.T) {
			_, err := gemaraVersion("", bad)
			require.ErrorContains(t, err, "invalid Gemara version")
			_, err = gemaraVersion(bad, "1.6.0")
			require.ErrorContains(t, err, "invalid Gemara version")
		})
	}
}

func TestReadArtifactMeta_RejectsNonIdentifierType(t *testing.T) {
	for _, bad := range []string{"Policy | _", "Policy\nOK", "#Policy", "1Policy", "a.b"} {
		t.Run(bad, func(t *testing.T) {
			path := writeTempFile(t, t.TempDir(), "a.yaml", "metadata:\n  type: "+strconv.Quote(bad)+"\n")
			_, _, err := readArtifactMeta(path)
			require.ErrorContains(t, err, "metadata.type")
		})
	}
}

func TestValidate_RejectsCraftedGemaraVersion(t *testing.T) {
	requireCue(t)
	workdir := isolatedWorkdir(t)
	writeTempFile(t, workdir, "evil.cue", "package evil\n\n#Policy: {...}\n")
	input := writeTempFile(t, workdir, "bad.yaml",
		"garbage: true\nmetadata:\n  type: Policy\n  gemara-version: \"/../../../evil.cue\"\n")

	out, err := runRootExpectErr(t, "validate", "-f", input)
	require.Error(t, err)
	require.Contains(t, out, "FAIL")
	require.Contains(t, out, "invalid Gemara version")
	require.NotContains(t, out, "OK")
}

func TestValidate_InvalidVersionFlag(t *testing.T) {
	isolatedWorkdir(t)
	_, err := runRootExpectErr(t, "validate", "-f", "x.yaml", "--gemara-version", "../x")
	require.ErrorContains(t, err, "invalid Gemara version")
}

func TestVetArgs(t *testing.T) {
	t.Run("registry", func(t *testing.T) {
		require.Equal(t,
			[]string{"vet", "-d", "#Policy", "github.com/gemaraproj/gemara@v1.6.0", "/abs/policy.yaml"},
			vetArgs("", "v1.6.0", "Policy", "/abs/policy.yaml"))
	})
	t.Run("local-spec", func(t *testing.T) {
		require.Equal(t,
			[]string{"vet", "-d", "#Policy", ".", "/abs/policy.yaml"},
			vetArgs("/some/spec", "v1.6.0", "Policy", "/abs/policy.yaml"))
	})
}

func TestValidate_Registry_HappyPath(t *testing.T) {
	requireRegistry(t)

	workdir := isolatedWorkdir(t)
	input := writeTempFile(t, workdir, "policy.yaml", minimalValidPolicy)

	out := runRoot(t, "validate", "-f", input)
	require.Contains(t, out, "OK")
	require.Contains(t, out, "#Policy")
	require.Contains(t, out, "gemara v0.20.0", "the file's own metadata.gemara-version is the default")

	entries, err := os.ReadDir(workdir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "validate must not write the spec into the workspace")
}

func TestValidate_Registry_DetectsSchemaViolation(t *testing.T) {
	requireRegistry(t)

	workdir := isolatedWorkdir(t)
	bad := minimalValidPolicy + "not-a-real-field: oops\n"
	input := writeTempFile(t, workdir, "bad.yaml", bad)

	out, err := runRootExpectErr(t, "validate", "-f", input)
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed validation")
	require.Contains(t, out, "FAIL")
	require.Contains(t, out, "not-a-real-field")
}

func TestValidate_Registry_VersionFlagOverridesMetadata(t *testing.T) {
	requireRegistry(t)

	workdir := isolatedWorkdir(t)
	input := writeTempFile(t, workdir, "policy.yaml", minimalValidPolicy)

	out := runRoot(t, "validate", "-f", input, "--gemara-version", "1.6.0")
	require.Contains(t, out, "gemara v1.6.0")
}

func TestValidate_Registry_VersionFromEnv(t *testing.T) {
	requireRegistry(t)

	workdir := isolatedWorkdir(t)
	t.Setenv("GRCLI_GEMARA_VERSION", "1.6.0")
	input := writeTempFile(t, workdir, "policy.yaml", minimalValidPolicy)

	out := runRoot(t, "validate", "-f", input)
	require.Contains(t, out, "gemara v1.6.0")
}

// An explicit --gemara-version beats a spec dir that only comes from the
// environment, so a durable local setting cannot block a one-off run.
func TestValidate_VersionFlagBeatsSpecEnv(t *testing.T) {
	requireRegistry(t)

	workdir := isolatedWorkdir(t)
	t.Setenv("GRCLI_GEMARA_SPEC_DIR", writeLocalSpec(t))
	input := writeTempFile(t, workdir, "policy.yaml", minimalValidPolicy)

	out := runRoot(t, "validate", "-f", input, "--gemara-version", "1.6.0")
	require.Contains(t, out, "gemara v1.6.0")
}

// A local spec wins over a version that only comes from the environment.
func TestValidate_LocalSpec_IgnoresVersionEnv(t *testing.T) {
	requireCue(t)
	specDir := writeLocalSpec(t)

	workdir := isolatedWorkdir(t)
	t.Setenv("GRCLI_GEMARA_VERSION", "1.6.0")
	input := writeTempFile(t, workdir, "policy.yaml", minimalValidPolicy)

	out := runRoot(t, "validate", "-f", input, "--spec", specDir)
	require.Contains(t, out, "OK")
	require.NotContains(t, out, "gemara v")
}

func TestValidate_LocalSpec_HappyPath(t *testing.T) {
	requireCue(t)
	specDir := writeLocalSpec(t)

	workdir := isolatedWorkdir(t)
	input := writeTempFile(t, workdir, "policy.yaml", minimalValidPolicy)

	out := runRoot(t, "validate", "-f", input, "--spec", specDir)
	require.Contains(t, out, "OK")
	require.Contains(t, out, "#Policy")
	require.NotContains(t, out, "gemara v", "a local spec has no registry version to report")
}

func TestValidate_LocalSpec_DetectsSchemaViolation(t *testing.T) {
	requireCue(t)
	specDir := writeLocalSpec(t)

	workdir := isolatedWorkdir(t)
	bad := minimalValidPolicy + "not-a-real-field: oops\n"
	input := writeTempFile(t, workdir, "bad.yaml", bad)

	out, err := runRootExpectErr(t, "validate", "-f", input, "--spec", specDir)
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed validation")
	require.Contains(t, out, "FAIL")
	require.Contains(t, out, "not-a-real-field")
}

func TestValidate_LocalSpec_FromEnv(t *testing.T) {
	requireCue(t)
	specDir := writeLocalSpec(t)

	workdir := isolatedWorkdir(t)
	t.Setenv("GRCLI_GEMARA_SPEC_DIR", specDir)
	input := writeTempFile(t, workdir, "policy.yaml", minimalValidPolicy)

	out := runRoot(t, "validate", "-f", input)
	require.Contains(t, out, "OK")
	require.NotContains(t, out, "gemara v")
}

func TestValidate_MissingMetadataType(t *testing.T) {
	requireCue(t)
	workdir := isolatedWorkdir(t)
	input := writeTempFile(t, workdir, "noo.yaml", "metadata: {}\n")

	out, err := runRootExpectErr(t, "validate", "-f", input)
	require.Error(t, err)
	require.Contains(t, out, "metadata.type is missing")
}
