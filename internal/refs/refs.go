// SPDX-License-Identifier: Apache-2.0

// Package refs parses the mapping references out of a Gemara artifact body
// and decides which of them grcli unpack should resolve against a
// hub. It is deliberately pure — no network, no filesystem — so the
// selection and host-recognition rules are unit-testable in isolation.
//
// Gemara models references in two layers (go-gemara generated_types.go):
//   - metadata.mapping-references is the registry of external documents, each
//     a {id, title, version, url}. The url+version live here.
//   - relationship fields (extends, imports, lexicon) point INTO that registry
//     by reference-id; they carry the relationship, not the locator.
//
// So --with-references resolves every entry in the metadata registry, while
// --with-imports resolves only the entries an `imports` relationship points at.
package refs

import (
	"fmt"
	neturl "net/url"
	"strings"

	gemara "github.com/gemaraproj/go-gemara"
	"github.com/revanite-io/grc-store-protocol/slug"
	"sigs.k8s.io/yaml"
)

// Mode selects which references to resolve.
type Mode int

const (
	// ImportsOnly resolves only references targeted by an `imports`
	// relationship (--with-imports).
	ImportsOnly Mode = iota
	// AllReferences resolves every mapping reference in the metadata
	// registry (--with-references).
	AllReferences
)

// Reference category labels (also the materialization subdirectory names).
const (
	CategoryImports   = "imports"
	CategoryExtends   = "extends"
	CategoryLexicon   = "lexicon"
	CategoryReference = "reference"
)

// Selected is one mapping reference chosen for resolution.
type Selected struct {
	Category string // imports | extends | lexicon | reference
	ID       string // the MappingReference.id
	Title    string
	Version  string // MappingReference.version (the locator carries no version)
	URL      string // MappingReference.url
}

// Artifact is the subset of a parsed Gemara artifact that matters for
// reference resolution.
type Artifact struct {
	// Type is the artifact's metadata.type, for diagnostics.
	Type string
	// MappingRefs is the metadata registry of external documents.
	MappingRefs []gemara.MappingReference
	// category maps a MappingReference.id to how it is referenced.
	category map[string]string
	// importIDs is the set of MappingReference ids an `imports`
	// relationship points at.
	importIDs map[string]bool
	// Notes records non-fatal parse caveats (e.g. an artifact type whose
	// imports shape we don't yet walk), surfaced to the user.
	Notes []string
}

// Scan parses an artifact body (YAML) and extracts its mapping references and
// the relationships that point at them. Metadata parsing is required; failure
// to parse the relationship fields (e.g. Policy's differently-shaped `imports`)
// is recorded as a Note rather than failing — --with-references still works off
// the metadata registry alone.
func Scan(body []byte) (*Artifact, error) {
	var meta struct {
		Metadata gemara.Metadata `json:"metadata"`
	}
	if err := yaml.Unmarshal(body, &meta); err != nil {
		return nil, fmt.Errorf("parsing artifact metadata: %w", err)
	}

	a := &Artifact{
		Type:        meta.Metadata.Type.String(),
		MappingRefs: meta.Metadata.MappingReferences,
		category:    make(map[string]string),
		importIDs:   make(map[string]bool),
	}

	// lexicon is a single optional relationship on the metadata block.
	if lex := meta.Metadata.Lexicon; lex != nil && lex.ReferenceId != "" {
		a.category[lex.ReferenceId] = CategoryLexicon
	}

	// extends/imports are top-level on the catalog artifact types and share a
	// uniform shape ([]ArtifactMapping / []MultiEntryMapping). Policy carries a
	// structurally different `imports`, which fails this decode — caught and
	// noted, not fatal.
	var rel struct {
		Imports []gemara.MultiEntryMapping `json:"imports"`
		Extends []gemara.ArtifactMapping   `json:"extends"`
	}
	if err := yaml.Unmarshal(body, &rel); err != nil {
		a.Notes = append(a.Notes, fmt.Sprintf(
			"could not read imports/extends relationships for artifact type %q (%v) — "+
				"--with-imports will resolve nothing for it; use --with-references to pull every mapping reference",
			a.Type, err))
		return a, nil
	}
	for _, ext := range rel.Extends {
		if ext.ReferenceId != "" {
			a.category[ext.ReferenceId] = CategoryExtends
		}
	}
	for _, imp := range rel.Imports {
		if imp.ReferenceId != "" {
			a.category[imp.ReferenceId] = CategoryImports
			a.importIDs[imp.ReferenceId] = true
		}
	}
	return a, nil
}

// Select returns the references to resolve for the given mode. References with
// no url are skipped (nothing to retrieve). Order follows the metadata registry.
func (a *Artifact) Select(mode Mode) []Selected {
	var out []Selected
	for _, r := range a.MappingRefs {
		if strings.TrimSpace(r.Url) == "" {
			continue
		}
		if mode == ImportsOnly && !a.importIDs[r.Id] {
			continue
		}
		cat := a.category[r.Id]
		if cat == "" {
			cat = CategoryReference
		}
		out = append(out, Selected{
			Category: cat,
			ID:       r.Id,
			Title:    r.Title,
			Version:  r.Version,
			URL:      r.Url,
		})
	}
	return out
}

