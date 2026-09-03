// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/revanite-io/grcli/internal/hub"
)

const flagLatest = "latest"

// digestDisplayLen is the count of hex chars after "sha256:" shown in
// the default table. Matches the docker/oras short-digest convention so
// the table stays readable in narrow terminals; full digests remain
// available via the hub API directly.
const digestDisplayLen = 12

func newVersionsCmd(v *viper.Viper) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "versions <namespace>/<catalog-id>",
		Aliases: []string{"releases"},
		Short:   "List published versions of a named asset",
		Long: `Looks up an asset on the hub and prints its published versions,
newest first. The asset is identified by its <namespace>/<catalog-id>
coordinate — the same shape grcli publish uses for --repository.

By default every release is printed; pass --latest to print only the
current latest version (suitable for scripting — exits 0 with no
output if the catalog has no releases yet).

Reads are public — no token required.

Examples:
  grcli versions finos-ccc/ccc.objstor.cn
  grcli versions finos-ccc/ccc.objstor.cn --latest`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVersions(cmd, v, args[0])
		},
	}

	flags := cmd.Flags()
	flags.String(flagURL, defaultURL, "grc.store base URL")
	flags.Bool(flagLatest, false, "print only the latest version")

	return cmd
}

func runVersions(cmd *cobra.Command, v *viper.Viper, coord string) error {
	if err := v.BindPFlags(cmd.Flags()); err != nil {
		return fmt.Errorf("binding flags: %w", err)
	}
	ctx := cmd.Context()

	ns, id, ok := strings.Cut(coord, "/")
	if !ok || ns == "" || id == "" || strings.Contains(id, "/") {
		return fmt.Errorf("expected <namespace>/<catalog-id>, got %q", coord)
	}

	// versions has no local/dry-run mode — the hub is the only source of
	// truth, so an empty --url is a hard error here (unlike publish/unpack
	// where --url="" is meaningful for offline flows).
	url := v.GetString(flagURL)
	if url == "" {
		return errors.New("--url is required")
	}

	catalog, err := hub.New(url, "").GetCatalog(ctx, ns, id)
	if err != nil {
		// Both ErrCatalogNotFound and ErrCatalogTombstoned already carry
		// the namespace/id in their wrapped messages, so passing the error
		// through is fine — the user sees "catalog not found: ns/id" or
		// "catalog was yanked: ns/id" without the leaky hub URL.
		return err
	}

	out := cmd.OutOrStdout()
	if v.GetBool(flagLatest) {
		return writeLatest(out, catalog)
	}
	return writeReleases(out, catalog)
}

// writeLatest prints the latest version on its own line. When the
// catalog exists but has no releases yet, exits silently with success
// so `grcli versions x/y --latest | xargs ...` pipelines have a clean
// no-op rather than a noisy error.
func writeLatest(out io.Writer, catalog *hub.Catalog) error {
	if catalog.LatestVersion == "" {
		return nil
	}
	fmt.Fprintln(out, catalog.LatestVersion)
	return nil
}

func writeReleases(out io.Writer, catalog *hub.Catalog) error {
	if len(catalog.Releases) == 0 {
		return fmt.Errorf("hub returned no releases for %s/%s",
			catalog.Namespace, catalog.CatalogID)
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "VERSION\tPUSHED\tDIGEST")
	for _, r := range catalog.Releases {
		marker := r.Version
		if r.Version == catalog.LatestVersion {
			marker += " (latest)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", marker, r.PushedAt, shortDigest(r.ManifestDigest))
	}
	return tw.Flush()
}

// shortDigest collapses a sha256:HEX64 digest down to sha256:HEX12 for
// the default table. Anything that doesn't match the expected shape is
// returned unchanged so unrecognized digest algorithms still display.
func shortDigest(d string) string {
	const prefix = "sha256:"
	if !strings.HasPrefix(d, prefix) {
		return d
	}
	hex := d[len(prefix):]
	if len(hex) <= digestDisplayLen {
		return d
	}
	return prefix + hex[:digestDisplayLen]
}
