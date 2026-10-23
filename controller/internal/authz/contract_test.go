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

	local, err := NewLocal(shippedBundleWithMappings(t, testRoleMappings), testIssuers)
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
					Principal: subject, Issuer: testIssuer, Action: "getCluster",
					ResourceType: "NKE::Cluster", ResourceID: "11111111-1111-1111-1111-111111111111",
					Method: http.MethodGet,
				})
				require.NoErrorf(t, err, "%s refused a subject this API produces", name)
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

// Engine selection, which is the whole of the configuration surface: a bundle
// directory or nothing.
func TestSetupSelectsTheInProcessEngine(t *testing.T) {
	engine, err := Options{BundleDir: shippedBundleWithMappings(t, testRoleMappings)}.engine(t.Context(), newIssuerSet(testIssuers), nil)
	require.NoError(t, err)
	require.NotNil(t, engine)
	assert.Equal(t, "cedar-go", engine.Name())
}

// No bundle is "disabled", which is a nil engine and no error — never an engine
// that allows everyone.
func TestSetupWithNoBundleIsDisabled(t *testing.T) {
	engine, err := Options{}.engine(t.Context(), nil, nil)
	require.NoError(t, err)
	assert.Nil(t, engine, "disabled must be a nil engine, never a permissive one")
}

// A bundle that does not load is a startup failure, not a disabled deployment
// and not a deployment that refuses everything quietly.
func TestSetupRefusesABundleThatDoesNotLoad(t *testing.T) {
	_, err := Options{BundleDir: t.TempDir()}.engine(t.Context(), nil, nil)
	require.Error(t, err, "set-but-unloadable must be an error, never \"disabled\"")
}
