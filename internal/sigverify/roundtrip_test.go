// SPDX-License-Identifier: Apache-2.0

package sigverify

import (
	"testing"

	"github.com/sigstore/sigstore-go/pkg/testing/ca"
	"github.com/stretchr/testify/require"

	"github.com/revanite-io/grcli/internal/sign"
)

// TestVerifyEntity_AcceptsInTotoDSSE is the ADR-0049 sign→verify round-trip: it
// proves the EXACT in-toto DSSE payload grcli's in-process signer emits
// (sign.InTotoStatement) verifies against this verifier's WithArtifactDigest
// subject check. Since the hub mirrors internal/sigverify, a bundle grcli
// produces in-process is accepted by both — so dropping cosign does not change
// what the registry considers verifiable.
func TestVerifyEntity_AcceptsInTotoDSSE(t *testing.T) {
	vs, err := ca.NewVirtualSigstore()
	require.NoError(t, err)
	v := newTestVerifier(t, vs)

	// The digest carried in the statement's subject == what the verifier is
	// asked to confirm (the pushed manifest's digest).
	manifestDigest := digestOf([]byte("the-pushed-manifest-bytes"))
	statement, err := sign.InTotoStatement(manifestDigest)
	require.NoError(t, err)

	entity, err := vs.Attest(ghaSANRef, ghaIssuer, statement)
	require.NoError(t, err)

	res, err := v.verifyEntity(entity, manifestDigest, mustCertID(t, explicitPolicy(ghaSANRef)))
	require.NoError(t, err, "grcli's in-toto DSSE payload must verify against WithArtifactDigest")
	require.Equal(t, "keyless:"+ghaIssuer+"#"+workflowPath, res.Identity)
}

// A statement whose subject is a DIFFERENT digest must be rejected: the
// signature is cryptographically valid but attests a different artifact. This
// pins that WithArtifactDigest actually binds the subject, not just the cert.
func TestVerifyEntity_RejectsInTotoWrongSubject(t *testing.T) {
	vs, err := ca.NewVirtualSigstore()
	require.NoError(t, err)
	v := newTestVerifier(t, vs)

	statement, err := sign.InTotoStatement(digestOf([]byte("artifact-A")))
	require.NoError(t, err)
	entity, err := vs.Attest(ghaSANRef, ghaIssuer, statement)
	require.NoError(t, err)

	_, err = v.verifyEntity(entity, digestOf([]byte("artifact-B-different")), mustCertID(t, explicitPolicy(ghaSANRef)))
	require.Error(t, err, "a signature attesting a different subject digest must not verify")
}
