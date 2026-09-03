// SPDX-License-Identifier: Apache-2.0

package sign

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestInTotoStatement pins the DSSE payload structure the in-process signer
// emits: an in-toto Statement v1 whose single subject digest is the
// manifest digest, with cosign's predicateType and empty `{}` (not null)
// annotations/predicate. The sign→verify round-trip in internal/sigverify proves
// this exact structure verifies; this pins the structure itself.
func TestInTotoStatement(t *testing.T) {
	hexDigest := strings.Repeat("ab", 32) // 64 hex chars
	b, err := InTotoStatement("sha256:" + hexDigest)
	if err != nil {
		t.Fatalf("InTotoStatement: %v", err)
	}
	var stmt struct {
		Type    string `json:"_type"`
		Subject []struct {
			Digest      map[string]string `json:"digest"`
			Annotations json.RawMessage   `json:"annotations"`
		} `json:"subject"`
		PredicateType string          `json:"predicateType"`
		Predicate     json.RawMessage `json:"predicate"`
	}
	if err := json.Unmarshal(b, &stmt); err != nil {
		t.Fatalf("statement is not valid JSON: %v\n%s", err, b)
	}
	if stmt.Type != "https://in-toto.io/Statement/v1" {
		t.Errorf("_type = %q", stmt.Type)
	}
	if len(stmt.Subject) != 1 || stmt.Subject[0].Digest["sha256"] != hexDigest {
		t.Errorf("subject digest = %+v, want sha256=%s", stmt.Subject, hexDigest)
	}
	if stmt.PredicateType != cosignSignPredicateType {
		t.Errorf("predicateType = %q", stmt.PredicateType)
	}
	// Empty objects, not null — cosign compatibility.
	if string(stmt.Predicate) != "{}" {
		t.Errorf("predicate = %s, want {}", stmt.Predicate)
	}
	if string(stmt.Subject[0].Annotations) != "{}" {
		t.Errorf("annotations = %s, want {}", stmt.Subject[0].Annotations)
	}
}

func TestInTotoStatement_RejectsBadDigest(t *testing.T) {
	for _, bad := range []string{
		"",
		"not-a-digest",
		"sha256:tooshort",
		"sha256:" + strings.Repeat("zz", 32), // right length, non-hex
		"sha512:" + strings.Repeat("ab", 32), // wrong algorithm prefix (not stripped)
	} {
		if _, err := InTotoStatement(bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
	// A bare (prefix-less) 64-hex digest is accepted — the prefix is optional.
	if _, err := InTotoStatement(strings.Repeat("ab", 32)); err != nil {
		t.Errorf("bare 64-hex digest should be accepted, got %v", err)
	}
}
