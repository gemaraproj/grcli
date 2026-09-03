// SPDX-License-Identifier: Apache-2.0

// Package provenance produces a SLSA v1.0-shaped JSON predicate that is
// embedded as the "provenance" key of the OCI bundle's manifest
// metadata. The shape is forward-compatible with a future cosign DSSE
// attestation flow; once the hub gains a verifier, the same predicate
// can be lifted verbatim into a signed envelope.
package provenance

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"
)

// PredicateType is the SLSA v1 provenance predicate type identifier.
const PredicateType = "https://slsa.dev/provenance/v1"

// BuildType is grcli's own build-type URI — the schema for the
// invocation/buildConfig fields below. Unversioned so we can iterate
// the shape before there's a verifier consuming it; v1 once stable.
const BuildType = "https://grc.store/grcli/buildtype/v0"

// Predicate is the JSON object embedded under bundle metadata.provenance.
// Field names match the SLSA v1.0 ProvenanceBuildV1 predicate so that
// consumers can validate against the public schema even before grcli
// emits signed attestations.
type Predicate struct {
	BuildDefinition BuildDefinition `json:"buildDefinition"`
	RunDetails      RunDetails      `json:"runDetails"`
}

// BuildDefinition mirrors SLSA's BuildDefinition struct.
type BuildDefinition struct {
	BuildType            string          `json:"buildType"`
	ExternalParameters   map[string]any  `json:"externalParameters,omitempty"`
	InternalParameters   map[string]any  `json:"internalParameters,omitempty"`
	ResolvedDependencies []ResourceDescr `json:"resolvedDependencies,omitempty"`
}

// RunDetails mirrors SLSA's RunDetails struct.
type RunDetails struct {
	Builder    Builder         `json:"builder"`
	Metadata   Metadata        `json:"metadata"`
	Byproducts []ResourceDescr `json:"byproducts,omitempty"`
}

// Builder identifies what produced the build.
type Builder struct {
	ID                  string            `json:"id"`
	Version             map[string]string `json:"version,omitempty"`
	BuilderDependencies []ResourceDescr   `json:"builderDependencies,omitempty"`
}

// Metadata captures invocation-time info about the build run.
type Metadata struct {
	InvocationID string    `json:"invocationId,omitempty"`
	StartedOn    time.Time `json:"startedOn"`
	FinishedOn   time.Time `json:"finishedOn,omitempty"`
}

// ResourceDescr is the SLSA v1 resource descriptor used for inputs,
// dependencies, and byproducts. URI + digest are the load-bearing
// fields; name is informational.
type ResourceDescr struct {
	Name   string            `json:"name,omitempty"`
	URI    string            `json:"uri,omitempty"`
	Digest map[string]string `json:"digest,omitempty"`
}

// Input is the data grcli's caller has, packed into a tiny struct so
// Build() doesn't grow a 12-arg signature as fields accrete.
type Input struct {
	ToolVersion    string
	StartedOn      time.Time
	ArtifactType   string
	ArtifactID     string
	ArtifactName   string
	ArtifactDigest string            // sha256:<hex> of the merged bundle body
	SourceFiles    map[string]string // path -> sha256:<hex>
	Registry       string
	Repository     string
	Tag            string
}

