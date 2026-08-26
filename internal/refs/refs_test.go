// SPDX-License-Identifier: Apache-2.0

package refs

import "testing"

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
