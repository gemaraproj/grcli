// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"sigs.k8s.io/yaml"
)

const (
	flagSpec    = "spec"
	envSpecPath = "GRCLI_GEMARA_SPEC_DIR"
)

func newValidateCmd(v *viper.Viper) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate Gemara YAML files against the spec via cue vet",
		Long: `Reads each --file input, picks its #ArtifactType from metadata.type,
and runs 'cue vet -d \"#<Type>\" <spec-dir> <file>' to validate the file
against the Gemara CUE schemas.

The spec directory must be a local checkout of the Gemara CUE module
(https://github.com/gemaraproj/gemara). Provide it via --spec or the
GRCLI_GEMARA_SPEC_DIR environment variable. For reproducible validation,
check out the tag matching your artifact's metadata.gemara-version.

Requires the 'cue' binary on PATH (see https://cuelang.org).

Example:
  git clone --branch v1.0.0 https://github.com/gemaraproj/gemara /tmp/gemara
  grcli validate -f controls.yaml --spec /tmp/gemara`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runValidate(cmd, v)
		},
	}

	flags := cmd.Flags()
	flags.StringSliceP(flagFile, "f", nil, "input file(s) to validate (repeatable; comma-separated also accepted)")
	flags.String(flagSpec, "", "path to a Gemara CUE module checkout (or set "+envSpecPath+")")

	return cmd
}

func runValidate(cmd *cobra.Command, v *viper.Viper) error {
	if err := v.BindPFlags(cmd.Flags()); err != nil {
		return fmt.Errorf("binding flags: %w", err)
	}
	ctx := cmd.Context()

	files := expandCommas(v.GetStringSlice(flagFile))
	if len(files) == 0 {
		return errors.New("at least one --file is required")
	}

	specDir, err := resolveSpecDir(v)
	if err != nil {
		return err
	}

	if _, err := exec.LookPath("cue"); err != nil {
		return errors.New("cue binary not found on PATH — install from https://cuelang.org")
	}

	out := cmd.OutOrStdout()
	var failed []string
	for _, file := range files {
		artifactType, peekErr := readArtifactType(file)
		if peekErr != nil {
			fmt.Fprintf(out, "FAIL %s: %v\n", file, peekErr)
			failed = append(failed, file)
			continue
		}
		if validateErr := vetFile(ctx, specDir, artifactType, file, out); validateErr != nil {
			fmt.Fprintf(out, "FAIL %s (#%s): %v\n", file, artifactType, validateErr)
			failed = append(failed, file)
			continue
		}
		fmt.Fprintf(out, "OK   %s (#%s)\n", file, artifactType)
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d of %d file(s) failed validation", len(failed), len(files))
	}
	return nil
}

// resolveSpecDir returns the absolute path to the Gemara CUE module
// checkout. Precedence: --spec flag → GRCLI_GEMARA_SPEC_DIR env. Returns
// a typed error if neither is set or the path is not a directory.
func resolveSpecDir(v *viper.Viper) (string, error) {
	dir := v.GetString(flagSpec)
	if dir == "" {
		dir = os.Getenv(envSpecPath)
	}
	if dir == "" {
		return "", fmt.Errorf("spec directory is required: pass --spec or set %s", envSpecPath)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolving spec path: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("spec dir %s: %w", abs, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("spec path %s is not a directory", abs)
	}
	return abs, nil
}

// readArtifactType peeks at metadata.type without loading the whole
// artifact. Mirrors source.peekedMetadata's narrow approach.
func readArtifactType(path string) (string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var meta struct {
		Metadata struct {
			Type string `json:"type"`
		} `json:"metadata"`
	}
	if err := yaml.Unmarshal(body, &meta); err != nil {
		return "", fmt.Errorf("parsing %s: %w", path, err)
	}
	if meta.Metadata.Type == "" {
		return "", errors.New("metadata.type is missing or empty")
	}
	return meta.Metadata.Type, nil
}

// vetFile invokes 'cue vet -d "#<Type>" . <file>' with the working
// directory set to specDir. cue rejects absolute paths as package
// arguments, so we cd-and-use-"." rather than passing the spec path
// inline. The input file path is resolved to absolute first so the cd
// doesn't change which file is being vetted. Any output from cue is
// forwarded to out so users see schema violations inline.
func vetFile(ctx context.Context, specDir, artifactType, file string, out io.Writer) error {
	absFile, err := filepath.Abs(file)
	if err != nil {
		return fmt.Errorf("resolving file path: %w", err)
	}
	args := []string{"vet", "-d", "#" + artifactType, ".", absFile}
	cmd := exec.CommandContext(ctx, "cue", args...)
	cmd.Dir = specDir
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("cue vet exited with: %w", err)
	}
	return nil
}
