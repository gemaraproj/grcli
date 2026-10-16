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
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"sigs.k8s.io/yaml"
)

const (
	flagSpec          = "spec"
	flagGemaraVersion = "gemara-version"
	envSpecPath       = "GRCLI_GEMARA_SPEC_DIR"

	// gemaraModule is the Gemara CUE module path on the CUE central
	// registry. cue resolves it into its own module cache, so validating
	// leaves nothing behind in the working directory.
	gemaraModule = "github.com/gemaraproj/gemara"

	// latestVersion is cue's query for the newest published version.
	latestVersion = "latest"
)

func newValidateCmd(v *viper.Viper) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate Gemara YAML files against the spec via cue vet",
		Long: `Reads each --file input, picks its #ArtifactType from metadata.type,
and runs 'cue vet -d "#<Type>" ` + gemaraModule + `@<version> <file>' to
validate the file against the Gemara CUE schemas published on the CUE
central registry. No checkout of the spec is needed; cue keeps the module
in its own cache.

Each file is validated against the spec version named by its own
metadata.gemara-version, falling back to the latest published version when
the field is absent. Pass --gemara-version (or set GRCLI_GEMARA_VERSION) to
validate every file against one version instead, e.g. 1.6.0 or latest.

To validate against a local checkout of the Gemara CUE module instead —
offline, or to try unreleased schema changes — pass --spec or set
` + envSpecPath + `. The two flags cannot be combined; a flag
beats the other setting's environment value.

Requires the 'cue' binary on PATH (see https://cuelang.org). A private or
mirrored registry can be selected with cue's own CUE_REGISTRY variable.

Examples:
  grcli validate -f controls.yaml
  grcli validate -f controls.yaml --gemara-version 1.6.0
  grcli validate -f controls.yaml --spec ../gemara`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runValidate(cmd, v)
		},
	}

	flags := cmd.Flags()
	flags.StringSliceP(flagFile, "f", nil, "input file(s) to validate (repeatable; comma-separated also accepted)")
	flags.String(flagGemaraVersion, "", "Gemara spec version to validate against, e.g. 1.6.0 or latest (default: each file's metadata.gemara-version)")
	flags.String(flagSpec, "", "path to a local Gemara CUE module checkout, used instead of the registry (or set "+envSpecPath+")")

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

	// A local checkout fixes the version, so asking for both is a mistake.
	// Only explicit flags conflict: a flag beats the other setting's
	// environment/config value, and with neither flag the spec dir wins.
	versionFlagSet := cmd.Flags().Changed(flagGemaraVersion)
	if versionFlagSet && cmd.Flags().Changed(flagSpec) {
		return fmt.Errorf("--%s and --%s are mutually exclusive: a local spec checkout fixes the version", flagSpec, flagGemaraVersion)
	}
	var specDir, versionOverride string
	if !versionFlagSet {
		dir, err := resolveSpecDir(v)
		if err != nil {
			return err
		}
		specDir = dir
	}
	if specDir == "" {
		versionOverride = v.GetString(flagGemaraVersion)
		if _, err := gemaraVersion(versionOverride, ""); err != nil {
			return fmt.Errorf("--%s: %w", flagGemaraVersion, err)
		}
	}

	if _, err := exec.LookPath("cue"); err != nil {
		return errors.New("cue binary not found on PATH — install from https://cuelang.org")
	}

	out := cmd.OutOrStdout()
	var failed []string
	for _, file := range files {
		body, meta, peekErr := readArtifactMeta(file)
		if peekErr != nil {
			fmt.Fprintf(out, "FAIL %s: %v\n", file, peekErr)
			failed = append(failed, file)
			continue
		}
		warnReferences(out, "WARN "+file+": ", body)
		label := "#" + meta.Type
		version := ""
		if specDir == "" {
			var versionErr error
			if version, versionErr = gemaraVersion(versionOverride, meta.GemaraVersion); versionErr != nil {
				fmt.Fprintf(out, "FAIL %s (%s): metadata.gemara-version: %v\n", file, label, versionErr)
				failed = append(failed, file)
				continue
			}
			label += ", gemara " + version
		}
		if validateErr := vetFile(ctx, specDir, version, meta.Type, file, out); validateErr != nil {
			fmt.Fprintf(out, "FAIL %s (%s): %v\n", file, label, validateErr)
			failed = append(failed, file)
			continue
		}
		fmt.Fprintf(out, "OK   %s (%s)\n", file, label)
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d of %d file(s) failed validation", len(failed), len(files))
	}
	return nil
}

