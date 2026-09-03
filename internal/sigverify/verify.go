// SPDX-License-Identifier: Apache-2.0

// Package sigverify is grcli's in-process Sigstore keyless-verification
// substrate. It is a MIRROR of the hub's internal/sigverify — not an
// import: the backend's internals
// aren't importable, and the zero-dependency grc-store-protocol rightly
// excludes sigstore-go. Keeping the two in lockstep means "verifies on the hub
// but not in grcli" (or vice versa) can only come from policy intent, never
// implementation drift.
//
// The verifier is PURE crypto: it does no registry I/O. Callers hand it the raw
// Sigstore bundle bytes (discovered as an OCI referrer of the artifact by the
// fetch layer, internal/registry) and the artifact's digest; it verifies the
// signature against a pinned trust root AND against an expected signer identity.
//
// The ONE deliberate divergence from the hub: the hub uses
// WithoutIdentitiesUnsafe because it TOFU-pins (first publish accepts any valid
// keyless cert, then the handler pins the extracted identity per coordinate).
// grcli is the consumer — it already KNOWS the identity to expect (from the
// --certificate-identity flag or the hub's recorded record) — so it
// pins the SAN + issuer IN the sigstore-go policy. The cryptographic floor is
// identical; grcli additionally enforces WHO signed.
package sigverify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gemaraproj/grc-store-clientkit/trustroot"
	"github.com/revanite-io/grc-store-protocol/identity"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// Result is what a successful Verify yields: the canonical, scheme-prefixed
// signer identity recovered from the verified certificate. It matches the hub's
// Result.Identity byte-for-byte (grc-store-protocol/identity) so a
// post-verify confirmation line names the same identity the hub recorded.
type Result struct {
	// Identity is the canonical keyless identity
	// ("keyless:<issuer>#<workflow-path>", @refs stripped) recovered from the
	// verified Fulcio certificate.
	Identity string
}

// ErrUnsigned is returned when the artifact has no signature bundle (the fetch
// layer found no referrer). It is distinct from a present-but-invalid signature
// so cmd/verify.go can print an "artifact is not signed" message rather than a
// crypto failure.
var ErrUnsigned = errors.New("artifact is not signed")

// embeddedTrustedRoot is the pinned public-good Sigstore trust root — the SAME
// material the hub pins, which is exactly why it is no
// longer vendored here: grcli, privateer-sdk and the hub each carried a
// byte-identical copy, so rotation was three edits and three chances to miss
// one. It now comes from grc-store-clientkit, and refreshing it is one release
// of that module. Pinning (rather than fetching live via TUF) still keeps
// verify offline and deterministic, adding no network failure mode. An override
// for air-gapped / private-Sigstore deployments is NewVerifierFromFile.
var embeddedTrustedRoot = trustroot.Bytes()

// defaultVerifyTimeout bounds the (offline, CPU-only) verification. Defense in
// depth: the work is local crypto, but a malformed bundle should never hang.
const defaultVerifyTimeout = 15 * time.Second

// Verifier performs in-process keyless verification of an artifact signature
// using sigstore-go against a pinned trust root, enforcing an expected identity.
type Verifier struct {
	verifier *verify.Verifier
	timeout  time.Duration
	// requireSCT records whether this verifier enforces SCTs. Production
	// (NewVerifier / NewVerifierFromFile) is always true; only the unexported
	// test constructor sets it false, because VirtualSigstore certs carry no
	// embedded SCT (see verify_test.go). Kept as a field purely so tests can
	// assert the prod path never relaxes it.
	requireSCT bool
}

// IdentityPolicy pins the expected keyless signer. It mirrors, one-for-one, the
// certificate-identity material cmd/verify.go previously handed to cosign:
//
//   - Explicit --certificate-identity mode: SAN set (exact), SANRegexp empty.
//   - Hub-lookup mode: SANRegexp set (the anchored "^QuoteMeta(path)@"
//     pattern), SAN empty. The ref-stripped pin admits any git ref but nothing
//     wider than the exact workflow path.
//
// Issuer is ALWAYS the exact expected OIDC issuer. Exactly one of SAN / SANRegexp
// must be non-empty; supplying neither is a programming error (rejected at build).
type IdentityPolicy struct {
	SAN       string // exact SAN (explicit --certificate-identity mode)
	SANRegexp string // anchored SAN regexp (hub-lookup mode)
	Issuer    string // exact OIDC issuer
}