// Recognize decides whether a reference URL points at an artifact resolvable
// against the targeted hub, and if so extracts its (namespace, catalogID) from
// the URL path. The version is NOT taken from the URL — it lives in the
// MappingReference.version field.
//
// Rules, given the host of the --url target:
//   - a grc.store host (grc.store, hub.grc.store, ...) is the canonical
//     placeholder family: it resolves against the target (we rewrite to the
//     target hub implicitly by using the target client).
//   - host exactly equal to targetHost resolves directly.
//   - any other host is not resolvable here.
//
// The path is read by parseCoordinate, a superset of the rule the hub uses
// to index references (grc.store-backend ResolveReferenceURL, ADR-0040).
// Segments are slugified with the shared hub rule, so a mixed-case url
// reaches the row the hub actually indexed.
//
// ok=false carries a human reason for the skip report; it is never an error —
// an unrecognized reference is expected and benign.
func Recognize(refURL, targetHost string) (namespace, catalogID string, ok bool, reason string) {
	u, err := neturl.Parse(refURL)
	if err != nil {
		return "", "", false, fmt.Sprintf("unparseable URL %q", refURL)
	}
	if u.Host == "" {
		return "", "", false, fmt.Sprintf("URL %q has no host (a Gemara reference must be an absolute https URL)", refURL)
	}
	if !isGrcStoreHost(u.Host) && u.Host != targetHost {
		return "", "", false, fmt.Sprintf("host %q is neither grc.store nor the targeted hub %q", u.Host, targetHost)
	}
	c, ok := parseCoordinate(u.Path)
	if !ok {
		return "", "", false, fmt.Sprintf("path %q is not /{namespace}/{catalog_id}", u.Path)
	}
	return c.Namespace, c.CatalogID, true, ""
}

// Canonical is the one url form for a reference to a hub artifact that every
// grc.store surface (hub index, web UI, grcli) resolves: the coordinate as it
// appears in the artifact's page address, with the version in
// MappingReference.version rather than in the url.
func Canonical(namespace, catalogID string) string {
	return "https://grc.store/" + namespace + "/" + catalogID
}

// coordinate is what parseCoordinate reads out of a url path.
type coordinate struct {
	Namespace, CatalogID string
	// URLVersion is a /versions/<v> suffix, when present. Informational only.
	URLVersion string
	// Canonical reports whether the path was already the canonical
	// /<ns>/<id> form with slug-form segments and no version suffix.
	Canonical bool
}

// parseCoordinate reads a hub coordinate out of a url path. Accepted shapes,
// all with an optional trailing /versions/<v>:
//
//	/{ns}/{id}                     canonical UI form
//	.../v1/catalogs/{ns}/{id}      hub API form (any prefix)
//	/search/{ns}/{id}              legacy UI form, still found in published catalogs
//
// The hub's reference index only knows the first two, and the UI form only
// without a version suffix; the rest resolve here but earn a Lint warning.
// Segments are slugified. Empty after slugify → not a coordinate.
func parseCoordinate(path string) (coordinate, bool) {
	var segs []string
	for _, s := range strings.Split(path, "/") {
		if s != "" {
			segs = append(segs, s)
		}
	}
	// API form: strip everything through "v1/catalogs".
	for i := 0; i+1 < len(segs); i++ {
		if segs[i] == "v1" && segs[i+1] == "catalogs" {
			segs = segs[i+2:]
			break
		}
	}
	// Legacy form.
	if len(segs) > 0 && segs[0] == "search" {
		segs = segs[1:]
	}
	var c coordinate
	switch {
	case len(segs) == 2:
	case len(segs) == 4 && segs[2] == "versions":
		c.URLVersion = segs[3]
	default:
		return coordinate{}, false
	}
	c.Namespace, c.CatalogID = slug.Slugify(segs[0]), slug.Slugify(segs[1])
	if c.Namespace == "" || c.CatalogID == "" {
		return coordinate{}, false
	}
	// Canonical iff nothing was stripped, no version suffix, and every
	// segment was already in slug form.
	c.Canonical = strings.Trim(path, "/") == c.Namespace+"/"+c.CatalogID
	return c, true
}

// isGrcStoreHost reports whether host is grc.store or a subdomain of it,
// case- and port-insensitively. Mirrors the hub's hostIsGrcStore.
func isGrcStoreHost(host string) bool {
	h := strings.ToLower(host)
	if i := strings.IndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	return h == "grc.store" || strings.HasSuffix(h, ".grc.store")
}

// Lint returns human-readable warnings about references that look like they
// were meant to name a grc.store artifact but will not resolve everywhere:
//   - a relationship (imports/extends/lexicon) whose mapping reference has no
//     url — nothing can retrieve it;
//   - a grc.store url whose path is not a coordinate;
//   - a grc.store url that resolves here but is not the canonical form the
//     hub index and the web UI agree on (legacy /search/, API path, version
//     in the url, non-slug segments).
//
// Warnings only: a publisher may reference an external standard by any url,
// and the hub accepts every one of these bodies.
func (a *Artifact) Lint() []string {
	var out []string
	for _, r := range a.MappingRefs {
		raw := strings.TrimSpace(r.Url)
		if raw == "" {
			if cat := a.category[r.Id]; cat != "" {
				out = append(out, fmt.Sprintf(
					"mapping reference %q is used by %s but has no url; nothing can resolve it. "+
						"To reference a grc.store artifact set url: %s", r.Id, cat, Canonical("<namespace>", "<id>")))
			}
			continue
		}
		u, err := neturl.Parse(raw)
		if err != nil || !isGrcStoreHost(u.Host) {
			continue // external standard, or unparseable: not ours to judge
		}
		c, ok := parseCoordinate(u.Path)
		if !ok {
			out = append(out, fmt.Sprintf(
				"mapping reference %q url %q does not name a grc.store artifact; the form is %s",
				r.Id, raw, Canonical("<namespace>", "<id>")))
			continue
		}
		if !c.Canonical {
			msg := fmt.Sprintf("mapping reference %q url %q resolves, but the canonical form is %s",
				r.Id, raw, Canonical(c.Namespace, c.CatalogID))
			if c.URLVersion != "" {
				msg += fmt.Sprintf(" with version: %q", c.URLVersion)
			}
			out = append(out, msg)
		}
	}
	return out
}
