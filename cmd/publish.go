// SPDX-License-Identifier: LicenseRef-Revanite-Proprietary

package cmd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
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

// Flag names are declared once so the compiler catches typos at every
// viper.Get call site.
const (
	flagFile       = "file"
	flagRegistry   = "registry"
	flagRepository = "repository"
	flagTag        = "tag"
	flagHubURL     = "hub-url"
	flagToken      = "token"
	flagDryRun     = "dry-run"
	flagOutput     = "output"
	flagNoSign     = "no-sign"
	flagCosignKey  = "cosign-key"
)

func newPublishCmd(v *viper.Viper) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "publish",
		Short: "Bundle one Gemara artifact with provenance and push it to grc.store",
		Long: `Loads the file(s) provided via -f, verifies they describe a single
artifact, attaches a SLSA-shaped provenance record, packs an OCI bundle,
pushes it to the configured registry, optionally signs with cosign,
and notifies the hub via POST /v1/bundles/sync.

Use --dry-run to write the bundle to an OCI image layout on disk
instead of touching any network.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPublish(cmd, v)
		},
	}

	flags := cmd.Flags()
	flags.StringSliceP(flagFile, "f", nil, "input file(s) describing one artifact (repeatable; comma-separated also accepted)")
	flags.String(flagRegistry, "", "OCI registry hostname, e.g. registry.grc.store")
	flags.String(flagRepository, "", "repository path within the registry (default: <author.id>/<metadata.id>)")
	flags.String(flagTag, "", "OCI tag (default: metadata.version)")
	flags.String(flagHubURL, "", "grc.store hub base URL, e.g. https://grc.store")
	flags.String(flagToken, "", "bearer token for the hub sync call (or GRCLI_TOKEN)")
	flags.Bool(flagDryRun, false, "skip all network — emit OCI layout to --output instead")
	flags.String(flagOutput, "grcli-out", "directory to write the OCI layout to when --dry-run")
	flags.Bool(flagNoSign, false, "skip cosign signing even when material is available")
	flags.String(flagCosignKey, "", "cosign key file for local signing (or COSIGN_KEY)")

	// Bind every flag in one call so env (GRCLI_*) and config-file
	// resolution work uniformly without a hand-maintained name list.
	_ = v.BindPFlags(flags)
	// COSIGN_KEY is the conventional env name for the cosign key path;
	// override the GRCLI_ prefix so existing cosign users see it picked up.
	_ = v.BindEnv(flagCosignKey, "COSIGN_KEY")

	return cmd
}

// publishTarget holds the resolved push destination after flags, config,
// and artifact metadata defaults are merged.
type publishTarget struct {
	registryHost string
	repository   string
	tag          string
	dryRun       bool
	output       string
}

func runPublish(cmd *cobra.Command, v *viper.Viper) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	startedOn := time.Now().UTC()

	files := expandCommas(v.GetStringSlice(flagFile))
	if len(files) == 0 {
		return errors.New("at least one --file is required")
	}

	loaded, err := source.Load(ctx, files)
	if err != nil {
		return err
	}

	target, err := resolveTarget(v, loaded)
	if err != nil {
		return err
	}

	predicate := provenance.Build(provenance.Input{
		ToolVersion:    version,
		StartedOn:      startedOn,
		ArtifactType:   loaded.Type,
		ArtifactID:     loaded.ID,
		ArtifactName:   loaded.Filename,
		ArtifactDigest: registry.SHA256Hex(loaded.Body),
		SourceFiles:    loaded.SourceDigests,
		Registry:       target.registryHost,
		Repository:     target.repository,
		Tag:            target.tag,
	})

	packInput := registry.PackInput{
		Filename:      loaded.Filename,
		ArtifactType:  loaded.Type,
		ArtifactID:    loaded.ID,
		GemaraVersion: loaded.GemaraVersion,
		Body:          loaded.Body,
		Provenance:    predicate,
	}

	out := cmd.OutOrStdout()
	result, err := pushBundle(ctx, target, packInput, out, loaded.Type, loaded.ID)
	if err != nil {
		return err
	}
	if target.dryRun {
		return nil
	}

	return signAndNotify(ctx, v, target.repository, target.tag, result.Reference, out)
}

// resolveTarget merges --tag/--repository/--registry/--dry-run with the
// metadata-derived defaults and validates the combination.
func resolveTarget(v *viper.Viper, loaded *source.Loaded) (publishTarget, error) {
	tag := cmp.Or(v.GetString(flagTag), loaded.Version)
	if tag == "" {
		return publishTarget{}, errors.New("could not determine tag — set --tag or metadata.version")
	}
	repository := cmp.Or(v.GetString(flagRepository), defaultRepository(loaded.AuthorID, loaded.ID))
	if repository == "" {
		return publishTarget{}, errors.New("could not determine --repository — set it explicitly or populate metadata.author.id + metadata.id")
	}
	target := publishTarget{
		registryHost: v.GetString(flagRegistry),
		repository:   repository,
		tag:          tag,
		dryRun:       v.GetBool(flagDryRun),
		output:       v.GetString(flagOutput),
	}
	if !target.dryRun && target.registryHost == "" {
		return publishTarget{}, errors.New("--registry is required (use --dry-run to skip push)")
	}
	return target, nil
}

// pushBundle either writes the bundle to a local OCI layout (dry-run)
// or pushes it to the configured registry, printing a one-line summary
// in either case.
func pushBundle(ctx context.Context, target publishTarget, in registry.PackInput, out io.Writer, artifactType, artifactID string) (*registry.PushResult, error) {
	if target.dryRun {
		result, err := registry.PushLocal(ctx, target.output, target.tag, in)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(out,
			"dry-run: wrote bundle to %s\n  manifest digest: %s\n  body digest:     %s\n  artifact: %s/%s\n",
			result.Reference, result.ManifestDigest, result.BodyDigest, artifactType, artifactID)
		return result, nil
	}
	result, err := registry.PushRemote(ctx, target.registryHost, target.repository, target.tag, in)
	if err != nil {
		return nil, fmt.Errorf("push: %w", err)
	}
	fmt.Fprintf(out, "pushed %s\n  manifest digest: %s\n", result.Reference, result.ManifestDigest)
	return result, nil
}

// signAndNotify runs the optional cosign step and the hub sync call,
// reporting each outcome to out. Either step can be skipped via flags
// without producing an error.
func signAndNotify(ctx context.Context, v *viper.Viper, repository, tag, reference string, out io.Writer) error {
	signResult, err := sign.Sign(ctx, sign.Options{
		Disabled:  v.GetBool(flagNoSign),
		KeyPath:   v.GetString(flagCosignKey),
		Reference: reference,
	})
	if err != nil {
		return fmt.Errorf("sign: %w", err)
	}
	if signResult.Mode == sign.ModeSkipped {
		fmt.Fprintf(out, "signing skipped: %s\n", signResult.Reason)
	} else {
		fmt.Fprintf(out, "signed (%s)\n", signResult.Mode)
	}

	hubURL := v.GetString(flagHubURL)
	if hubURL == "" {
		fmt.Fprintln(out, "skipping hub sync: --hub-url not set")
		return nil
	}
	syncResp, err := hub.New(hubURL, v.GetString(flagToken)).Sync(ctx, repository, tag)
	if err != nil {
		return fmt.Errorf("hub sync: %w", err)
	}
	fmt.Fprintf(out,
		"hub indexed %s:%s — %d artifacts (%d new), types=%s\n",
		syncResp.Repository, syncResp.Tag,
		syncResp.ArtifactCount, syncResp.NewCount,
		strings.Join(syncResp.Types, ","),
	)
	return nil
}

// expandCommas lets users write `-f a.yaml,b.yaml` in addition to
// `-f a.yaml -f b.yaml`. Cobra's StringSliceP splits commas at the
// flag layer, but viper.GetStringSlice does not when the underlying
// source is a config file, so we re-split defensively.
func expandCommas(in []string) []string {
	out := make([]string, 0, len(in))
	for _, raw := range in {
		for part := range strings.SplitSeq(raw, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

// defaultRepository slugifies <author.id>/<metadata.id> for the
// registry path. Anything outside [a-zA-Z0-9._-] is collapsed to "-".
func defaultRepository(authorID, artifactID string) string {
	if authorID == "" || artifactID == "" {
		return ""
	}
	return slugify(authorID) + "/" + slugify(artifactID)
}

var slugPattern = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func slugify(s string) string {
	s = slugPattern.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-_.")
	return strings.ToLower(s)
}
