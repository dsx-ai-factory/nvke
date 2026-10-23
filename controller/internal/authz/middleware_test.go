// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/auth/authn"
)

// fakeAuthorizer answers however a test needs, and records what it was asked.
type fakeAuthorizer struct {
	allow bool
	err   error
	asked []Query
}

func (f *fakeAuthorizer) Authorize(_ context.Context, q Query) (Decision, error) {
	f.asked = append(f.asked, q)
	if f.err != nil {
		return Decision{}, f.err
	}
	return Decision{Allow: f.allow, Reasons: []string{"a-policy"}}, nil
}

func (f *fakeAuthorizer) Ready(context.Context) error { return f.err }

func (f *fakeAuthorizer) Name() string { return "fake" }

type recorded struct {
	outcome Outcome
	reason  DenyReason
	action  string
}

// harness builds the middleware over a handler that records whether it ran.
func harness(t *testing.T, authorizer Authorizer, mode Mode) (http.Handler, *bool, *[]recorded) {
	t.Helper()

	// mux exists only so NewResolver has routes to match against; the
	// middleware wraps a separate handler below, so these bodies never run.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/clusters/{clusterID}", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("GET /api/clusters", func(http.ResponseWriter, *http.Request) {})

	resolver, err := NewResolver(mux)
	require.NoError(t, err)

	var reachedHandler bool
	var counts []recorded
	record := func(outcome Outcome, reason DenyReason, action string) {
		counts = append(counts, recorded{outcome, reason, action})
	}

	m, err := NewMiddleware(authorizer, resolver, mode,
		slog.New(slog.DiscardHandler), record)
	require.NoError(t, err)

	handler := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reachedHandler = true
		w.WriteHeader(http.StatusOK)
	}))
	return handler, &reachedHandler, &counts
}

// authenticated returns a GET carrying a Principal, as the authentication
// middleware would have left it. The method is fixed because what varies
// between these tests is the policy decision, not the verb.
func authenticated(t *testing.T, path string) *http.Request {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, http.NoBody)
	return r.WithContext(authn.ContextWithPrincipal(r.Context(), &authn.Principal{
		IssuerName: "kube-apiserver",
		Issuer:     "https://kubernetes.default.svc.cluster.local",
		Subject:    "system:serviceaccount:nke-system:cli",
	}))
}

func TestNewMiddlewareRejectsMissingDependencies(t *testing.T) {
	resolver, err := NewResolver(http.NewServeMux())
	require.NoError(t, err)

	_, err = NewMiddleware(nil, resolver, ModeEnforcing, nil, nil)
	require.Error(t, err, "disabling authorization means building no middleware, not one that allows everyone")

	_, err = NewMiddleware(&fakeAuthorizer{}, nil, ModeEnforcing, nil, nil)
	require.Error(t, err)
}

func TestAllowReachesTheHandler(t *testing.T) {
	authorizer := &fakeAuthorizer{allow: true}
	handler, reached, counts := harness(t, authorizer, ModeEnforcing)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authenticated(t, "/api/clusters/"+someClusterID))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, *reached)
	require.Len(t, authorizer.asked, 1)

	q := authorizer.asked[0]
	assert.Equal(t, "getCluster", q.Action)
	assert.Equal(t, someClusterID, q.ResourceID)
	assert.Equal(t, "NKE::Cluster", q.ResourceType)
	assert.Equal(t, "system:serviceaccount:nke-system:cli", q.Principal)
	assert.Equal(t, "kube-apiserver", q.Issuer, "policy names the issuer entry, never the iss claim")

	require.Len(t, *counts, 1)
	assert.Equal(t, OutcomeAllowed, (*counts)[0].outcome)
}

func TestDenyAnswers403AndStopsTheRequest(t *testing.T) {
	handler, reached, counts := harness(t, &fakeAuthorizer{allow: false}, ModeEnforcing)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authenticated(t, "/api/clusters/"+someClusterID))

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.False(t, *reached, "a refused request must not reach the handler")
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	require.Len(t, *counts, 1)
	assert.Equal(t, OutcomeRejected, (*counts)[0].outcome)
	assert.Equal(t, ReasonNoPolicyMatch, (*counts)[0].reason)
}

// The response says it was refused and nothing more. Which policy matched, and
// what the engine said, belong in the log.
func TestDenialBodyLeaksNothing(t *testing.T) {
	handler, _, _ := harness(t, &fakeAuthorizer{allow: false}, ModeEnforcing)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authenticated(t, "/api/clusters/"+someClusterID))

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

	message, _ := body["message"].(string)
	assert.NotContains(t, message, "a-policy")
	assert.NotContains(t, message, "kube-apiserver")
	assert.NotContains(t, message, "abc")
}

