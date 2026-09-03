// SPDX-License-Identifier: Apache-2.0

package source

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

const policyYAML = `metadata:
  id: my-policy
  type: Policy
  version: 1.0.0
  gemara-version: 0.20.0
  author:
    id: my-team
    type: Human
`

const controlCatalogPartA = `metadata:
  id: my-controls
  type: ControlCatalog
  version: 1.0.0
  gemara-version: 0.20.0
  author:
    id: my-team
    type: Human
controls:
  - id: AC-1
    title: Access Control 1
`

const controlCatalogPartB = `metadata:
  id: my-controls
  type: ControlCatalog
  version: 1.0.0
  gemara-version: 0.20.0
  author:
    id: my-team
    type: Human
controls:
  - id: AC-2
    title: Access Control 2
`

func TestLoad_SingleFile_PassesThrough(t *testing.T) {
	d := t.TempDir()
	p := writeFile(t, d, "policy.yaml", policyYAML)

	out, err := Load(context.Background(), []string{p})
	require.NoError(t, err)
	require.Equal(t, "Policy", out.Type)
	require.Equal(t, "my-policy", out.ID)
	require.Equal(t, "1.0.0", out.Version)
	require.Equal(t, "my-team", out.AuthorID)
	require.Equal(t, "policy.yaml", out.Filename)
	require.Equal(t, policyYAML, string(out.Body))
	require.Contains(t, out.SourceDigests, p)
	require.True(t, strings.HasPrefix(out.SourceDigests[p], "sha256:"))
}

func TestLoad_MultipleControlCatalogs_AreMerged(t *testing.T) {
	d := t.TempDir()
	a := writeFile(t, d, "a.yaml", controlCatalogPartA)
	b := writeFile(t, d, "b.yaml", controlCatalogPartB)

	out, err := Load(context.Background(), []string{a, b})
	require.NoError(t, err)
	require.Equal(t, "ControlCatalog", out.Type)
	require.Equal(t, "my-controls", out.ID)
	require.Equal(t, "control-catalog.yaml", out.Filename)
	// Merged body must contain both controls from the two inputs.
	require.Contains(t, string(out.Body), "AC-1")
	require.Contains(t, string(out.Body), "AC-2")
}

func TestLoad_MismatchedID_Errors(t *testing.T) {
	d := t.TempDir()
	a := writeFile(t, d, "a.yaml", controlCatalogPartA)
	bad := strings.Replace(controlCatalogPartB, "my-controls", "other-id", 1)
	b := writeFile(t, d, "b.yaml", bad)

	_, err := Load(context.Background(), []string{a, b})
	require.Error(t, err)
	require.Contains(t, err.Error(), "must describe one artifact")
}

func TestLoad_MismatchedType_Errors(t *testing.T) {
	d := t.TempDir()
	a := writeFile(t, d, "a.yaml", controlCatalogPartA)
	bad := strings.Replace(controlCatalogPartB, "ControlCatalog", "GuidanceCatalog", 1)
	b := writeFile(t, d, "b.yaml", bad)

	_, err := Load(context.Background(), []string{a, b})
	require.Error(t, err)
	require.Contains(t, err.Error(), "must describe one artifact")
}

func TestLoad_MissingMetadata_Errors(t *testing.T) {
	d := t.TempDir()
	p := writeFile(t, d, "x.yaml", "metadata: {}\n")
	_, err := Load(context.Background(), []string{p})
	require.Error(t, err)
	require.Contains(t, err.Error(), "metadata.type and metadata.id are required")
}

func TestLoad_MultipleNonCatalog_Errors(t *testing.T) {
	d := t.TempDir()
	a := writeFile(t, d, "a.yaml", policyYAML)
	b := writeFile(t, d, "b.yaml", policyYAML)
	_, err := Load(context.Background(), []string{a, b})
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not support multi-file merge")
}

func TestLoad_NoFiles_Errors(t *testing.T) {
	_, err := Load(context.Background(), nil)
	require.Error(t, err)
}
