// SPDX-License-Identifier: Apache-2.0

package refs

import (
	"strings"
	"testing"
)

const controlCatalogYAML = `
metadata:
  id: my-catalog
  type: ControlCatalog
  gemara-version: "0.5.0"
  description: a test catalog
  author:
    id: acme
    name: Acme
  mapping-references:
    - id: base
      title: Base Catalog
      version: "2.1.0"
      url: https://grc.store/acme/baseline
    - id: extref
      title: Extended Catalog
      version: "3.0.0"
      url: https://grc.store/acme/extended
    - id: nourl
      title: A reference with no retrievable URL
      version: "1.0.0"
extends:
  - reference-id: extref
imports:
  - reference-id: base
`

func TestScanCategorizesReferences(t *testing.T) {
	a, err := Scan([]byte(controlCatalogYAML))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if a.Type != "ControlCatalog" {
		t.Errorf("Type = %q, want ControlCatalog", a.Type)
	}
	if len(a.MappingRefs) != 3 {
		t.Fatalf("MappingRefs = %d, want 3", len(a.MappingRefs))
	}
	if got := a.category["base"]; got != CategoryImports {
		t.Errorf("category[base] = %q, want %q", got, CategoryImports)
	}
	if got := a.category["extref"]; got != CategoryExtends {
		t.Errorf("category[extref] = %q, want %q", got, CategoryExtends)
	}
	if !a.importIDs["base"] || a.importIDs["extref"] {
		t.Errorf("importIDs = %v, want only base", a.importIDs)
	}
}

func TestSelectByMode(t *testing.T) {
	a, err := Scan([]byte(controlCatalogYAML))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	all := a.Select(AllReferences)
	// base (imports) + extref (extends); nourl has no URL so it is skipped.
	if len(all) != 2 {
		t.Fatalf("AllReferences selected %d, want 2: %+v", len(all), all)
	}

	imports := a.Select(ImportsOnly)
	if len(imports) != 1 {
		t.Fatalf("ImportsOnly selected %d, want 1: %+v", len(imports), imports)
	}
	if imports[0].ID != "base" || imports[0].Category != CategoryImports {
		t.Errorf("ImportsOnly[0] = %+v, want base/imports", imports[0])
	}
	if imports[0].Version != "2.1.0" || imports[0].URL != "https://grc.store/acme/baseline" {
		t.Errorf("ImportsOnly[0] locator = %q@%q, want baseline@2.1.0", imports[0].URL, imports[0].Version)
	}
}

func TestScanPolicyImportsAreNoted(t *testing.T) {
	// Policy's `imports` is a map, not a list — it cannot decode into the
	// catalog-shaped relationship struct. Scan must record a note, not fail,
	// and metadata references must still be readable.
	const policyYAML = `
metadata:
  id: my-policy
  type: Policy
  gemara-version: "0.5.0"
  description: a test policy
  author:
    id: acme
    name: Acme
  mapping-references:
    - id: cat
      title: A catalog
      version: "1.0.0"
      url: https://grc.store/acme/catalog
imports:
  catalogs:
    - reference-id: cat
`
	a, err := Scan([]byte(policyYAML))
	if err != nil {
		t.Fatalf("Scan must not fail on Policy: %v", err)
	}
	if len(a.Notes) == 0 {
		t.Error("expected a note about unreadable Policy imports")
	}
	// --with-references still works off the metadata registry.
	if got := a.Select(AllReferences); len(got) != 1 {
		t.Errorf("AllReferences selected %d, want 1", len(got))
	}
	// --with-imports finds nothing (we don't walk Policy imports yet).
	if got := a.Select(ImportsOnly); len(got) != 0 {
		t.Errorf("ImportsOnly selected %d, want 0 for Policy", len(got))
	}
}

