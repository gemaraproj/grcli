// SPDX-License-Identifier: LicenseRef-Revanite-Proprietary

package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/revanite-io/grcli/internal/hub"
	"github.com/revanite-io/grcli/internal/provenance"
	"github.com/revanite-io/grcli/internal/registry"
	"github.com/revanite-io/grcli/internal/sign"
	"github.com/revanite-io/grcli/internal/source"
)

func newPublishCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "publish",
		Short: "Bundle one Gemara artifact with provenance and push it to grc.store",
		Long: `Loads the file(s) provided via -f, verifies they describe a single
artifact, attaches a SLSA-shaped provenance record, packs an OCI bundle,
pushes it to the configured registry, optionally signs with cosign,
and notifies the hub via POST /v1/bundles/sync.

Use --dry-run to write the bundle to an OCI image layout on disk
instead of touching any network.`,
		RunE: runPublish,
	}

	f := cmd.Flags()
	f.StringSliceP("file", "f", nil, "input file(s) describing one artifact (repeatable; comma-separated also accepted)")
	f.String("registry", "", "OCI registry hostname, e.g. registry.grc.store")
	f.String("repository", "", "repository path within the registry (default: <author.id>/<metadata.id>)")
	f.String("tag", "", "OCI tag (default: metadata.version)")
	f.String("hub-url", "", "grc.store hub base URL, e.g. https://grc.store")
	f.String("token", "", "bearer token for the hub sync call (or GRCLI_TOKEN)")
	f.Bool("dry-run", false, "skip all network — emit OCI layout to --output instead")
	f.String("output", "grcli-out", "directory to write the OCI layout to when --dry-run")
	f.Bool("no-sign", false, "skip cosign signing even when material is available")
	f.String("cosign-key", "", "cosign key file for local signing (or COSIGN_KEY)")

	// Viper binding. We register every flag so env (GRCLI_*) and config
	// file resolution work uniformly.
	for _, key := range []string{
		"file", "registry", "repository", "tag", "hub-url", "token",
		"dry-run", "output", "no-sign", "cosign-key",
	} {
		_ = viper.BindPFlag(key, f.Lookup(key))
	}
	_ = viper.BindEnv("cosign-key", "COSIGN_KEY")

	return cmd
}

func runPublish(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	startedOn := time.Now().UTC()

	files := viper.GetStringSlice("file")
	if len(files) == 0 {
		return fmt.Errorf("at least one --file is required")
	}
	files = expandCommas(files)

	loaded, err := source.Load(ctx, files)
	if err != nil {
		return err
	}

	tag := firstNonEmpty(viper.GetString("tag"), loaded.Version)
	if tag == "" {
		return fmt.Errorf("could not determine tag — set --tag or metadata.version")
	}
	repository := firstNonEmpty(viper.GetString("repository"),
		defaultRepository(loaded.AuthorID, loaded.ID))
	if repository == "" {
		return fmt.Errorf("could not determine --repository — set it explicitly or populate metadata.author.id + metadata.id")
	}
	registryHost := viper.GetString("registry")
	dryRun := viper.GetBool("dry-run")
	if !dryRun && registryHost == "" {
		return fmt.Errorf("--registry is required (use --dry-run to skip push)")
	}

	pred := provenance.Build(provenance.Input{
		ToolVersion:    version,
		StartedOn:      startedOn,
		ArtifactType:   loaded.Type,
		ArtifactID:     loaded.ID,
		ArtifactName:   loaded.Filename,
		ArtifactDigest: "sha256:" + sha256OfBytes(loaded.Body),
		SourceFiles:    loaded.SourceDigests,
		Registry:       registryHost,
		Repository:     repository,
		Tag:            tag,
	})

	in := registry.PackInput{
		Filename:      loaded.Filename,
		ArtifactType:  loaded.Type,
		ArtifactID:    loaded.ID,
		GemaraVersion: loaded.GemaraVersion,
		Body:          loaded.Body,
		Provenance:    pred,
	}

	var result *registry.PushResult
	if dryRun {
		out := viper.GetString("output")
		result, err = registry.PushLocal(ctx, out, tag, in)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(),
			"dry-run: wrote bundle to %s\n  manifest digest: %s\n  body digest:     %s\n  artifact: %s/%s\n",
			result.Reference, result.ManifestDigest, result.BodyDigest, loaded.Type, loaded.ID)
		return nil
	}

	result, err = registry.PushRemote(ctx, registryHost, repository, tag, in)
	if err != nil {
		return fmt.Errorf("push: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "pushed %s\n  manifest digest: %s\n",
		result.Reference, result.ManifestDigest)

	signResult, err := sign.Sign(ctx, sign.Options{
		Disabled:  viper.GetBool("no-sign"),
		KeyPath:   viper.GetString("cosign-key"),
		Reference: result.Reference,
	})
	if err != nil {
		return fmt.Errorf("sign: %w", err)
	}
	switch signResult.Mode {
	case sign.ModeSkipped:
		fmt.Fprintf(cmd.OutOrStdout(), "signing skipped: %s\n", signResult.Reason)
	default:
		fmt.Fprintf(cmd.OutOrStdout(), "signed (%s)\n", signResult.Mode)
	}

	hubURL := viper.GetString("hub-url")
	if hubURL == "" {
		fmt.Fprintln(cmd.OutOrStdout(), "skipping hub sync: --hub-url not set")
		return nil
	}
	token := viper.GetString("token")
	if token == "" {
		token = os.Getenv("GRCLI_TOKEN")
	}
	syncResp, err := hub.New(hubURL, token).Sync(ctx, repository, tag)
	if err != nil {
		return fmt.Errorf("hub sync: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(),
		"hub indexed %s:%s — %d artifacts (%d new), types=%s\n",
		syncResp.Repository, syncResp.Tag,
		syncResp.ArtifactCount, syncResp.NewCount,
		strings.Join(syncResp.Types, ","),
	)
	return nil
}

// expandCommas lets users write `-f a.yaml,b.yaml` in addition to
// `-f a.yaml -f b.yaml`. Cobra's StringSliceP already splits commas,
// but viper.GetStringSlice does not when the underlying source is a
// config file, so we re-split defensively.
func expandCommas(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		for _, p := range strings.Split(s, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// defaultRepository slugifies <author.id>/<metadata.id> for the
// registry path. Anything outside [a-z0-9-_./] is collapsed to "-".
func defaultRepository(authorID, artifactID string) string {
	if authorID == "" || artifactID == "" {
		return ""
	}
	return slugify(authorID) + "/" + slugify(artifactID)
}

var slugRE = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func slugify(s string) string {
	s = slugRE.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-_.")
	return strings.ToLower(s)
}

func sha256OfBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
