// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The Authorizer contract, asserted against every implementation rather than
// described in a comment and hoped for. An engine that passes this and
// bundleMatrix() is swappable; one that does not is a trap for whoever swaps it.
//
// The sidecar is exercised here through a stub server rather than a live
// cedar-agent, so the contract runs in the ordinary unit-test job for both
// engines. TestLiveMatchesTheBundle covers the real thing.

// contractEngines returns each implementation with a bundle already loaded.
func contractEngines(t *testing.T) map[string]Authorizer {
	t.Helper()

	local, err := NewLocal(bundleDir(t))
	require.NoError(t, err)

	// A stub that answers like cedar-agent, so CedarAgent's own request
	// building, response decoding and error classification are under test.
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case authorizePath:
			_, _ = w.Write([]byte(`{"decision":"Deny","diagnostics":{"reason":[],"errors":[]}}`))
		case policiesPath:
			_, _ = w.Write([]byte(`[{"id":"p"}]`))
		case dataPath:
			_, _ = w.Write([]byte(`[{"uid":{"type":"NKE::Role","id":"reader"},"attrs":{},"parents":[]}]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(stub.Close)

	agent, err := NewCedarAgent(stub.URL, 0)
	require.NoError(t, err)

	return map[string]Authorizer{"cedar-go": local, "cedar-agent": agent}
}

func TestEveryEngineNamesItself(t *testing.T) {
	for want, engine := range contractEngines(t) {
		assert.Equalf(t, want, engine.Name(), "%T reports the wrong name", engine)
	}
}

// Ready is what turns "the bundle did not load" into a startup failure, so it
// has to answer for a loaded bundle without a request having been made.
func TestEveryEngineIsReadyWithALoadedBundle(t *testing.T) {
	for name, engine := range contractEngines(t) {
		assert.NoErrorf(t, engine.Ready(context.Background()), "%s should be ready", name)
	}
}

// A query missing a field is the caller's fault, and every engine has to say so
// the same way — otherwise the middleware's reason label depends on which engine
// is deployed, and an operator comparing two sites sees different metrics for
// the same bug.
func TestEveryEngineWrapsErrInvalidQuery(t *testing.T) {
	incomplete := []struct {
		name string
		q    Query
	}{
		{"no principal", Query{Action: "getCluster", ResourceType: "NKE::Cluster", ResourceID: "c1"}},
		{"no action", Query{Principal: subjectReader, ResourceType: "NKE::Cluster", ResourceID: "c1"}},
		{"no resource type", Query{Principal: subjectReader, Action: "getCluster", ResourceID: "c1"}},
		{"no resource id", Query{Principal: subjectReader, Action: "getCluster", ResourceType: "NKE::Cluster"}},
	}
	for name, engine := range contractEngines(t) {
		for _, tt := range incomplete {
			t.Run(name+"/"+tt.name, func(t *testing.T) {
				_, err := engine.Authorize(context.Background(), tt.q)
				require.Error(t, err)
				require.ErrorIsf(t, err, ErrInvalidQuery,
					"%s must wrap ErrInvalidQuery so the middleware classifies it without knowing the engine", name)
			})
		}
	}
}

// An engine that cannot answer must return an error, never a zero-value
// Decision. The middleware counts the two differently — engine_error against
// no_policy_match — and an operator watching would_reject before enforcing needs
// that split to be true.
func TestEveryEngineReturnsNoAllowOnError(t *testing.T) {
	for name, engine := range contractEngines(t) {
		t.Run(name, func(t *testing.T) {
			decision, err := engine.Authorize(context.Background(), Query{Principal: "x"})
			require.Error(t, err)
			assert.False(t, decision.Allow, "%s returned Allow alongside an error", name)
		})
	}
}

// Every engine must tolerate the identifiers this API actually produces. The
// sidecar renders them into Cedar source and the in-process engine passes them
// as typed values, so the two have different reasons to get this wrong.
func TestEveryEngineAcceptsRealSubjectShapes(t *testing.T) {
	subjects := []string{
		"system:serviceaccount:nke-system:nke-cli",
		"spiffe://nvidia.com/ns/nke-system/sa/nke-controller",
		"spiffe://nvidia.com/ns/nke-system/sa/some-workload",
	}
	for name, engine := range contractEngines(t) {
		for _, subject := range subjects {
			t.Run(name+"/"+subject, func(t *testing.T) {
				_, err := engine.Authorize(context.Background(), Query{
					Principal: subject, Issuer: "kube-apiserver", Action: "getCluster",
					ResourceType: "NKE::Cluster", ResourceID: "11111111-1111-1111-1111-111111111111",
					Method: http.MethodGet,
				})
				require.NoErrorf(t, err, "%s refused a subject this API produces", name)
			})
		}
	}
}

// A hostile identifier must never produce an Allow. How each engine gets there
// differs — the sidecar refuses to render it, the in-process engine treats it as
// data that matches no entity — and the contract is only the outcome.
func TestNoEngineAllowsAHostileIdentifier(t *testing.T) {
	hostile := []string{
		`evil" || true || "`,
		`x", NKE::Action::"deleteCluster`,
		"line\nbreak",
	}
	for name, engine := range contractEngines(t) {
		for _, id := range hostile {
			t.Run(name+"/"+id, func(t *testing.T) {
				decision, err := engine.Authorize(context.Background(), Query{
					Principal: id, Issuer: "kube-apiserver", Action: "getCluster",
					ResourceType: "NKE::Cluster", ResourceID: "c1", Method: http.MethodGet,
				})
				assert.Falsef(t, decision.Allow, "%s allowed a hostile identifier", name)
				if err != nil {
					// If it refuses, it must refuse as a bad query rather than
					// as an engine fault, or the metric blames the wrong thing.
					assert.ErrorIsf(t, err, ErrInvalidQuery, "%s refused with the wrong error class", name)
				}
			})
		}
	}
}

// A nil Authorizer must never reach the middleware. Disabling authorization
// means building no middleware, not one that allows everyone.
func TestMiddlewareRefusesANilEngine(t *testing.T) {
	resolver, err := NewResolver(http.NewServeMux())
	require.NoError(t, err)

	_, err = NewMiddleware(nil, resolver, ModeEnforcing, nil, nil)
	require.Error(t, err)
}
