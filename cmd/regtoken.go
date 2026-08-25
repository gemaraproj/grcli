// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"os"

	"github.com/revanite-io/grcli/internal/hub"
)

// ensureRegistryToken makes grcli authenticate to the bearer-auth
// registry (ADR-0031) without the caller managing registry credentials:
// it fetches a repository-scoped Distribution token from the hub's
// /v2/token endpoint and exports it as GRCLI_REGISTRY_TOKEN, which both
// the oras push/pull path (internal/registry.dockerCredentials) and the
// cosign subprocess (internal/sign.registryCredArgs) already read.
//
// It is a no-op — returning whatever the user supplied — when:
//   - a registry credential is already set explicitly (GRCLI_REGISTRY_TOKEN
//     or the GRCLI_REGISTRY_USERNAME/PASSWORD pair, or a `docker login`
//     the caller wants honored); manual overrides win, and
//   - there is no hub base URL to ask (--url explicitly cleared), in which
//     case grcli falls back to the Docker credential chain.
//
// bearer is the hub (Keycloak) access token from `grcli login`; pass ""
// for an anonymous pull token. The returned token is also handed back so
// callers that must pass it as an explicit flag (cosign verify) can.
//
// Note: the exported token is scoped to one repository. `unpack --with-*`
// resolves references from other repositories in the same invocation and mints
// a fresh per-repo token for each via mintRefPullToken (which deliberately does
// NOT reuse the already-exported token, since it is scoped to a different repo);
// this function still governs the primary artifact and the user-override rules.
func ensureRegistryToken(ctx context.Context, hubBaseURL, bearer, repository string, actions []string) (string, error) {
	if tok := os.Getenv("GRCLI_REGISTRY_TOKEN"); tok != "" {
		return tok, nil
	}
	if os.Getenv("GRCLI_REGISTRY_USERNAME") != "" && os.Getenv("GRCLI_REGISTRY_PASSWORD") != "" {
		return "", nil // explicit basic-auth override; leave the chain alone
	}
	if hubBaseURL == "" {
		return "", nil // no hub to ask; fall back to the Docker credential chain
	}

	tok, err := hub.FetchRegistryToken(ctx, hubBaseURL, bearer, repository, actions)
	if err != nil {
		return "", err
	}
	if tok != "" {
		_ = os.Setenv("GRCLI_REGISTRY_TOKEN", tok)
	}
	return tok, nil
}
