// SPDX-License-Identifier: Apache-2.0

package sigverify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sigstore/sigstore-go/pkg/testing/ca"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/stretchr/testify/require"
)

// digestOf returns the "sha256:<hex>" coordinate for artifact bytes — the same
// value the fetch layer produces and that the policy binds to.
func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// newTestVerifier builds a Verifier whose trust root IS the virtual sigstore, so
// verification is fully offline and deterministic. sctThreshold=0: VirtualSigstore
// certs carry no embedded SCT. Production (NewVerifier / NewVerifierFromFile)
// requires one — see TestProductionVerifierRequiresSCT.
func newTestVerifier(t *testing.T, vs *ca.VirtualSigstore) *Verifier {
	t.Helper()
	v, err := newVerifier(vs, 5*time.Second, 0)
	require.NoError(t, err)
	require.False(t, v.requireSCT, "test verifier must NOT require SCTs (VirtualSigstore carries none)")
	return v
}

const (
	ghaIssuer = "https://token.actions.githubusercontent.com"
	// The pinned workflow path (ref-stripped) and a concrete signing ref of it.
	workflowPath = "https://github.com/finos/ccc-evaluator/.github/workflows/release.yml"
	ghaSANRef    = workflowPath + "@refs/tags/v1.2.0"
)

// mustCertID builds the sigstore-go matcher from an IdentityPolicy, failing the
// test on a malformed policy. It exercises the exact production path
// (IdentityPolicy.certificateIdentity) rather than a hand-rolled matcher.
func mustCertID(t *testing.T, ip IdentityPolicy) verify.CertificateIdentity {
	t.Helper()
	certID, err := ip.certificateIdentity()
	require.NoError(t, err)
	return certID
}

// explicitPolicy pins the exact SAN (with ref) + exact issuer — the
// --certificate-identity mode.
func explicitPolicy(san string) IdentityPolicy {
	return IdentityPolicy{SAN: san, Issuer: ghaIssuer}
}

// hubLookupPolicy pins the anchored SAN regexp + exact issuer — the zero-flag
// hub-lookup mode. The pattern is byte-identical to what cmd/verify.go's
// resolveHubIdentity builds ("^" + QuoteMeta(path) + "@").
func hubLookupPolicy(anchoredRegexp string) IdentityPolicy {
	return IdentityPolicy{SANRegexp: anchoredRegexp, Issuer: ghaIssuer}
}

func TestVerifyEntity_AcceptsMatchingExactIdentity(t *testing.T) {
	vs, err := ca.NewVirtualSigstore()
	require.NoError(t, err)
	artifact := []byte("the-artifact-manifest-bytes")
	entity, err := vs.Sign(ghaSANRef, ghaIssuer, artifact)
	require.NoError(t, err)

	v := newTestVerifier(t, vs)
	res, err := v.verifyEntity(entity, digestOf(artifact), mustCertID(t, explicitPolicy(ghaSANRef)))
	require.NoError(t, err)
	// Result identity is canonical (ref-stripped), matching the hub's record.
	require.Equal(t, "keyless:"+ghaIssuer+"#"+workflowPath, res.Identity)
}

func TestVerifyEntity_RejectsWrongDigest(t *testing.T) {
	vs, err := ca.NewVirtualSigstore()
	require.NoError(t, err)
	entity, err := vs.Sign(ghaSANRef, ghaIssuer, []byte("artifact-A"))
	require.NoError(t, err)

	v := newTestVerifier(t, vs)
	_, err = v.verifyEntity(entity, digestOf([]byte("artifact-B-tampered")), mustCertID(t, explicitPolicy(ghaSANRef)))
	require.Error(t, err)
}

func TestVerifyEntity_RejectsWrongIdentity(t *testing.T) {
	vs, err := ca.NewVirtualSigstore()
	require.NoError(t, err)
	artifact := []byte("artifact")
	entity, err := vs.Sign(ghaSANRef, ghaIssuer, artifact)
	require.NoError(t, err)

	v := newTestVerifier(t, vs)
	// A valid signature by a DIFFERENT workflow SAN must be rejected — this is
	// the whole point of pinning identity (grcli, unlike the TOFU hub, knows
	// who it expects).
	otherSAN := "https://github.com/evil/repo/.github/workflows/release.yml@refs/tags/v1.2.0"
	_, err = v.verifyEntity(entity, digestOf(artifact), mustCertID(t, explicitPolicy(otherSAN)))
	require.Error(t, err)
}