// Build assembles the SLSA-shaped predicate from environment + Input.
// It never returns an error: missing fields degrade to omitted entries
// rather than failing the publish.
func Build(in Input) Predicate {
	builderID, builderVer := identifyBuilder(in.ToolVersion)

	external := map[string]any{
		"artifact": map[string]string{
			"type": in.ArtifactType,
			"id":   in.ArtifactID,
		},
		"target": map[string]string{
			"registry":   in.Registry,
			"repository": in.Repository,
			"tag":        in.Tag,
		},
	}

	resolved := make([]ResourceDescr, 0, len(in.SourceFiles)+1)
	for _, path := range sortedKeys(in.SourceFiles) {
		digest := in.SourceFiles[path]
		resolved = append(resolved, ResourceDescr{
			Name:   path,
			URI:    "file://" + path,
			Digest: digestMap(digest),
		})
	}
	if git := detectGit(); git != nil {
		resolved = append(resolved, *git)
	}

	byproducts := []ResourceDescr{}
	if in.ArtifactDigest != "" {
		byproducts = append(byproducts, ResourceDescr{
			Name:   in.ArtifactName,
			Digest: digestMap(in.ArtifactDigest),
		})
	}

	return Predicate{
		BuildDefinition: BuildDefinition{
			BuildType:            BuildType,
			ExternalParameters:   external,
			InternalParameters:   internalParams(),
			ResolvedDependencies: resolved,
		},
		RunDetails: RunDetails{
			Builder: Builder{
				ID:      builderID,
				Version: builderVer,
			},
			Metadata: Metadata{
				InvocationID: invocationID(),
				StartedOn:    in.StartedOn,
				FinishedOn:   time.Now().UTC(),
			},
			Byproducts: byproducts,
		},
	}
}

// identifyBuilder distinguishes a CI run (preferred SLSA identity) from
// a local invocation (best-effort, never asserted as trusted).
func identifyBuilder(toolVersion string) (string, map[string]string) {
	ver := map[string]string{
		"grcli":   toolVersion,
		"go":      runtime.Version(),
		"go-arch": runtime.GOARCH,
		"go-os":   runtime.GOOS,
	}
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		server := envOr("GITHUB_SERVER_URL", "https://github.com")
		return fmt.Sprintf("%s/%s/actions/runs/%s",
			server,
			os.Getenv("GITHUB_REPOSITORY"),
			os.Getenv("GITHUB_RUN_ID")), ver
	}
	host, _ := os.Hostname()
	user := envOr("USER", envOr("USERNAME", "unknown"))
	return fmt.Sprintf("local://%s@%s", user, host), ver
}

func internalParams() map[string]any {
	out := map[string]any{}
	// Allowlist only env vars that document the build environment
	// without leaking secrets. Anything that looks like a token is
	// excluded by design.
	allow := []string{
		"GITHUB_ACTIONS", "GITHUB_WORKFLOW", "GITHUB_RUN_ID",
		"GITHUB_RUN_ATTEMPT", "GITHUB_REPOSITORY", "GITHUB_REF",
		"GITHUB_SHA", "GITHUB_ACTOR", "RUNNER_OS", "CI",
	}
	for _, k := range allow {
		if v := os.Getenv(k); v != "" {
			out[k] = v
		}
	}
	return out
}

func invocationID() string {
	if v := os.Getenv("GITHUB_RUN_ID"); v != "" {
		if a := os.Getenv("GITHUB_RUN_ATTEMPT"); a != "" {
			return v + "-" + a
		}
		return v
	}
	return ""
}

func detectGit() *ResourceDescr {
	// Best-effort: only emit a materials entry if we're inside a git
	// repo and can resolve both remote + HEAD. Anything less and we
	// silently skip — provenance is informative, not authoritative.
	remote, err := gitCmd("config", "--get", "remote.origin.url")
	if err != nil || remote == "" {
		return nil
	}
	sha, err := gitCmd("rev-parse", "HEAD")
	if err != nil || sha == "" {
		return nil
	}
	return &ResourceDescr{
		Name:   "source",
		URI:    "git+" + strings.TrimSuffix(stripUserinfo(remote), ".git") + "@" + sha,
		Digest: map[string]string{"gitCommit": sha},
	}
}

// stripUserinfo drops credentials embedded in a remote URL
// (https://user:token@host/…) so they never land in signed, immutable
// provenance. scp-style remotes (git@host:path) have no userinfo and pass
// through unchanged.
func stripUserinfo(remote string) string {
	u, err := url.Parse(remote)
	if err != nil || u.Scheme == "" || u.User == nil {
		return remote
	}
	u.User = nil
	return u.String()
}

func gitCmd(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func digestMap(prefixed string) map[string]string {
	idx := strings.Index(prefixed, ":")
	if idx < 0 {
		return map[string]string{"sha256": prefixed}
	}
	return map[string]string{prefixed[:idx]: prefixed[idx+1:]}
}
