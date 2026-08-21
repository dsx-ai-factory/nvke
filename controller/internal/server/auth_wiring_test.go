// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server_test

// Where authentication sits in the chain, tested through the real NewServer.
// internal/auth's own tests drive a bare handler and prove nothing about the
// wiring. Routes are driven rather than read, because http.ServeMux is not
// enumerable and the generated router types BaseRouter as a concrete
// *http.ServeMux.

import (
	"bytes"
	"context"
	"crypto"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/api"
	"github.com/NVIDIA/nke/controller/internal/auth"
	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/providers/forge"
	"github.com/NVIDIA/nke/controller/internal/server"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

// errNoKeys is what the stub key source returns. No key is ever asked for; the
// source exists because NewVerifier requires one per issuer.
var errNoKeys = errors.New("no keys in this test")

type stubKeySource struct{}

func (stubKeySource) KeyFor(context.Context, string) (crypto.PublicKey, error) {
	return nil, errNoKeys
}

// newEnforcingTestServer builds the production handler, enforcing.
func newEnforcingTestServer(t *testing.T) http.Handler {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Config{
		FWEAPIKey:            "test-key",
		ManagedByK8sPlatform: false,
		ProfilesDir:          "./profiles",
	}

	authConfig := &auth.Config{
		Audiences: []string{"https://cwe.test.invalid"},
		Issuers: []auth.Issuer{{
			Name:       "test-kubernetes",
			Issuer:     "https://kubernetes.default.svc.cluster.local",
			JWKS:       &auth.JWKSKeys{URI: "https://kubernetes.default.svc/openid/v1/jwks"},
			Algorithms: []string{"RS256"},
			Admit: &auth.Admit{
				ServiceAccounts: []auth.ServiceAccountRule{{Namespace: "nke-clients"}},
			},
		}},
	}
	verifier, err := auth.NewVerifier(authConfig, map[string]auth.KeySource{"test-kubernetes": stubKeySource{}})
	require.NoError(t, err)

	authn, err := auth.NewAuthenticator(verifier, auth.ModeEnforcing, logger, nil)
	require.NoError(t, err)

	mockDB := &testutil.MockDB{}
	forgeProvider := forge.NewForgeProvider(cfg, mockDB, logger)
	srv, err := server.NewServer(cfg, logger, mockDB, forgeProvider, nil, authn)
	require.NoError(t, err)

	return srv.Handler
}

// The anonymous surface through the real mux, with no credential.
func TestEnforcingServerAnonymousRoutes(t *testing.T) {
	handler := newEnforcingTestServer(t)

	for _, path := range []string{"/openapi.yaml", "/api/version", "/api/docs/", "/docs"} {
		t.Run(path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, http.NoBody))
			assert.Equal(t, http.StatusOK, rr.Code, "%s must not require a token", path)
		})
	}

	// The redirect onto "/api/docs/" is generated inside the mux, downstream of
	// authentication, so this would 401 a browser if only the slashed form were
	// exempt. Which status the mux picks is not this test's business.
	t.Run("/api/docs", func(t *testing.T) {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/docs", http.NoBody))
		assert.GreaterOrEqual(t, rr.Code, http.StatusMultipleChoices)
		assert.Less(t, rr.Code, http.StatusBadRequest)
		assert.Equal(t, "/api/docs/", rr.Header().Get("Location"))
	})
}

// api.GetSwagger is the authority on what the API is: its Paths enumerate every
// generated operation, and none may be reachable without a credential.
func TestEnforcingServerRequiresTokenOnEveryOperation(t *testing.T) {
	handler := newEnforcingTestServer(t)

	swagger, err := api.GetSwagger()
	require.NoError(t, err)
	require.NotEmpty(t, swagger.Paths.Map())

	// The anonymous surface is registered directly on the mux and appears in no
	// path item, so there is nothing to exempt. Deliberately no skip list: if one
	// of those gains a path item, this test should fail rather than pass it over.
	for path, item := range swagger.Paths.Map() {
		for method := range item.Operations() {
			// Path parameters are left as literal "{id}" segments, so this only
			// works if the 401 happens before routing.
			target := "/api" + path
			t.Run(method+" "+target, func(t *testing.T) {
				rr := httptest.NewRecorder()
				handler.ServeHTTP(rr, httptest.NewRequestWithContext(t.Context(), method, target, http.NoBody))
				assert.Equal(t, http.StatusUnauthorized, rr.Code)
			})
		}
	}
}

// Why the middleware is not in the generated router's per-operation Middlewares
// list: those run inside the mux alongside OapiRequestValidator, so a malformed
// body would be answered 400 and the missing credential never mentioned.
func TestEnforcingServerAuthenticatesBeforeValidating(t *testing.T) {
	handler := newEnforcingTestServer(t)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/clusters",
		strings.NewReader(`{"this":"is not a cluster"}`))
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusUnauthorized, rr.Code,
		"an unauthenticated request must be 401 even when its body would also fail validation")
}

// The chain order, through the real NewServer, with authentication disabled --
// which is every deployment today. Wiring authentication in reordered the two
// middlewares that were already there, and this is the one behaviour that
// changes for a deployment that never turns authentication on.
//
// managed-by used to wrap logging, so it answered 409 before logging ever ran.
// It is now inside logging, so the same 409 produces the usual request/response
// pair. Asserted here because the ordering is invisible to internal/auth's own
// tests and to middleware_test.go, which drives managedByMiddleware directly.
func TestDisabledServerLogsTheManagedByRejection(t *testing.T) {
	var logged bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))

	cfg := config.Config{
		FWEAPIKey:            "test-key",
		ManagedByK8sPlatform: true,
		ProfilesDir:          "./profiles",
	}
	mockDB := &testutil.MockDB{}
	forgeProvider := forge.NewForgeProvider(cfg, mockDB, logger)

	// nil authenticator: authentication is absent from the chain entirely.
	srv, err := server.NewServer(cfg, logger, mockDB, forgeProvider, nil, nil)
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rr, httptest.NewRequestWithContext(
		t.Context(), http.MethodPost, "/api/clusters", strings.NewReader(`{}`)))

	assert.Equal(t, http.StatusConflict, rr.Code,
		"a mutating request with no managed-by header is still rejected when authentication is off")

	out := logged.String()
	assert.Contains(t, out, "Incoming request",
		"logging now wraps managed-by, so the rejected request is logged on the way in")
	assert.Contains(t, out, "Request completed",
		"and on the way out; before this change managed-by answered before logging ran")
}
