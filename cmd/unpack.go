// SPDX-License-Identifier: LicenseRef-Revanite-Proprietary

package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gemaraproj/go-gemara/bundle"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/revanite-io/grcli/internal/registry"
)

const flagSource = "source"

func newUnpackCmd(v *viper.Viper) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "unpack",
		Short: "Extract a Gemara bundle from a local OCI layout or remote registry",
		Long: `Reads a Gemara bundle and writes its artifact files to a directory.
The bundle manifest, including any SLSA-shaped provenance record, is
written alongside as bundle.json.

The source can be a local OCI image layout (--source, the shape produced
by 'grcli publish --dry-run') or a remote registry (--registry plus
--repository). Exactly one of --source / --registry must be set.

Registry auth flows through the same Docker credential chain and
GRCLI_REGISTRY_USERNAME / GRCLI_REGISTRY_PASSWORD / GRCLI_REGISTRY_TOKEN
overrides as 'grcli publish'.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUnpack(cmd, v)
		},
	}

	flags := cmd.Flags()
	flags.String(flagSource, "", "OCI image layout directory (mutually exclusive with --registry)")
	flags.String(flagRegistry, "", "OCI registry hostname (mutually exclusive with --source)")
	flags.String(flagRepository, "", "repository path within the registry (requires --registry)")
	flags.String(flagTag, "", "OCI tag to unpack (required)")
	flags.String(flagOutput, "grcli-unpacked", "directory to write extracted files to")

	// Bind at RunE time, not here — see comment in newPublishCmd.
	return cmd
}

func runUnpack(cmd *cobra.Command, v *viper.Viper) error {
	if err := v.BindPFlags(cmd.Flags()); err != nil {
		return fmt.Errorf("binding flags: %w", err)
	}
	ctx := cmd.Context()

	source := v.GetString(flagSource)
	registryHost := v.GetString(flagRegistry)
	repository := v.GetString(flagRepository)
	tag := v.GetString(flagTag)
	output := v.GetString(flagOutput)

	if tag == "" {
		return errors.New("--tag is required")
	}
	switch {
	case source == "" && registryHost == "":
		return errors.New("either --source or --registry is required")
	case source != "" && registryHost != "":
		return errors.New("--source and --registry are mutually exclusive")
	case registryHost != "" && repository == "":
		return errors.New("--repository is required when --registry is set")
	}

	var (
		unpacked *bundle.Bundle
		refLabel string
		err      error
	)
	if source != "" {
		unpacked, err = registry.UnpackLocal(ctx, source, tag)
		refLabel = source
	} else {
		unpacked, err = registry.UnpackRemote(ctx, registryHost, repository, tag)
		refLabel = registryHost + "/" + repository
	}
	if err != nil {
		return err
	}

	if err := os.MkdirAll(output, 0o755); err != nil {
		return fmt.Errorf("creating output dir: %w", err)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "unpacked %s:%s → %s (%d files, %d imports)\n",
		refLabel, tag, output, len(unpacked.Files), len(unpacked.Imports))
	return writeBundle(unpacked, output, out)
}

// writeBundle writes the bundle's primary files, any imports (under an
// imports/ subdir to avoid collisions), and the bundle manifest as
// bundle.json. Filenames are path-cleaned and rejected if they try to
// escape the output directory.
func writeBundle(b *bundle.Bundle, dir string, out io.Writer) error {
	for _, file := range b.Files {
		name, err := safeWriteFile(dir, file.Name, file.Data)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "  - %s\n", name)
	}
	if len(b.Imports) > 0 {
		importsDir := filepath.Join(dir, "imports")
		if err := os.MkdirAll(importsDir, 0o755); err != nil {
			return fmt.Errorf("creating imports dir: %w", err)
		}
		for _, file := range b.Imports {
			name, err := safeWriteFile(importsDir, file.Name, file.Data)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "  - imports/%s\n", name)
		}
	}
	if !b.Manifest.Empty() {
		manifestBytes, err := json.MarshalIndent(b.Manifest, "", "  ")
		if err != nil {
			return fmt.Errorf("encoding manifest: %w", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "bundle.json"), manifestBytes, 0o644); err != nil {
			return fmt.Errorf("writing manifest: %w", err)
		}
		fmt.Fprintln(out, "  - bundle.json (bundle manifest)")
	}
	return nil
}

// safeWriteFile writes data to dir/name, rejecting names that would
// escape dir via "..", absolute paths, or other traversal tricks.
// Returns the path-cleaned name (relative to dir) on success.
func safeWriteFile(dir, name string, data []byte) (string, error) {
	if name == "" {
		return "", errors.New("bundle file has empty name")
	}
	clean := filepath.Clean(name)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe bundle file name %q", name)
	}
	path := filepath.Join(dir, clean)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return clean, nil
}