// certificateIdentity turns the pinned IdentityPolicy into a sigstore-go
// certificate-identity matcher. The mapping is the security-critical seam that
// replaces cosign's flags:
//
//	explicit:   --certificate-identity <SAN>   --certificate-oidc-issuer <ISS>
//	            → SAN exact-match,  issuer exact-match
//	hub-lookup: --certificate-identity-regexp <RE> --certificate-oidc-issuer <ISS>
//	            → SAN regexp-match, issuer exact-match
//
// NewShortCertificateIdentity(issuer, issuerRegex, sanValue, sanRegex): we pass
// issuerRegex="" (exact issuer) always, and set exactly one of sanValue /
// sanRegex. sigstore-go's SubjectAlternativeNameMatcher does a byte-exact string
// compare for sanValue and an unanchored regexp match for sanRegex — the same
// semantics cosign's two flags have, so the anchoring/escaping the caller built
// (regexp.QuoteMeta + '^...@') carries through unchanged.
func (ip IdentityPolicy) certificateIdentity() (verify.CertificateIdentity, error) {
	if ip.Issuer == "" {
		return verify.CertificateIdentity{}, errors.New("identity policy has no issuer")
	}
	if (ip.SAN == "") == (ip.SANRegexp == "") {
		return verify.CertificateIdentity{}, errors.New("identity policy must set exactly one of SAN or SANRegexp")
	}
	return verify.NewShortCertificateIdentity(ip.Issuer, "", ip.SAN, ip.SANRegexp)
}

// NewVerifier builds a verifier over the embedded pinned trust root. It requires,
// for keyless GitHub Actions signatures, an SCT (Fulcio), a transparency-log
// entry (Rekor), and at least one observed timestamp — the production posture,
// identical to the hub's NewSigstoreVerifier.
func NewVerifier(timeout time.Duration) (*Verifier, error) {
	return newVerifierFromRoot(embeddedTrustedRoot, "embedded", timeout)
}

// NewVerifierFromFile builds a verifier over a trusted_root.json read from disk
// instead of the embedded public-good root (GRCLI_TRUSTED_ROOT). It serves air-gapped deployments and private Sigstore instances —
// the same posture as the hub's NewSigstoreVerifierFromFile. The SCT/Rekor/
// timestamp policy is UNCHANGED (a private Sigstore still runs a CT log); only
// the set of trusted CAs/logs differs. An empty path is a programming error
// (callers gate on the config value being set), so it errors rather than
// silently falling back to the embedded root and masking a misconfiguration.
func NewVerifierFromFile(path string, timeout time.Duration) (*Verifier, error) {
	if path == "" {
		return nil, errors.New("trusted root file path is empty")
	}
	rootJSON, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read trusted root file %q: %w", path, err)
	}
	return newVerifierFromRoot(rootJSON, path, timeout)
}

// newVerifierFromRoot parses a trusted_root.json (from any source) and builds
// the production-posture verifier over it. src is a label for error context
// ("embedded" or a file path). sctThreshold is always 1: an SCT proves the
// Fulcio cert was logged to a CT log, and the public-good root carries the CT
// keys to check it.
func newVerifierFromRoot(rootJSON []byte, src string, timeout time.Duration) (*Verifier, error) {
	tm, err := root.NewTrustedRootFromJSON(rootJSON)
	if err != nil {
		return nil, fmt.Errorf("parse %s trusted root: %w", src, err)
	}
	return newVerifier(tm, timeout, 1)
}

