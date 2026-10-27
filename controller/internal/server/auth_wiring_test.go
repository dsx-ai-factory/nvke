// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server_test

// Where authentication sits in the chain, tested through the real NewServer.
// auth/authn's own tests drive a bare handler and prove nothing about the
// wiring. Routes are driven rather than read, because http.ServeMux is not
// enumerable and the generated router types BaseRouter as a concrete
// *http.ServeMux.

import (
	"bytes"
	"context"
	"crypto"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	auth "github.com/dsx-ai-factory/nvke/auth/authn"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/server"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

// errNoKeys is what the stub key source returns. No key is ever asked for; the
// source exists because NewVerifier requires one per issuer.
var errNoKeys = errors.New("no keys in this test")

type stubKeySource struct{}

func (stubKeySource) KeyFor(context.Context, string) (crypto.PublicKey, error) {
	return nil, errNoKeys
}

// testAuthConfig is the registry both test servers verify against. No key is
// ever resolved, so the issuer only has to be well formed.
func testAuthConfig() *auth.Config {
	return &auth.Config{
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
}

// newEnforcingTestServer builds the production handler, enforcing.
func newEnforcingTestServer(t *testing.T) http.Handler {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Config{
		ManagedByK8sPlatform: false,
		ProfilesDir:          "./profiles",
	}

	verifier, err := auth.NewVerifier(testAuthConfig(), map[string]auth.KeySource{"test-kubernetes": stubKeySource{}})
	require.NoError(t, err)

	authn, err := auth.NewAuthenticator(verifier, auth.ModeEnforcing, logger, nil)
	require.NoError(t, err)

	mockDB := &testutil.MockDB{}
	srv, err := server.NewServer(cfg, logger, mockDB, nil, authn, nil)
	require.NoError(t, err)

	return srv.Handler
}

// decisions records what the Authenticator reported, so a permissive run can be
// checked for what it would have done rather than only for what it served.
type decisions struct {
	mu   sync.Mutex
	seen []decision
}

type decision struct {
	outcome auth.Outcome
	reason  auth.FailureReason
}

func (d *decisions) record(outcome auth.Outcome, reason auth.FailureReason, _ string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seen = append(d.seen, decision{outcome: outcome, reason: reason})
}

func (d *decisions) all() []decision {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]decision(nil), d.seen...)
}

// newPermissiveTestServer builds the production handler in permissive mode. The
// database is a mock with one expectation, so a request that gets through
// reaches a handler rather than panicking in the mock.
func newPermissiveTestServer(t *testing.T) (http.Handler, *decisions) {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Config{
		ManagedByK8sPlatform: false,
		ProfilesDir:          "./profiles",
	}

	verifier, err := auth.NewVerifier(testAuthConfig(), map[string]auth.KeySource{"test-kubernetes": stubKeySource{}})
	require.NoError(t, err)

	recorded := &decisions{}
	authn, err := auth.NewAuthenticator(verifier, auth.ModePermissive, logger, recorded.record)
	require.NoError(t, err)

	mockDB := &testutil.MockDB{}
	mockDB.On("ListClusters", mock.Anything, mock.Anything).Return([]db.Cluster{}, nil)

	srv, err := server.NewServer(cfg, logger, mockDB, nil, authn, nil)
	require.NoError(t, err)

	return srv.Handler, recorded
}

// Permissive serves the request and records what enforcing would have refused.
// auth/authn proves the middleware does this; only the real chain shows that
// a permissive Authenticator hands the request on to the mux instead of
// answering it.
func TestPermissiveServerServesUnauthenticatedRequests(t *testing.T) {
	handler, recorded := newPermissiveTestServer(t)

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequestWithContext(
		t.Context(), http.MethodGet, "/api/clusters", http.NoBody,
	))

	assert.Equal(t, http.StatusOK, rr.Code,
		"permissive serves a request carrying no token; body: %s", rr.Body)
	assert.Equal(t, []decision{{outcome: auth.OutcomeWouldReject, reason: auth.ReasonNoToken}},
		recorded.all(), "the decision is counted as would_reject, not authenticated")
}

