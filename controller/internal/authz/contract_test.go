// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The Authorizer contract, asserted against every implementation rather than
// described in a comment and hoped for.
//
// There is one implementation today. These rules are still worth pinning: they
// are what the middleware relies on, and they are the specification a second
// engine would have to meet. An out-of-process engine was built and dropped —
// see the design's Appendix C — and every rule below is a lesson from it,
// because each one names something the two implementations disagreed about
// until it was written down.

// contractEngines returns each implementation with a bundle already loaded.
func contractEngines(t *testing.T) map[string]Authorizer {
	t.Helper()

	local, err := NewLocal(bundleDir(t))
	require.NoError(t, err)

	return map[string]Authorizer{"cedar-go": local}
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

// Every engine must tolerate the identifiers this API actually produces.
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

// A hostile identifier must never produce an Allow, and must be refused as a bad
// question rather than denied on the merits — otherwise the metric blames the
// policy for what is really a malformed request.
func TestNoEngineAllowsAHostileIdentifier(t *testing.T) {
	hostile := []string{
		`evil" || true || "`,
		`x", NKE::Action::"deleteCluster`,
		"line\nbreak",
		"invalid\xffutf8",
	}
	for name, engine := range contractEngines(t) {
		for _, id := range hostile {
			t.Run(name+"/"+id, func(t *testing.T) {
				decision, err := engine.Authorize(context.Background(), Query{
					Principal: id, Issuer: "kube-apiserver", Action: "getCluster",
					ResourceType: "NKE::Cluster", ResourceID: "c1", Method: http.MethodGet,
				})
				assert.Falsef(t, decision.Allow, "%s allowed a hostile identifier", name)
				require.Errorf(t, err, "%s evaluated a hostile identifier instead of refusing it", name)
				assert.ErrorIsf(t, err, ErrInvalidQuery, "%s refused with the wrong error class", name)
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

// Setup's engine selection, which no test covered before the sidecar was
// dropped and which is now the whole of the configuration surface.
func TestSetupSelectsTheInProcessEngine(t *testing.T) {
	engine, err := Options{BundleDir: bundleDir(t)}.engine()
	require.NoError(t, err)
	require.NotNil(t, engine)
	assert.Equal(t, "cedar-go", engine.Name())
}

// No bundle is "disabled", which is a nil engine and no error — never an engine
// that allows everyone.
func TestSetupWithNoBundleIsDisabled(t *testing.T) {
	engine, err := Options{}.engine()
	require.NoError(t, err)
	assert.Nil(t, engine, "disabled must be a nil engine, never a permissive one")
}

// A bundle that does not load is a startup failure, not a disabled deployment
// and not a deployment that refuses everything quietly.
func TestSetupRefusesABundleThatDoesNotLoad(t *testing.T) {
	_, err := Options{BundleDir: t.TempDir()}.engine()
	require.Error(t, err, "set-but-unloadable must be an error, never \"disabled\"")
}