// An error from the engine is CWE's failure, not the caller's — and it is still
// a denial. Failing open here would make an unreachable sidecar an open API.
func TestEngineErrorFailsClosed(t *testing.T) {
	handler, reached, counts := harness(t, &fakeAuthorizer{err: errors.New("connection refused")}, ModeEnforcing)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authenticated(t, "/api/clusters/"+someClusterID))

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.False(t, *reached)
	require.Len(t, *counts, 1)
	assert.Equal(t, ReasonEngineError, (*counts)[0].reason)
}

func TestUnroutedRequestIsRefusedWithoutAskingTheEngine(t *testing.T) {
	authorizer := &fakeAuthorizer{allow: true}
	handler, reached, counts := harness(t, authorizer, ModeEnforcing)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authenticated(t, "/api/no-such-route"))

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.False(t, *reached)
	assert.Empty(t, authorizer.asked, "an unrouted request has no operation to ask about")
	require.Len(t, *counts, 1)
	assert.Equal(t, ReasonNoRoute, (*counts)[0].reason)
}

// Authentication puts a Principal on every non-anonymous request it admits.
// Without one the chain is misordered, or authentication is off while this is
// on. Neither is a state to authorize a request in.
func TestMissingPrincipalIsRefused(t *testing.T) {
	authorizer := &fakeAuthorizer{allow: true}
	handler, reached, counts := harness(t, authorizer, ModeEnforcing)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/clusters/"+someClusterID, http.NoBody))

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.False(t, *reached)
	assert.Empty(t, authorizer.asked)
	require.Len(t, *counts, 1)
	assert.Equal(t, ReasonNoPrincipal, (*counts)[0].reason)
}

// The anonymous surface is authentication's list, reused. A probe that had to be
// authorized would make a policy outage look like a dead pod.
func TestAnonymousSurfaceSkipsAuthorization(t *testing.T) {
	authorizer := &fakeAuthorizer{allow: false}
	handler, _, counts := harness(t, authorizer, ModeEnforcing)

	for _, path := range []string{"/healthz", "/readyz", "/api/version", "/openapi.yaml", "/docs", "/api/docs/"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, http.NoBody))
			assert.Equal(t, http.StatusOK, rec.Code, "%s must not require authorization", path)
		})
	}
	assert.Empty(t, authorizer.asked)
	assert.Empty(t, *counts, "an exempt path is not a decision, so it is not counted")
}

// Permissive evaluates and counts everything and refuses nothing. It is a
// migration tool, not an off switch: the same code path runs, one step short of
// the 403.
func TestPermissiveNeverRefuses(t *testing.T) {
	tests := []struct {
		name       string
		authorizer Authorizer
		path       string
		wantReason DenyReason
	}{
		{"a denial", &fakeAuthorizer{allow: false}, "/api/clusters/" + someClusterID, ReasonNoPolicyMatch},
		{"an engine error", &fakeAuthorizer{err: errors.New("boom")}, "/api/clusters/" + someClusterID, ReasonEngineError},
		{"an unrouted request", &fakeAuthorizer{allow: true}, "/api/nope", ReasonNoRoute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler, reached, counts := harness(t, tt.authorizer, ModePermissive)

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, authenticated(t, tt.path))

			assert.Equal(t, http.StatusOK, rec.Code)
			assert.True(t, *reached, "permissive must serve the request")
			require.Len(t, *counts, 1)
			assert.Equal(t, OutcomeWouldReject, (*counts)[0].outcome,
				"the counter an operator watches before enforcing")
			assert.Equal(t, tt.wantReason, (*counts)[0].reason)
		})
	}
}

func TestCollectionOperationsAuthorizeAgainstTheCollection(t *testing.T) {
	authorizer := &fakeAuthorizer{allow: true}
	handler, _, _ := harness(t, authorizer, ModeEnforcing)

	handler.ServeHTTP(httptest.NewRecorder(), authenticated(t, "/api/clusters"))

	require.Len(t, authorizer.asked, 1)
	assert.Equal(t, "listClusters", authorizer.asked[0].Action)
	assert.Equal(t, CollectionType, authorizer.asked[0].ResourceType)
	assert.Equal(t, "clusters", authorizer.asked[0].ResourceID)
}
