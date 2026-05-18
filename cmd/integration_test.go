// SPDX-License-Identifier: LicenseRef-Revanite-Proprietary

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPublishUnpackRoundtrip exercises the full publish → unpack cycle
// via the cobra commands. It writes input YAML(s) to a temp dir, packs
// them into a local OCI layout with `publish --dry-run`, then unpacks
// that layout with `unpack` and verifies the recovered files and the
// embedded bundle manifest.

const policyYAML = `metadata:
  id: roundtrip-policy
  type: Policy
  version: 1.0.0
  gemara-version: 0.20.0
  author:
    id: test-team
    type: Human
`

const controlsPartA = `metadata:
  id: roundtrip-controls
  type: ControlCatalog
  version: 2.0.0
  gemara-version: 0.20.0
  author:
    id: test-team
    type: Human
controls:
  - id: AC-1
    title: Access Control 1
`

const controlsPartB = `metadata:
  id: roundtrip-controls
  type: ControlCatalog
  version: 2.0.0
  gemara-version: 0.20.0
  author:
    id: test-team
    type: Human
controls:
  - id: AC-2
    title: Access Control 2
`

func TestPublishUnpackRoundtrip_SinglePolicy(t *testing.T) {
	workdir := isolatedWorkdir(t)
	input := writeTempFile(t, workdir, "policy.yaml", policyYAML)
	layout := filepath.Join(workdir, "layout")
	unpacked := filepath.Join(workdir, "unpacked")

	publishOut := runRoot(t, "publish", "--dry-run", "-f", input, "--output", layout)
	require.Contains(t, publishOut, "dry-run: wrote bundle to oci:"+layout+":1.0.0")
	require.Contains(t, publishOut, "artifact: Policy/roundtrip-policy")

	unpackOut := runRoot(t, "unpack", "--source", layout, "--tag", "1.0.0", "--output", unpacked)
	require.Contains(t, unpackOut, "unpacked "+layout+":1.0.0")
	require.Contains(t, unpackOut, "policy.yaml")
	require.Contains(t, unpackOut, "bundle.json")

	got, err := os.ReadFile(filepath.Join(unpacked, "policy.yaml"))
	require.NoError(t, err)
	require.Equal(t, policyYAML, string(got), "policy.yaml should round-trip byte-for-byte")

	manifest := readManifest(t, filepath.Join(unpacked, "bundle.json"))
	require.Equal(t, "0.20.0", manifest["gemara-version"])
	artifacts, ok := manifest["artifacts"].([]any)
	require.True(t, ok, "manifest has no artifacts array")
	require.Len(t, artifacts, 1)
	first, _ := artifacts[0].(map[string]any)
	require.Equal(t, "Policy", first["type"])
	require.Equal(t, "roundtrip-policy", first["id"])
	require.Equal(t, "policy.yaml", first["name"])

	metadata, ok := manifest["metadata"].(map[string]any)
	require.True(t, ok, "manifest has no metadata field")
	provenance, ok := metadata["provenance"].(map[string]any)
	require.True(t, ok, "manifest metadata.provenance is missing")
	require.Contains(t, provenance, "buildDefinition")
	require.Contains(t, provenance, "runDetails")
}

func TestPublishUnpackRoundtrip_MergedControlCatalog(t *testing.T) {
	workdir := isolatedWorkdir(t)
	aPath := writeTempFile(t, workdir, "a.yaml", controlsPartA)
	bPath := writeTempFile(t, workdir, "b.yaml", controlsPartB)
	layout := filepath.Join(workdir, "layout")
	unpacked := filepath.Join(workdir, "unpacked")

	runRoot(t, "publish", "--dry-run", "-f", aPath, "-f", bPath, "--output", layout)
	runRoot(t, "unpack", "--source", layout, "--tag", "2.0.0", "--output", unpacked)

	// Two source files get merged into a single control-catalog.yaml
	// inside the bundle. The unpacked file should contain controls from
	// both inputs.
	merged, err := os.ReadFile(filepath.Join(unpacked, "control-catalog.yaml"))
	require.NoError(t, err)
	body := string(merged)
	require.Contains(t, body, "AC-1")
	require.Contains(t, body, "AC-2")

	manifest := readManifest(t, filepath.Join(unpacked, "bundle.json"))
	artifacts, _ := manifest["artifacts"].([]any)
	require.Len(t, artifacts, 1)
	first, _ := artifacts[0].(map[string]any)
	require.Equal(t, "ControlCatalog", first["type"])
	require.Equal(t, "roundtrip-controls", first["id"])
	require.Equal(t, "control-catalog.yaml", first["name"])
}

func TestUnpack_MissingTag_Errors(t *testing.T) {
	workdir := isolatedWorkdir(t)
	input := writeTempFile(t, workdir, "policy.yaml", policyYAML)
	layout := filepath.Join(workdir, "layout")
	runRoot(t, "publish", "--dry-run", "-f", input, "--output", layout)

	_, err := runRootExpectErr(t, "unpack", "--source", layout, "--tag", "does-not-exist", "--output", filepath.Join(workdir, "unpacked"))
	require.Error(t, err)
}

// isolatedWorkdir chdirs into a fresh temp dir and points HOME +
// XDG_CONFIG_HOME at it so any real ~/.grcli.yaml on the dev machine
// can't influence the test's viper resolution.
func isolatedWorkdir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	return dir
}

func writeTempFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

// runRoot builds a fresh root command (and viper instance) and runs it
// with the given args, asserting success and returning captured output.
func runRoot(t *testing.T, args ...string) string {
	t.Helper()
	out, err := executeRoot(args)
	require.NoError(t, err, "command %v failed: %s", args, out)
	return out
}

func runRootExpectErr(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return executeRoot(args)
}

func executeRoot(args []string) (string, error) {
	var buf bytes.Buffer
	root := newRootCmd()
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(args)
	root.SetContext(context.Background())
	err := root.Execute()
	return buf.String(), err
}

func readManifest(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var manifest map[string]any
	require.NoError(t, json.Unmarshal(raw, &manifest))
	return manifest
}
