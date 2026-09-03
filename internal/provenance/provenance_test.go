// SPDX-License-Identifier: Apache-2.0

package provenance

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBuild_BasicShape(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "")
	t.Setenv("USER", "testuser")

	now := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	p := Build(Input{
		ToolVersion:    "1.2.3",
		StartedOn:      now,
		ArtifactType:   "ControlCatalog",
		ArtifactID:     "my-controls",
		ArtifactName:   "control-catalog.yaml",
		ArtifactDigest: "sha256:abc123",
		SourceFiles: map[string]string{
			"a.yaml": "sha256:aaa",
			"b.yaml": "sha256:bbb",
		},
		Registry:   "registry.example",
		Repository: "team/my-controls",
		Tag:        "1.0.0",
	})

	require.Equal(t, BuildType, p.BuildDefinition.BuildType)
	require.Equal(t, now, p.RunDetails.Metadata.StartedOn)
	require.False(t, p.RunDetails.Metadata.FinishedOn.IsZero())
	require.Equal(t, "1.2.3", p.RunDetails.Builder.Version["grcli"])
	require.Contains(t, p.RunDetails.Builder.ID, "local://")
	// ExternalParameters carries the artifact + target coordinates.
	ext := p.BuildDefinition.ExternalParameters
	require.Equal(t, map[string]string{"type": "ControlCatalog", "id": "my-controls"}, ext["artifact"])
	require.Equal(t, map[string]string{"registry": "registry.example", "repository": "team/my-controls", "tag": "1.0.0"}, ext["target"])
	// ResolvedDependencies carries one entry per source file (sorted).
	require.GreaterOrEqual(t, len(p.BuildDefinition.ResolvedDependencies), 2)
	require.Equal(t, "a.yaml", p.BuildDefinition.ResolvedDependencies[0].Name)
	require.Equal(t, "b.yaml", p.BuildDefinition.ResolvedDependencies[1].Name)
	require.Equal(t, "aaa", p.BuildDefinition.ResolvedDependencies[0].Digest["sha256"])
	// Byproducts carries the merged body digest.
	require.Len(t, p.RunDetails.Byproducts, 1)
	require.Equal(t, "abc123", p.RunDetails.Byproducts[0].Digest["sha256"])
}

func TestBuild_GitHubActions_BuilderIDIsRunURL(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("GITHUB_SERVER_URL", "https://github.com")
	t.Setenv("GITHUB_REPOSITORY", "gemaraproj/grcli")
	t.Setenv("GITHUB_RUN_ID", "42")
	t.Setenv("GITHUB_RUN_ATTEMPT", "1")

	p := Build(Input{
		ToolVersion: "1.0.0",
		StartedOn:   time.Now().UTC(),
	})
	require.Equal(t,
		"https://github.com/gemaraproj/grcli/actions/runs/42",
		p.RunDetails.Builder.ID)
	require.Equal(t, "42-1", p.RunDetails.Metadata.InvocationID)
}

func TestBuild_SerializesAsValidJSON(t *testing.T) {
	p := Build(Input{
		ToolVersion: "1.0.0",
		StartedOn:   time.Now().UTC(),
		SourceFiles: map[string]string{"x": "sha256:1"},
	})
	b, err := json.Marshal(p)
	require.NoError(t, err)
	require.Contains(t, string(b), `"buildType"`)
	require.Contains(t, string(b), `"resolvedDependencies"`)
}

func TestStripUserinfo(t *testing.T) {
	cases := map[string]string{
		"https://user:tok3n@github.com/org/repo.git":         "https://github.com/org/repo.git",
		"https://x-access-token:ghs_abc@github.com/org/repo": "https://github.com/org/repo",
		"https://github.com/org/repo.git":                    "https://github.com/org/repo.git",
		"git@github.com:org/repo.git":                        "git@github.com:org/repo.git",
		"ssh://git@github.com/org/repo.git":                  "ssh://github.com/org/repo.git",
	}
	for in, want := range cases {
		if got := stripUserinfo(in); got != want {
			t.Errorf("stripUserinfo(%q) = %q, want %q", in, got, want)
		}
	}
}
