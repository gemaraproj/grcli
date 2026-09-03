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
// the URL path. The version is NOT in the URL — it lives
// in the MappingReference.version field.
//
// Rules, given the host of the --url target:
//   - host "grc.store" is the canonical placeholder: it resolves against the
//     target (we rewrite to the target hub implicitly by using the target client).
//   - host exactly equal to targetHost resolves directly.
//   - any other host is not resolvable here.
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
	if u.Host != "grc.store" && u.Host != targetHost {
		return "", "", false, fmt.Sprintf("host %q is neither grc.store nor the targeted hub %q", u.Host, targetHost)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false, fmt.Sprintf("path %q is not /{namespace}/{catalog_id}", u.Path)
	}
	return parts[0], parts[1], true, ""
}