func TestVerifyEntity_RejectsWrongIssuer(t *testing.T) {
	vs, err := ca.NewVirtualSigstore()
	require.NoError(t, err)
	artifact := []byte("artifact")
	entity, err := vs.Sign(ghaSANRef, ghaIssuer, artifact)
	require.NoError(t, err)

	v := newTestVerifier(t, vs)
	wrongIssuer := IdentityPolicy{SAN: ghaSANRef, Issuer: "https://gitlab.example.com"}
	_, err = v.verifyEntity(entity, digestOf(artifact), mustCertID(t, wrongIssuer))
	require.Error(t, err)
}

func TestVerifyEntity_RejectsForeignTrustRoot(t *testing.T) {
	signer, err := ca.NewVirtualSigstore()
	require.NoError(t, err)
	artifact := []byte("artifact")
	entity, err := signer.Sign(ghaSANRef, ghaIssuer, artifact)
	require.NoError(t, err)

	otherRoot, err := ca.NewVirtualSigstore()
	require.NoError(t, err)
	v := newTestVerifier(t, otherRoot)

	_, err = v.verifyEntity(entity, digestOf(artifact), mustCertID(t, explicitPolicy(ghaSANRef)))
	require.Error(t, err)
}

func TestVerifyEntity_RejectsBadDigestFormat(t *testing.T) {
	vs, err := ca.NewVirtualSigstore()
	require.NoError(t, err)
	entity, err := vs.Sign(ghaSANRef, ghaIssuer, []byte("x"))
	require.NoError(t, err)
	v := newTestVerifier(t, vs)
	_, err = v.verifyEntity(entity, "not-a-sha256-digest", mustCertID(t, explicitPolicy(ghaSANRef)))
	require.Error(t, err)
}

// TestVerifyEntity_HubLookupRegexp_Adversarial reuses the ADR-0045 adversarial
// SAN cases, now asserted against the IN-PROCESS matcher (a real
// VirtualSigstore-signed cert carrying each SAN) rather than a cosign arg
// string. The anchored "^QuoteMeta(path)@" pin must admit ANY ref of the exact
// workflow while refusing a prefixed, sibling, or look-alike identity.
func TestVerifyEntity_HubLookupRegexp_Adversarial(t *testing.T) {
	// Build the pin exactly as cmd/verify.go's resolveHubIdentity does. A '.'
	// in the path is a regexp metacharacter, so escaping is load-bearing.
	const dottedPath = "https://github.com/acme/repo.name/.github/workflows/publish.yml"
	// regexp.QuoteMeta is what production uses; replicate its output here.
	anchored := `^https://github\.com/acme/repo\.name/\.github/workflows/publish\.yml@`
	pol := hubLookupPolicy(anchored)

	cases := []struct {
		name   string
		san    string
		accept bool
	}{
		{"tag ref of the pinned workflow", dottedPath + "@refs/tags/v1.0.0", true},
		{"branch ref of the pinned workflow", dottedPath + "@refs/heads/main", true},
		{"prefixed identity rejected by the ^ anchor", "https://evil.example/" + dottedPath + "@refs/tags/v1", false},
		{"longer sibling path rejected by the @ boundary", dottedPath + "-sibling/.github/workflows/publish.yml@refs/tags/v1", false},
		{"escaped '.' is a literal, not a wildcard", "https://github.com/acme/repoXname/.github/workflows/publish.yml@refs/tags/v1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vs, err := ca.NewVirtualSigstore()
			require.NoError(t, err)
			artifact := []byte("artifact-" + tc.name)
			entity, err := vs.Sign(tc.san, ghaIssuer, artifact)
			require.NoError(t, err)

			v := newTestVerifier(t, vs)
			_, err = v.verifyEntity(entity, digestOf(artifact), mustCertID(t, pol))
			if tc.accept {
				require.NoError(t, err, "SAN %q must verify against the pinned workflow", tc.san)
			} else {
				require.Error(t, err, "SAN %q must be rejected by the anchored pin", tc.san)
			}
		})
	}
}

// TestVerify_Unsigned confirms empty bundle bytes → ErrUnsigned (the fetch layer
// found no referrer), distinct from a present-but-invalid signature.
func TestVerify_Unsigned(t *testing.T) {
	vs, err := ca.NewVirtualSigstore()
	require.NoError(t, err)
	v := newTestVerifier(t, vs)
	_, err = v.Verify(context.Background(), nil, digestOf([]byte("x")), explicitPolicy(ghaSANRef))
	require.ErrorIs(t, err, ErrUnsigned)
}

