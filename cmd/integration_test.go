// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

	publishOut := runRoot(t, "publish", "--dry-run", "-f", input, "--output", layout, "--license", "Apache-2.0")
	require.Contains(t, publishOut, "dry-run: wrote bundle to oci:"+layout+":1.0.0")
	require.Contains(t, publishOut, "artifact: Policy/roundtrip-policy")

	unpackOut := runRoot(t, "unpack", "--source", layout, "--version", "1.0.0", "--output", unpacked)
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

// TestPublish_License_Valid_StampsCanonicalAnnotation covers ADR-0036
// decisions 1, 2, and 4 on the happy path: a valid --license (given in
// non-canonical casing) is canonicalized and stamped as the standard OCI
// manifest annotation org.opencontainers.image.licenses. --dry-run keeps it
// off the network; we read the annotation back off the local OCI manifest.
func TestPublish_License_Valid_StampsCanonicalAnnotation(t *testing.T) {
	workdir := isolatedWorkdir(t)
	input := writeTempFile(t, workdir, "policy.yaml", policyYAML)
	layout := filepath.Join(workdir, "layout")

	// Non-canonical input "apache-2.0" must come back canonicalized to
	// "Apache-2.0" — proving the stamped value is spdx.Canonicalize's output,
	// not the raw flag.
	runRoot(t, "publish", "--dry-run", "-f", input, "--output", layout, "--license", "apache-2.0")

	ann := readOCIManifestAnnotations(t, layout)
	require.Equal(t, "Apache-2.0", ann["org.opencontainers.image.licenses"],
		"the canonical SPDX expression must be stamped as the standard OCI license annotation")
}

// TestPublish_License_CompoundExpression confirms a compound SPDX expression
// round-trips canonicalized (operator casing normalized) into the annotation.
func TestPublish_License_CompoundExpression(t *testing.T) {
	workdir := isolatedWorkdir(t)
	input := writeTempFile(t, workdir, "policy.yaml", policyYAML)
	layout := filepath.Join(workdir, "layout")

	// SPDX operators are case-sensitive uppercase; the leaf ids are not, so
	// "mit" canonicalizes to "MIT" while "OR" must already be uppercase.
	runRoot(t, "publish", "--dry-run", "-f", input, "--output", layout, "--license", "mit OR apache-2.0")

	ann := readOCIManifestAnnotations(t, layout)
	require.Equal(t, "MIT OR Apache-2.0", ann["org.opencontainers.image.licenses"])
}

// TestPublish_License_Invalid_RejectedBeforePush covers ADR-0036 decision 4's
// strict gate: a malformed/unknown --license aborts the publish and writes NO
// OCI output, even under --dry-run (the strict check runs before pack).
func TestPublish_License_Invalid_RejectedBeforePush(t *testing.T) {
	cases := []struct {
		name    string
		license string
		wantSub string
	}{
		{
			name:    "unknown-id",
			license: "Apache-9.9", // well-formed grammar, not a real SPDX id
			wantSub: "unknown SPDX id",
		},
		{
			name:    "malformed-grammar",
			license: "MIT OR OR Apache-2.0", // dangling operator
			wantSub: "malformed SPDX expression",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workdir := isolatedWorkdir(t)
			input := writeTempFile(t, workdir, "policy.yaml", policyYAML)
			layout := filepath.Join(workdir, "layout")

			_, err := runRootExpectErr(t, "publish", "--dry-run", "-f", input, "--output", layout, "--license", tc.license)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantSub)
			require.Contains(t, err.Error(), "invalid --license")

			// No OCI bytes may have been written: the layout dir must not exist.
			_, statErr := os.Stat(layout)
			require.True(t, os.IsNotExist(statErr),
				"an invalid --license must abort before any OCI output is written")
		})
	}
}

// TestPublish_License_Omitted_RejectedBeforePush covers ADR-0037 decision 1:
// --license is now REQUIRED. Omitting it aborts the publish — before any pack
// or push, even under --dry-run — with the distinct "is required" error (NOT
// the "invalid --license" malformed-value message) and writes NO OCI output.
func TestPublish_License_Omitted_RejectedBeforePush(t *testing.T) {
	workdir := isolatedWorkdir(t)
	input := writeTempFile(t, workdir, "policy.yaml", policyYAML)
	layout := filepath.Join(workdir, "layout")

	_, err := runRootExpectErr(t, "publish", "--dry-run", "-f", input, "--output", layout)
	require.Error(t, err)
	require.Contains(t, err.Error(), "a publication license is required",
		"a missing --license must produce the distinct required-license error")
	require.NotContains(t, err.Error(), "invalid --license",
		"a missing flag and a malformed value must read differently")

	// No OCI bytes may have been written: the layout dir must not exist.
	_, statErr := os.Stat(layout)
	require.True(t, os.IsNotExist(statErr),
		"a missing --license must abort before any OCI output is written")
}