func TestRecognize(t *testing.T) {
	const target = "hub.grc.store"
	cases := []struct {
		name   string
		url    string
		wantOK bool
		wantNS string
		wantID string
	}{
		{"canonical placeholder", "https://grc.store/acme/baseline", true, "acme", "baseline"},
		{"exact target host", "https://hub.grc.store/acme/baseline", true, "acme", "baseline"},
		{"other host", "https://example.com/acme/baseline", false, "", ""},
		{"schemeless", "grc.store/acme/baseline", false, "", ""},
		{"too few path segments", "https://grc.store/acme", false, "", ""},
		{"too many path segments", "https://grc.store/acme/baseline/extra", false, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns, id, ok, reason := Recognize(tc.url, target)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v (reason %q), want %v", ok, reason, tc.wantOK)
			}
			if ok && (ns != tc.wantNS || id != tc.wantID) {
				t.Errorf("(ns,id) = (%q,%q), want (%q,%q)", ns, id, tc.wantNS, tc.wantID)
			}
			if !ok && reason == "" {
				t.Error("expected a non-empty skip reason")
			}
		})
	}
}

func TestRecognizeWidenedShapes(t *testing.T) {
	const target = "hub.preview.grc.store"
	cases := []struct {
		name   string
		url    string
		wantOK bool
		wantNS string
		wantID string
	}{
		{"hub api form on prod host while targeting preview", "https://hub.grc.store/v1/catalogs/acme/baseline", true, "acme", "baseline"},
		{"hub api form with version", "https://hub.grc.store/v1/catalogs/acme/baseline/versions/2.0", true, "acme", "baseline"},
		{"legacy search form", "https://grc.store/search/finos-aigf/finos-air/versions/0.2.0", true, "finos-aigf", "finos-air"},
		{"ui form with version suffix", "https://grc.store/acme/baseline/versions/1.0", true, "acme", "baseline"},
		{"mixed case slugifies to the indexed row", "https://grc.store/FINOS-CCC/CCC.Core.CN", true, "finos-ccc", "ccc.core.cn"},
		{"percent-encoded segment", "https://grc.store/FINOS%20CCC/ccc.objstor.cp", true, "finos-ccc", "ccc.objstor.cp"},
		{"api form on a foreign host is not fetched from the target", "https://hub.example.org/v1/catalogs/acme/baseline", false, "", ""},
		{"versions without a version", "https://grc.store/acme/baseline/versions", false, "", ""},
		{"segments that slugify to nothing", "https://grc.store/---/baseline", false, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns, id, ok, reason := Recognize(tc.url, target)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v (reason %q), want %v", ok, reason, tc.wantOK)
			}
			if ok && (ns != tc.wantNS || id != tc.wantID) {
				t.Errorf("(ns,id) = (%q,%q), want (%q,%q)", ns, id, tc.wantNS, tc.wantID)
			}
		})
	}
}

func TestLint(t *testing.T) {
	body := []byte(`metadata:
  id: my-catalog
  type: ControlCatalog
  lexicon:
    reference-id: lex
  mapping-references:
    - id: ok
      title: Canonical
      version: "1.0"
      url: https://grc.store/acme/baseline
    - id: ext
      title: External standard, any url is fine
      url: https://github.com/ossf/scorecard
    - id: legacy
      title: Legacy search form
      url: https://grc.store/search/finos-aigf/finos-air/versions/0.2.0
    - id: api
      title: API form
      url: https://hub.grc.store/v1/catalogs/Acme/Base_Guidance
    - id: junk
      title: grc.store host but no coordinate
      url: https://grc.store/acme/baseline/extra
    - id: lex
      title: Lexicon with no url
    - id: shelf
      title: Declared, unused, no url
imports:
  - reference-id: ok
`)
	a, err := Scan(body)
	if err != nil {
		t.Fatal(err)
	}
	got := a.Lint()
	want := []string{
		`"legacy" url "https://grc.store/search/finos-aigf/finos-air/versions/0.2.0" resolves, but the canonical form is https://grc.store/finos-aigf/finos-air with version: "0.2.0"`,
		`"api" url "https://hub.grc.store/v1/catalogs/Acme/Base_Guidance" resolves, but the canonical form is https://grc.store/acme/base-guidance`,
		`"junk" url "https://grc.store/acme/baseline/extra" does not name a grc.store artifact`,
		`"lex" is used by lexicon but has no url`,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d warnings, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
	for i, w := range want {
		if !strings.Contains(got[i], w) {
			t.Errorf("warning %d = %q, want it to contain %q", i, got[i], w)
		}
	}
}
