// SPDX-License-Identifier: Apache-2.0

package refs

import "testing"

// FuzzScan: Scan takes untrusted YAML straight off disk or a registry and
// must never panic; when it reports success it must hand back an artifact.
// Seeds are the shapes the unit tests already cover plus the degenerate ones
// (empty, scalar metadata, list root). Crashers Go saves under
// testdata/fuzz/FuzzScan/ are regression seeds — commit them.
func FuzzScan(f *testing.F) {
	for _, s := range []string{
		controlCatalogYAML,
		"",
		"metadata: 7",
		"[1, 2]",
		"metadata:\n  id: x\n  version: v1\n",
		"metadata:\n  mapping-references:\n    - id: a\n      title: A\n      version: v1\n",
		"metadata:\n  type: Policy\nimports: [{reference-id: a}]\n",
		"metadata: {id: x}\nmapping-references: {}\n",
		"\xff\xfe",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		a, err := Scan(body)
		if err == nil && a == nil {
			t.Fatal("Scan returned a nil artifact without an error")
		}
	})
}