// TestPublish_License_Whitespace_RejectedBeforePush confirms a
// whitespace-only --license is treated as absent (the required-license
// error), not as a malformed value.
func TestPublish_License_Whitespace_RejectedBeforePush(t *testing.T) {
	workdir := isolatedWorkdir(t)
	input := writeTempFile(t, workdir, "policy.yaml", policyYAML)
	layout := filepath.Join(workdir, "layout")

	_, err := runRootExpectErr(t, "publish", "--dry-run", "-f", input, "--output", layout, "--license", "   ")
	require.Error(t, err)
	require.Contains(t, err.Error(), "a publication license is required")

	_, statErr := os.Stat(layout)
	require.True(t, os.IsNotExist(statErr))
}

// TestPublish_License_LicenseRef_Accepted confirms a LicenseRef- token (the
// custom/proprietary escape hatch named in the required-license error and
// ADR-0037) is accepted and stamped verbatim.
func TestPublish_License_LicenseRef_Accepted(t *testing.T) {
	workdir := isolatedWorkdir(t)
	input := writeTempFile(t, workdir, "policy.yaml", policyYAML)
	layout := filepath.Join(workdir, "layout")

	runRoot(t, "publish", "--dry-run", "-f", input, "--output", layout, "--license", "LicenseRef-Acme-Proprietary")

	ann := readOCIManifestAnnotations(t, layout)
	require.Equal(t, "LicenseRef-Acme-Proprietary", ann["org.opencontainers.image.licenses"],
		"a LicenseRef- token must be accepted and stamped as the OCI license annotation")
}

func TestPublishUnpackRoundtrip_MergedControlCatalog(t *testing.T) {
	workdir := isolatedWorkdir(t)
	aPath := writeTempFile(t, workdir, "a.yaml", controlsPartA)
	bPath := writeTempFile(t, workdir, "b.yaml", controlsPartB)
	layout := filepath.Join(workdir, "layout")
	unpacked := filepath.Join(workdir, "unpacked")

	runRoot(t, "publish", "--dry-run", "-f", aPath, "-f", bPath, "--output", layout, "--license", "Apache-2.0")
	runRoot(t, "unpack", "--source", layout, "--version", "2.0.0", "--output", unpacked)

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

func TestUnpack_FlagValidation(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantSub string
	}{
		{
			// Pass --url="" to defeat the bake-in default — otherwise
			// the default would be a valid source and this test's
			// premise ("no source set") wouldn't be reachable. The
			// branch still exists for users who explicitly opt out of
			// the default.
			name:    "no-source-or-url",
			args:    []string{"unpack", "--version", "1.0.0", "--url", ""},
			wantSub: "either --source or --url is required",
		},
		{
			name:    "both-source-and-url",
			args:    []string{"unpack", "--version", "1.0.0", "--source", "/tmp/x", "--url", "https://hub.example"},
			wantSub: "mutually exclusive",
		},
		{
			// A bogus --url is fine: the --repository check runs before any
			// hub round-trip, so this never dials the host.
			name:    "url-without-repository",
			args:    []string{"unpack", "--version", "1.0.0", "--url", "https://hub.example"},
			wantSub: "--repository is required when --url is set",
		},
		{
			name:    "missing-version",
			args:    []string{"unpack", "--source", "/tmp/x"},
			wantSub: "--version is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolatedWorkdir(t)
			out, err := runRootExpectErr(t, tc.args...)
			require.Error(t, err, "expected error, got output: %s", out)
			require.Contains(t, err.Error(), tc.wantSub)
		})
	}
}

// TestPublishPositionalFile_Roundtrip mirrors the single-policy
// roundtrip but passes the input file as a positional argument instead
// of via -f. Same assertions as the -f path — the goal is to prove the
// positional surface is wired all the way through to the bundle output,
// not to re-test the bundle internals.
func TestPublishPositionalFile_Roundtrip(t *testing.T) {
	workdir := isolatedWorkdir(t)
	input := writeTempFile(t, workdir, "policy.yaml", policyYAML)
	layout := filepath.Join(workdir, "layout")

	publishOut := runRoot(t, "publish", "--dry-run", "--output", layout, input, "--license", "Apache-2.0")
	require.Contains(t, publishOut, "dry-run: wrote bundle to oci:"+layout+":1.0.0")
	require.Contains(t, publishOut, "artifact: Policy/roundtrip-policy")
}

func TestPublish_MixingFlagAndPositional_Errors(t *testing.T) {
	workdir := isolatedWorkdir(t)
	input := writeTempFile(t, workdir, "policy.yaml", policyYAML)
	layout := filepath.Join(workdir, "layout")

	_, err := runRootExpectErr(t, "publish", "--dry-run", "--output", layout, "-f", input, input)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not both")
}

