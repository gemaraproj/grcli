// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"fmt"
	"io"

	"github.com/gemaraproj/go-gemara/bundle"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func newCatCmd(v *viper.Viper) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cat",
		Short: "Print a Gemara artifact's contents to stdout",
		Long: `Streams a Gemara artifact's contents to stdout without writing any files.
This is the read-only companion to 'grcli unpack' (which writes a directory):
cat is for piping into yq/jq or eyeballing an artifact.

cat emits Gemara content ONLY — the artifact file(s), which carry the Gemara
document including its metadata block. It does NOT emit bundle information:
the bundle.json manifest and any SLSA-shaped provenance are not reachable
through cat. Use 'grcli unpack' if you want the manifest.

The source can be a local OCI image layout (--source) or a remote registry
discovered from the hub (--url plus --repository). Exactly one must be set,
plus --version.

The bundle's artifact prints verbatim. --file <name> must name that
artifact. Caching behaves exactly as for 'grcli unpack':
a remote fetch is served from the on-disk cache when warm; --no-cache forces a
fresh pull. Cache diagnostics go to stderr so stdout stays pipe-clean.

Examples:
  # Print the artifact from a remote registry
  grcli cat --url https://hub.grc.store \
    --repository myorg/my-controls --version 1.0.0

  # Pipe into yq
  grcli cat --url https://hub.grc.store \
    --repository myorg/my-controls --version 1.0.0 | yq '.title'

  # From a local 'publish --dry-run' output, one file out of several
  grcli cat --source ./grcli-out --version 1.0.0 --file controls.yaml`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCat(cmd, v)
		},
	}

	flags := cmd.Flags()
	flags.String(flagSource, "", "OCI image layout directory (mutually exclusive with --url)")
	flags.String(flagURL, defaultURL, "grc.store base URL (discovers the registry)")
	flags.String(flagRepository, "", "repository path within the registry (requires --url)")
	flags.String(flagVersion, "", "artifact version to print — the metadata.version of the published bundle (required)")
	flags.String(flagFile, "", "print the artifact only if it has this file name")
	flags.Bool(flagNoCache, false, "bypass the local artifact cache for this run; set cache-enabled: false in config to disable it durably")

	// Bind at RunE time, not here — see comment in newPublishCmd.
	return cmd
}

func runCat(cmd *cobra.Command, v *viper.Viper) error {
	if err := v.BindPFlags(cmd.Flags()); err != nil {
		return fmt.Errorf("binding flags: %w", err)
	}
	suppressDefaultURLIfExplicit(cmd, v, flagSource)
	ctx := cmd.Context()

	// Diagnostics to stderr; artifact content to stdout — keep stdout pipeable.
	b, _, err := resolveBundle(ctx, v, cmd.ErrOrStderr())
	if err != nil {
		return err
	}
	// Reference resolution is unpack's job, and the v2 cache never
	// stores Imports — but a --source layout can carry them. Never drop content
	// silently: cat prints Files only, so say what was omitted (on stderr).
	if len(b.Imports) > 0 {
		noteCatOmittedImports(cmd.ErrOrStderr(), len(b.Imports))
	}
	// Read --file from the command's OWN flags, not viper: the viper key "file"
	// is publish's input-file list (bound from config/env as GRCLI_FILE /
	// `file:` in .grcli.yaml), and reading it here would let publish settings
	// select a bundle member the user never asked for.
	fileName, err := cmd.Flags().GetString(flagFile)
	if err != nil {
		return err
	}
	return catBundle(b, fileName, cmd.OutOrStdout())
}

// noteCatOmittedImports warns (on the diagnostics stream, never stdout) that a
// bundle's imports are not part of cat's output.
func noteCatOmittedImports(w io.Writer, n int) {
	fmt.Fprintf(w, "! bundle carries %d import(s) not included in cat output — use 'grcli unpack' to materialize them\n", n)
}

// catBundle writes a bundle's artifact to out byte-for-byte. When fileName is
// set it must name that artifact. It never writes bundle.json.
func catBundle(b *bundle.Bundle, fileName string, out io.Writer) error {
	files := artifactFiles(b)
	if len(files) == 0 {
		return fmt.Errorf("bundle has no artifact files")
	}
	if fileName != "" && fileName != files[0].Name {
		return fmt.Errorf("no file named %q in bundle (have: %s)", fileName, files[0].Name)
	}
	_, err := out.Write(files[0].Data)
	return err
}

// artifactFiles returns a bundle's artifact files: its Source, when set.
// go-gemara models a bundle as one Source artifact plus Imports, so a bundle
// has at most one artifact file. Imports are handled separately.
func artifactFiles(b *bundle.Bundle) []bundle.File {
	if b.Source.Name == "" {
		return nil
	}
	return []bundle.File{b.Source}
}
