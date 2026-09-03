// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"testing"

	"github.com/spf13/viper"
)

// TestPlanUnpackVerify pins the pre-network gating decision: remote
// unpack verifies by default, --no-verify opts out, and a local --source layout
// can never be verified (and that reason wins even when --no-verify is also set).
func TestPlanUnpackVerify(t *testing.T) {
	cases := []struct {
		name     string
		source   string
		noVerify bool
		want     unpackVerifyPlan
	}{
		{"remote default verifies", "", false, unpackVerify},
		{"no-verify opts out", "", true, unpackSkipNoVerify},
		{"source cannot be verified", "./layout", false, unpackSkipSource},
		{"source wins over no-verify", "./layout", true, unpackSkipSource},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := planUnpackVerify(tc.source, tc.noVerify); got != tc.want {
				t.Fatalf("planUnpackVerify(%q, %v) = %d, want %d", tc.source, tc.noVerify, got, tc.want)
			}
		})
	}
}

// TestUnpackReusesVerifyTrustFlags confirms unpack registers the keyless trust
// flags it shares with `grcli verify`, so an unpack can assert its own identity
// (bypassing the hub) exactly as verify does. A missing flag here would make
// verifyBeforeUnpack silently fall back to zero-flag mode and ignore the
// caller's asserted identity.
func TestUnpackReusesVerifyTrustFlags(t *testing.T) {
	cmd := newUnpackCmd(viper.New())
	for _, name := range []string{flagNoVerify, flagCertIdentity, flagCertOIDCIssuer} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("unpack is missing the --%s flag", name)
		}
	}
}