// The anonymous surface through the real mux, with no credential.
func TestEnforcingServerAnonymousRoutes(t *testing.T) {
	handler := newEnforcingTestServer(t)

	for _, path := range []string{"/openapi.yaml", "/openapi-public.yaml", "/api/version", "/api/docs/", "/docs"} {
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

// Swagger UI must load the filtered document. The full specification stays at
// /openapi.yaml for client generators and still answers without a token.
func TestSwaggerUILoadsThePublicSpecification(t *testing.T) {
	handler := newEnforcingTestServer(t)

	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/docs/", http.NoBody))
	require.Equal(t, http.StatusOK, page.Code)
	assert.Contains(t, page.Body.String(), "/openapi-public.yaml")

	public := httptest.NewRecorder()
	handler.ServeHTTP(public, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/openapi-public.yaml", http.NoBody))
	require.Equal(t, http.StatusOK, public.Code)
	assert.Equal(t, "application/yaml", public.Header().Get("Content-Type"))
	assert.NotContains(t, strings.ToLower(public.Body.String()), "teleport")
	assert.Contains(t, public.Body.String(), "url: /api")

	full := httptest.NewRecorder()
	handler.ServeHTTP(full, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/openapi.yaml", http.NoBody))
	require.Equal(t, http.StatusOK, full.Code)
	assert.Contains(t, full.Body.String(), "teleportEnabled")
}

// api.GetSwagger is the authority on what the API is: its Paths enumerate every
// generated operation, and none may be reachable without a credential.
func TestEnforcingServerRequiresTokenOnEveryOperation(t *testing.T) {
	handler := newEnforcingTestServer(t)

	swagger, err := api.GetSwagger()
	require.NoError(t, err)
	require.NotEmpty(t, swagger.Paths.Map())

	// The anonymous surface is registered on the mux and appears in no path item,
	// so there is nothing to exempt. No skip list: if one of them ever gains a
	// path item, this test should fail.
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

// Authentication runs ahead of the OpenAPI validator, so a request that is both
// unauthenticated and malformed is answered 401, not 400. The generated router's
// per-operation middleware list runs after routing, which is why authentication
// is not in it.
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

// The authN library writes its own error body so its module doesn't depend on
// controller/api. This holds it to the API's GenericError: an unknown field or a
// missing message fails the decode.
func TestAuthenticationErrorBodyIsAGenericError(t *testing.T) {
	handler := newEnforcingTestServer(t)

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/clusters", http.NoBody))
	require.Equal(t, http.StatusUnauthorized, rr.Code)
	assert.Equal(t, "application/json", rr.Header().Get("Content-Type"))

	dec := json.NewDecoder(rr.Body)
	dec.DisallowUnknownFields()
	var body api.GenericError
	require.NoError(t, dec.Decode(&body))
	assert.NotEmpty(t, body.Message)
}

// The chain order, through the real NewServer, with authentication disabled --
// which is every deployment today. Wiring authentication in reordered the two
// middlewares that were already there, and this is the one behavior that
// changes for a deployment that never turns authentication on.
//
// managed-by used to wrap logging, so it answered 409 before logging ever ran.
// It is now inside logging, so the same 409 produces the usual request/response
// pair. Asserted here because the ordering is invisible to auth/authn's own
// tests and to middleware_test.go, which drives managedByMiddleware directly.
func TestDisabledServerLogsTheManagedByRejection(t *testing.T) {
	var logged bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))

	cfg := config.Config{
		ManagedByK8sPlatform: true,
		ProfilesDir:          "./profiles",
	}
	mockDB := &testutil.MockDB{}
	// nil authenticator: authentication is absent from the chain entirely.
	srv, err := server.NewServer(cfg, logger, mockDB, nil, nil, nil)
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rr, httptest.NewRequestWithContext(
		t.Context(), http.MethodPost, "/api/clusters", strings.NewReader(`{}`),
	))

	assert.Equal(t, http.StatusConflict, rr.Code,
		"a mutating request with no managed-by header is still rejected when authentication is off")

	var incoming, completed map[string]any
	for _, line := range bytes.Split(logged.Bytes(), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var record map[string]any
		require.NoError(t, json.Unmarshal(line, &record))
		switch record["msg"] {
		case "Incoming request":
			incoming = record
		case "Request completed":
			completed = record
		}
	}
	require.NotNil(t, incoming,
		"logging now wraps managed-by, so the rejected request is logged on the way in")
	require.NotNil(t, completed,
		"and on the way out; before this change managed-by answered before logging ran")
	require.Equal(t, "managed_by_header_required", completed["reason"])
}
