// SPDX-License-Identifier: LicenseRef-Revanite-Proprietary

package cmd

import (
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"

	"github.com/revanite-io/grcli/internal/source"
)

func TestResolveTarget(t *testing.T) {
	loadedFull := &source.Loaded{
		Type:     "Policy",
		ID:       "my-policy",
		Version:  "1.2.3",
		AuthorID: "my-team",
	}
	loadedNoMetadata := &source.Loaded{
		Type: "Policy",
		ID:   "my-policy",
	}

	tests := []struct {
		name       string
		flags      map[string]any
		loaded     *source.Loaded
		wantErrSub string
		wantTarget publishTarget
	}{
		{
			name: "all-defaults-from-metadata",
			flags: map[string]any{
				flagRegistry: "registry.example",
			},
			loaded: loadedFull,
			wantTarget: publishTarget{
				registryHost: "registry.example",
				repository:   "my-team/my-policy",
				tag:          "1.2.3",
				output:       "grcli-out",
			},
		},
		{
			name: "flag-tag-overrides-metadata-version",
			flags: map[string]any{
				flagRegistry: "registry.example",
				flagTag:      "override",
			},
			loaded: loadedFull,
			wantTarget: publishTarget{
				registryHost: "registry.example",
				repository:   "my-team/my-policy",
				tag:          "override",
				output:       "grcli-out",
			},
		},
		{
			name: "flag-repository-overrides-default",
			flags: map[string]any{
				flagRegistry:   "registry.example",
				flagRepository: "custom/repo",
			},
			loaded: loadedFull,
			wantTarget: publishTarget{
				registryHost: "registry.example",
				repository:   "custom/repo",
				tag:          "1.2.3",
				output:       "grcli-out",
			},
		},
		{
			name: "dry-run-does-not-require-registry",
			flags: map[string]any{
				flagDryRun: true,
				flagOutput: "/tmp/out",
			},
			loaded: loadedFull,
			wantTarget: publishTarget{
				registryHost: "",
				repository:   "my-team/my-policy",
				tag:          "1.2.3",
				dryRun:       true,
				output:       "/tmp/out",
			},
		},
		{
			name:       "missing-registry-when-not-dry-run",
			flags:      map[string]any{},
			loaded:     loadedFull,
			wantErrSub: "--registry is required",
		},
		{
			name: "missing-tag",
			flags: map[string]any{
				flagRegistry: "registry.example",
			},
			loaded:     loadedNoMetadata,
			wantErrSub: "could not determine tag",
		},
		{
			name: "missing-repository",
			flags: map[string]any{
				flagRegistry: "registry.example",
				flagTag:      "1.0.0",
			},
			loaded: &source.Loaded{
				Type: "Policy",
				// no ID, no AuthorID — defaultRepository returns ""
			},
			wantErrSub: "could not determine --repository",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := viper.New()
			for k, val := range tc.flags {
				v.Set(k, val)
			}
			// Output default mirrors the flag default; resolveTarget reads
			// it via viper, so set it unless the test overrode it.
			if _, ok := tc.flags[flagOutput]; !ok {
				v.SetDefault(flagOutput, "grcli-out")
			}

			got, err := resolveTarget(v, tc.loaded)
			if tc.wantErrSub != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErrSub)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantTarget, got)
		})
	}
}