// newVerifier is the test-friendly constructor: it takes any root.TrustedMaterial
// (so unit tests can pass a VirtualSigstore) and an SCT threshold. Tests pass 0
// because VirtualSigstore certs carry no embedded SCT; production always passes 1.
// It is UNEXPORTED so the production API (NewVerifier / NewVerifierFromFile) can
// only ever construct an SCT-requiring verifier.
func newVerifier(tm root.TrustedMaterial, timeout time.Duration, sctThreshold int) (*Verifier, error) {
	// Transparency-log inclusion (Rekor) + at least one observed timestamp is
	// sigstore-go's canonical keyless posture; SCT is additional cert-issuance
	// transparency, required in production.
	opts := []verify.VerifierOption{
		verify.WithTransparencyLog(1),
		verify.WithObserverTimestamps(1),
	}
	if sctThreshold > 0 {
		opts = append(opts, verify.WithSignedCertificateTimestamps(sctThreshold))
	}
	v, err := verify.NewVerifier(tm, opts...)
	if err != nil {
		return nil, fmt.Errorf("build sigstore verifier: %w", err)
	}
	if timeout <= 0 {
		timeout = defaultVerifyTimeout
	}
	return &Verifier{verifier: v, timeout: timeout, requireSCT: sctThreshold > 0}, nil
}

// Verify parses the attached signature bundle and verifies it against the
// artifact digest AND the expected identity. Empty bundle bytes are ErrUnsigned.
// Verification is bounded by the configured timeout (defense in depth; the work
// is offline crypto). It fails CLOSED: any non-nil error means "do not trust".
func (v *Verifier) Verify(ctx context.Context, signatureBundle []byte, artifactDigest string, id IdentityPolicy) (Result, error) {
	if len(signatureBundle) == 0 {
		return Result{}, ErrUnsigned
	}
	certID, err := id.certificateIdentity()
	if err != nil {
		return Result{}, fmt.Errorf("build identity policy: %w", err)
	}
	var b bundle.Bundle
	if err := b.UnmarshalJSON(signatureBundle); err != nil {
		return Result{}, fmt.Errorf("parse signature bundle: %w", err)
	}

	type outcome struct {
		res Result
		err error
	}
	ch := make(chan outcome, 1)
	go func() {
		res, err := v.verifyEntity(&b, artifactDigest, certID)
		ch <- outcome{res: res, err: err}
	}()
	select {
	case <-ctx.Done():
		return Result{}, ctx.Err()
	case <-time.After(v.timeout):
		return Result{}, fmt.Errorf("signature verification timed out after %s", v.timeout)
	case r := <-ch:
		return r.res, r.err
	}
}

// verifyEntity runs the sigstore policy check over a SignedEntity bound to the
// artifact digest AND the pinned certificate identity, then recovers the
// canonical identity from the verified cert. Split out (taking a
// verify.SignedEntity, not bundle bytes) so unit tests can drive it with a
// VirtualSigstore TestEntity and no bundle serialization.
//
// This is where grcli differs from the hub: WithCertificateIdentity(certID)
// replaces the hub's WithoutIdentitiesUnsafe() — the SAN + issuer are pinned at
// verify time, so a valid Sigstore signature by the WRONG identity is rejected.
func (v *Verifier) verifyEntity(entity verify.SignedEntity, artifactDigest string, certID verify.CertificateIdentity) (Result, error) {
	digestBytes, err := hex.DecodeString(strings.TrimPrefix(artifactDigest, "sha256:"))
	if err != nil || len(digestBytes) != sha256.Size {
		return Result{}, fmt.Errorf("invalid artifact digest %q", artifactDigest)
	}
	policy := verify.NewPolicy(
		verify.WithArtifactDigest("sha256", digestBytes),
		verify.WithCertificateIdentity(certID),
	)
	res, err := v.verifier.Verify(entity, policy)
	if err != nil {
		return Result{}, fmt.Errorf("signature verification failed: %w", err)
	}
	if res.Signature == nil || res.Signature.Certificate == nil {
		return Result{}, errors.New("verified signature carries no certificate identity (key-based signing is not accepted on the keyless path)")
	}
	cert := res.Signature.Certificate
	if cert.Issuer == "" || cert.SubjectAlternativeName == "" {
		return Result{}, errors.New("verified certificate is missing OIDC issuer or SAN")
	}
	// The canonical signer identity comes from the shared wire-contract module
	// — the SAME definition the hub uses — so grcli's confirmation
	// names the identity in exactly the form the hub recorded.
	return Result{
		Identity: identity.CanonicalKeylessIdentity(cert.Issuer, cert.SubjectAlternativeName),
	}, nil
}