// TestDefaultURL_AppliesWhenUnset locks in the user-visible behavior
// that grcli ships with hub.grc.store as the default --url target.
// Driven through the publish command's flag definition rather than
// resolveTarget directly so we catch a regression if the default is
// silently dropped from the cobra flag spec.
func TestDefaultURL_AppliesWhenUnset(t *testing.T) {
	root := newRootCmd()
	pub, _, err := root.Find([]string{"publish"})
	require.NoError(t, err)
	urlFlag := pub.Flags().Lookup(flagURL)
	require.NotNil(t, urlFlag, "publish must expose --url")
	require.Equal(t, "https://hub.grc.store", urlFlag.DefValue,
		"the bake-in default for --url must remain hub.grc.store until grcli has a private-hub story")
}

// TestSuppressDefaultURLIfExplicit_SourceAlone covers the helper's
// raison d'être: a user passing only --source should NOT trip the
// "--source is mutually exclusive with --url" branch, because the --url
// they're supposedly conflicting with is just the bake-in default.
func TestSuppressDefaultURLIfExplicit_SourceAlone(t *testing.T) {
	workdir := isolatedWorkdir(t)
	input := writeTempFile(t, workdir, "policy.yaml", policyYAML)
	layout := filepath.Join(workdir, "layout")
	runRoot(t, "publish", "--dry-run", "-f", input, "--output", layout, "--license", "Apache-2.0")

	// unpack --source with no explicit --url must not error with the
	// mutual-exclusion message; the helper suppresses the default --url.
	out := runRoot(t, "unpack", "--source", layout, "--version", "1.0.0", "--output", filepath.Join(workdir, "unpacked"))
	require.Contains(t, out, "unpacked")
}

func TestUnpack_MissingVersion_Errors(t *testing.T) {
	workdir := isolatedWorkdir(t)
	input := writeTempFile(t, workdir, "policy.yaml", policyYAML)
	layout := filepath.Join(workdir, "layout")
	runRoot(t, "publish", "--dry-run", "-f", input, "--output", layout, "--license", "Apache-2.0")

	_, err := runRootExpectErr(t, "unpack", "--source", layout, "--version", "does-not-exist", "--output", filepath.Join(workdir, "unpacked"))
	require.Error(t, err)
}

// isolatedWorkdir chdirs into a fresh temp dir and points HOME +
// XDG_CONFIG_HOME at it so any real ~/.grcli.yaml on the dev machine
// can't influence the test's viper resolution. Also clears the
// GRCLI_URL env var: viper's AutomaticEnv would otherwise pick up a
// dev's exported value and silently override --url in tests that
// exercise the "--url is required" path.
func isolatedWorkdir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("GRCLI_URL", "")
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

// executeRootSplit runs the root command with independent stdout and stderr
// buffers, so a test can assert that content and diagnostics land on the right
// stream (e.g. `cat` must keep stdout pipe-clean). executeRoot merges the two,
// which cannot detect a stream-separation regression.
func executeRootSplit(args ...string) (stdout, stderr string, err error) {
	var out, errBuf bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs(args)
	root.SetContext(context.Background())
	err = root.Execute()
	return out.String(), errBuf.String(), err
}

func readManifest(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var manifest map[string]any
	require.NoError(t, json.Unmarshal(raw, &manifest))
	return manifest
}

// readOCIManifestAnnotations reads the single manifest from an OCI image
// layout directory and returns its manifest-level annotations map. It walks
// index.json -> the manifest blob (addressed by digest), which is where
// go-gemara's bundle.WithAnnotations lands the publication license (ADR-0036),
// as opposed to bundle.json (the config blob) which readManifest covers.
func readOCIManifestAnnotations(t *testing.T, layoutDir string) map[string]any {
	t.Helper()
	index := readManifest(t, filepath.Join(layoutDir, "index.json"))
	manifests, ok := index["manifests"].([]any)
	require.True(t, ok, "index.json has no manifests array")
	require.Len(t, manifests, 1, "expected exactly one manifest in the layout")
	entry, _ := manifests[0].(map[string]any)
	digest, _ := entry["digest"].(string)
	require.NotEmpty(t, digest, "manifest entry has no digest")

	// "sha256:<hex>" -> blobs/sha256/<hex>
	algo, hex, ok := strings.Cut(digest, ":")
	require.True(t, ok, "manifest digest %q is not algo:hex", digest)
	manifest := readManifest(t, filepath.Join(layoutDir, "blobs", algo, hex))

	if ann, ok := manifest["annotations"].(map[string]any); ok {
		return ann
	}
	return map[string]any{}
}