// TestVerify_MalformedBundle confirms garbage bundle bytes are a hard error, NOT
// ErrUnsigned — a present-but-unparseable signature must fail closed, never be
// treated as "no signature".
func TestVerify_MalformedBundle(t *testing.T) {
	vs, err := ca.NewVirtualSigstore()
	require.NoError(t, err)
	v := newTestVerifier(t, vs)
	_, err = v.Verify(context.Background(), []byte("{not a valid sigstore bundle}"), digestOf([]byte("x")), explicitPolicy(ghaSANRef))
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrUnsigned)
}

// TestVerify_MalformedIdentityPolicy confirms an unusable identity pin fails the
// public Verify path loudly (a programming error), NOT as ErrUnsigned. The
// identity policy is validated before the bundle is parsed, so non-empty bytes
// with a bad policy exercise the guard without needing a real bundle. (The crypto
// + identity match is covered end-to-end against real signed entities by the
// verifyEntity tests above; the ca.TestEntity has no bundle-JSON serializer, so
// the wrapper is exercised via nil/garbage bytes here, as the hub's tests do.)
func TestVerify_MalformedIdentityPolicy(t *testing.T) {
	vs, err := ca.NewVirtualSigstore()
	require.NoError(t, err)
	v := newTestVerifier(t, vs)
	// Neither SAN nor SANRegexp set → invalid policy; the non-empty bytes get
	// past the ErrUnsigned check so we reach the policy guard.
	_, err = v.Verify(context.Background(), []byte("{}"), digestOf([]byte("x")), IdentityPolicy{Issuer: ghaIssuer})
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrUnsigned)
}

// TestIdentityPolicy_Validation pins the exactly-one-of-SAN/SANRegexp invariant
// and the required issuer — the guardrails that stop a policy from silently
// matching nothing (or everything).
func TestIdentityPolicy_Validation(t *testing.T) {
	cases := []struct {
		name string
		ip   IdentityPolicy
		ok   bool
	}{
		{"exact SAN + issuer", IdentityPolicy{SAN: ghaSANRef, Issuer: ghaIssuer}, true},
		{"SAN regexp + issuer", IdentityPolicy{SANRegexp: "^" + workflowPath + "@", Issuer: ghaIssuer}, true},
		{"no issuer", IdentityPolicy{SAN: ghaSANRef}, false},
		{"neither SAN nor regexp", IdentityPolicy{Issuer: ghaIssuer}, false},
		{"both SAN and regexp", IdentityPolicy{SAN: ghaSANRef, SANRegexp: "^x@", Issuer: ghaIssuer}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.ip.certificateIdentity()
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

// TestProductionVerifierRequiresSCT guards the load-bearing invariant that the
// PRODUCTION constructors always demand an SCT — the test knob (sctThreshold=0)
// must never leak into the exported API. We can't feed a VirtualSigstore bundle
// through the SCT-requiring verifier (its certs carry no SCT), so we assert the
// posture flag the constructors set instead.
func TestProductionVerifierRequiresSCT(t *testing.T) {
	v, err := NewVerifier(5 * time.Second)
	require.NoError(t, err)
	require.True(t, v.requireSCT, "NewVerifier must require SCTs in production")
}

// TestEmbeddedTrustRootParses guards against a corrupt/empty trusted_root.json
// embed: the pinned production root must parse into a usable verifier.
func TestEmbeddedTrustRootParses(t *testing.T) {
	v, err := NewVerifier(5 * time.Second)
	require.NoError(t, err)
	require.NotNil(t, v)
}

// TestNewVerifierFromFile_ReadsRoot round-trips the embedded root through a temp
// file (GRCLI_TRUSTED_ROOT), proving the file-read + parse path is equivalent to
// the embedded path without needing a live private sigstore.
func TestNewVerifierFromFile_ReadsRoot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trusted_root.json")
	require.NoError(t, os.WriteFile(path, embeddedTrustedRoot, 0o600))

	v, err := NewVerifierFromFile(path, 5*time.Second)
	require.NoError(t, err)
	require.NotNil(t, v)
	require.True(t, v.requireSCT, "the file-override path must keep the production SCT posture")
}

// TestNewVerifierFromFile_FailsClosed confirms an empty path (programming error)
// and a missing file both fail — never silently fall back to the embedded root,
// which would mask a misconfigured override.
func TestNewVerifierFromFile_FailsClosed(t *testing.T) {
	_, err := NewVerifierFromFile("", 5*time.Second)
	require.Error(t, err)

	_, err = NewVerifierFromFile(filepath.Join(t.TempDir(), "does-not-exist.json"), 5*time.Second)
	require.Error(t, err)
}