// resolveSpecDir returns the absolute path to a local Gemara CUE module
// checkout, or "" when none was requested and the registry should be used.
// Precedence: --spec flag → GRCLI_GEMARA_SPEC_DIR env. Returns an error if
// the path is set but is not a directory.
func resolveSpecDir(v *viper.Viper) (string, error) {
	dir := v.GetString(flagSpec)
	if dir == "" {
		dir = os.Getenv(envSpecPath)
	}
	if dir == "" {
		return "", nil
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

// gemaraVersion picks the registry version to validate against.
// Precedence: explicit override (--gemara-version / GRCLI_GEMARA_VERSION) →
// the artifact's metadata.gemara-version → latest. Plain versions gain the
// "v" prefix cue requires; "latest" and "vN.latest" queries pass through.
//
// Anything else is refused. The value comes from the file under validation
// and lands in a cue argument, and cue reads an argument whose version it
// cannot parse as a file path or package qualifier — which would let an
// artifact point validation at a schema of its own choosing.
func gemaraVersion(override, fromMeta string) (string, error) {
	version := strings.TrimSpace(override)
	if version == "" {
		version = strings.TrimSpace(fromMeta)
	}
	switch {
	case version == "":
		return latestVersion, nil
	case version == latestVersion, majorLatestRe.MatchString(version):
		return version, nil
	case !semverRe.MatchString(version):
		return "", fmt.Errorf("invalid Gemara version %q: want a full version like 1.6.0 (quote it in YAML), or %s", version, latestVersion)
	}
	return "v" + strings.TrimPrefix(version, "v"), nil
}

var (
	semverRe      = regexp.MustCompile(`^v?\d+\.\d+\.\d+(-[0-9A-Za-z]+(\.[0-9A-Za-z]+)*)?$`)
	majorLatestRe = regexp.MustCompile(`^v\d+\.latest$`)
	// artifactTypeRe keeps metadata.type to a bare CUE identifier: it is
	// spliced into the 'cue vet -d' expression, where "Policy | _" would
	// otherwise accept anything.
	artifactTypeRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// artifactMeta is the slice of metadata validate needs to pick a schema.
type artifactMeta struct {
	Type          string `json:"type"`
	GemaraVersion string `json:"gemara-version"`
}

// readArtifactMeta peeks at metadata.type and metadata.gemara-version
// without loading the whole artifact. Mirrors source.peekedMetadata's
// narrow approach. The raw body is returned too, so callers can lint it
// without a second read.
func readArtifactMeta(path string) ([]byte, artifactMeta, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, artifactMeta{}, err
	}
	var meta struct {
		Metadata artifactMeta `json:"metadata"`
	}
	if err := yaml.Unmarshal(body, &meta); err != nil {
		return nil, artifactMeta{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	if meta.Metadata.Type == "" {
		return nil, artifactMeta{}, errors.New("metadata.type is missing or empty")
	}
	if !artifactTypeRe.MatchString(meta.Metadata.Type) {
		return nil, artifactMeta{}, fmt.Errorf("metadata.type %q is not a valid Gemara type name", meta.Metadata.Type)
	}
	return body, meta.Metadata, nil
}

// vetArgs builds the 'cue vet' argument list. With no specDir the schema
// is the registry module at version; with one it is ".", because cue
// rejects absolute paths as package arguments and vetFile runs cue from
// inside the checkout instead.
func vetArgs(specDir, version, artifactType, absFile string) []string {
	schema := gemaraModule + "@" + version
	if specDir != "" {
		schema = "."
	}
	return []string{"vet", "-d", "#" + artifactType, schema, absFile}
}

// vetFile invokes 'cue vet' for one file. When specDir is set, cue runs
// with it as the working directory (see vetArgs); the input file path is
// resolved to absolute first so the cd doesn't change which file is being
// vetted. Any output from cue is forwarded to out so users see schema
// violations inline.
func vetFile(ctx context.Context, specDir, version, artifactType, file string, out io.Writer) error {
	absFile, err := filepath.Abs(file)
	if err != nil {
		return fmt.Errorf("resolving file path: %w", err)
	}
	cmd := exec.CommandContext(ctx, "cue", vetArgs(specDir, version, artifactType, absFile)...)
	cmd.Dir = specDir
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("cue vet exited with: %w", err)
	}
	return nil
}
