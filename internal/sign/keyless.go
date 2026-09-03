// SPDX-License-Identifier: Apache-2.0

package sign

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	sgbundle "github.com/sigstore/sigstore-go/pkg/bundle"
	sgsign "github.com/sigstore/sigstore-go/pkg/sign"
)

// In-process keyless signing. This is the symmetric half of the
// in-process VERIFY path (internal/sigverify): grcli signs the
// pushed artifact with a short-lived Fulcio certificate obtained via the
// runner's OIDC token, logs it in Rekor, and produces a Sigstore v0.3 bundle —
// all with the sigstore-go library grcli already depends on for verification,
// so publishing no longer requires cosign on PATH.
//
// The signature is a DSSE-wrapped in-toto Statement whose single subject digest
// is the artifact's manifest digest (payloadType application/vnd.in-toto+json,
// predicateType https://sigstore.dev/cosign/sign/v1) — byte-shaped to match what
// `cosign sign --new-bundle-format` produces, so the on-registry format stays
// uniform and the hub's verifier (which mirrors internal/sigverify and checks
// the subject via WithArtifactDigest) accepts it unchanged.

const (
	// sigstoreOIDCAudience is the audience Fulcio requires on the OIDC token.
	sigstoreOIDCAudience = "sigstore"

	// inTotoPayloadType is the DSSE payloadType cosign uses for a container
	// signature in the new bundle format; the verifier keys subject extraction
	// on the in-toto statement shape, not this exact string, but matching it
	// keeps grcli's output indistinguishable from cosign's.
	inTotoPayloadType = "application/vnd.in-toto+json"

	// cosignSignPredicateType is the predicateType on that statement.
	cosignSignPredicateType = "https://sigstore.dev/cosign/sign/v1"

	defaultFulcioURL = "https://fulcio.sigstore.dev"
	defaultRekorURL  = "https://rekor.sigstore.dev"
)

// fulcioURL / rekorURL resolve the signing endpoints, honoring env overrides for
// a private Sigstore / air-gapped deployment (mirrors GRCLI_TRUSTED_ROOT on the
// verify side). Empty env → the public-good instances.
func fulcioURL() string {
	if u := os.Getenv("GRCLI_FULCIO_URL"); u != "" {
		return u
	}
	return defaultFulcioURL
}

func rekorURL() string {
	if u := os.Getenv("GRCLI_REKOR_URL"); u != "" {
		return u
	}
	return defaultRekorURL
}

// InTotoStatement builds the DSSE payload: an in-toto Statement v1 whose lone
// subject carries the artifact's manifest digest. Maps (not structs) are used so
// the empty `annotations` and `predicate` objects marshal as `{}` rather than
// `null`, matching cosign. Key order is irrelevant — the verifier re-parses.
// Exported so the sign→verify round-trip test (internal/sigverify) can prove the
// exact payload this signer emits verifies against the verifier's subject check.
func InTotoStatement(manifestDigest string) ([]byte, error) {
	hexDigest := strings.TrimPrefix(manifestDigest, "sha256:")
	if raw, err := hex.DecodeString(hexDigest); err != nil || len(raw) != sha256.Size {
		return nil, fmt.Errorf("expected a sha256 manifest digest, got %q", manifestDigest)
	}
	stmt := map[string]any{
		"_type": "https://in-toto.io/Statement/v1",
		"subject": []map[string]any{{
			"digest":      map[string]string{"sha256": hexDigest},
			"annotations": map[string]any{},
		}},
		"predicateType": cosignSignPredicateType,
		"predicate":     map[string]any{},
	}
	return json.Marshal(stmt)
}

// signKeylessInProcess produces a Sigstore v0.3 bundle (JSON bytes) for the
// given manifest digest, keyless, via sigstore-go — no cosign. It obtains the
// OIDC token from the GitHub Actions runtime (the only keyless publish path),
// requests a Fulcio cert, signs the in-toto statement, and logs it in Rekor.
func signKeylessInProcess(ctx context.Context, manifestDigest string) ([]byte, error) {
	token, err := githubOIDCToken(ctx, sigstoreOIDCAudience)
	if err != nil {
		return nil, err
	}
	statement, err := InTotoStatement(manifestDigest)
	if err != nil {
		return nil, err
	}
	keypair, err := sgsign.NewEphemeralKeypair(nil)
	if err != nil {
		return nil, fmt.Errorf("generating ephemeral keypair: %w", err)
	}
	content := &sgsign.DSSEData{Data: statement, PayloadType: inTotoPayloadType}
	opts := sgsign.BundleOptions{
		CertificateProvider:        sgsign.NewFulcio(&sgsign.FulcioOptions{BaseURL: fulcioURL(), Timeout: 30 * time.Second, Retries: 2}),
		CertificateProviderOptions: &sgsign.CertificateProviderOptions{IDToken: token},
		TransparencyLogs:           []sgsign.Transparency{sgsign.NewRekor(&sgsign.RekorOptions{BaseURL: rekorURL(), Timeout: 60 * time.Second, Retries: 2})},
		Context:                    ctx,
	}
	pb, err := sgsign.Bundle(content, keypair, opts)
	if err != nil {
		return nil, fmt.Errorf("sigstore keyless sign: %w", err)
	}
	b, err := sgbundle.NewBundle(pb)
	if err != nil {
		return nil, fmt.Errorf("assembling signature bundle: %w", err)
	}
	out, err := b.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("serializing signature bundle: %w", err)
	}
	return out, nil
}

// githubOIDCToken requests an OIDC ID token from the GitHub Actions token
// service for the given audience. Requires `permissions: id-token: write` on the
// workflow (which populates ACTIONS_ID_TOKEN_REQUEST_URL / _TOKEN). This is the
// token cosign used to read implicitly; grcli now requests it directly.
func githubOIDCToken(ctx context.Context, audience string) (string, error) {
	reqURL := os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL")
	reqToken := os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN")
	if reqURL == "" || reqToken == "" {
		return "", errors.New("GitHub Actions OIDC token unavailable " +
			"(ACTIONS_ID_TOKEN_REQUEST_URL / _TOKEN unset) — add `permissions: id-token: write` " +
			"to the workflow for keyless signing, or pass --no-sign")
	}
	u := reqURL + "&audience=" + url.QueryEscape(audience)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", fmt.Errorf("building OIDC token request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+reqToken)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("requesting GitHub Actions OIDC token: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub Actions OIDC token endpoint returned %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	var out struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Value == "" {
		return "", errors.New("GitHub Actions OIDC token response had no `value`")
	}
	return out.Value, nil
}
