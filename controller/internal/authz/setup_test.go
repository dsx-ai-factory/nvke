// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Setup itself, which no test called.
//
// Options.engine() was covered and Setup was not, which left the difference
// between them — the readiness gate, the mode, and what a disabled deployment
// gets back — asserted nowhere. §13's evidence table claimed the startup half of
// AZ-5; this is what would have to fail for that claim to be worth making.

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestSetupWithNoBundleReturnsNoProvider(t *testing.T) {
	provider, err := Setup(t.Context(), Options{}, testLogger())
	require.NoError(t, err)
	assert.Nil(t, provider, "disabled is a nil Provider, never a permissive one")

	// And a nil Provider builds no middleware rather than an allow-everyone one.
	middleware, err := provider.Middleware(http.NewServeMux())
	require.NoError(t, err)
	assert.Nil(t, middleware)
}

func TestSetupRefusesABundleThatDidNotLoad(t *testing.T) {
	_, err := Setup(t.Context(), Options{BundleDir: t.TempDir()}, testLogger())
	require.Error(t, err, "set-but-unloadable must stop the process, never mean disabled")
}

// Permissive does not make an unloadable bundle survivable. The bundle is loaded
// before the mode is consulted, which is the opposite of what the mode's name
// suggests and the reason it is asserted rather than described.
func TestPermissiveDoesNotRescueABundleThatDidNotLoad(t *testing.T) {
	_, err := Setup(t.Context(), Options{BundleDir: t.TempDir(), Permissive: true}, testLogger())
	require.Error(t, err)
}

func TestSetupCarriesTheModeThroughToTheMiddleware(t *testing.T) {
	for _, tt := range []struct {
		name       string
		permissive bool
		want       Mode
	}{
		{"the default", false, ModeEnforcing},
		{"permissive", true, ModePermissive},
	} {
		t.Run(tt.name, func(t *testing.T) {
			provider, err := Setup(t.Context(),
				Options{BundleDir: bundleDir(t), Permissive: tt.permissive}, testLogger())
			require.NoError(t, err)
			require.NotNil(t, provider)
			assert.Equal(t, tt.want, provider.mode)

			middleware, err := provider.Middleware(http.NewServeMux())
			require.NoError(t, err)
			require.NotNil(t, middleware)
			assert.Equal(t, tt.want, middleware.mode, "the mode Setup validated must be the mode that runs")
		})
	}
}

// The gate that no shipped engine can fail, exercised by one that can.
//
// NewLocal refuses everything Ready refuses, so for cedar-go this check is
// unreachable. It is there for an engine that constructs from something other
// than a file — one that holds a handle to a store it has not read yet — and
// this is the test that keeps it from being deleted as dead code.
type notReadyEngine struct{ Authorizer }

func (notReadyEngine) Name() string { return "not-ready" }

func (notReadyEngine) Ready(context.Context) error {
	return errors.New("the store answered, and it was empty")
}

func TestSetupRefusesAnEngineThatIsNotReady(t *testing.T) {
	_, err := Setup(t.Context(), Options{
		BundleDir: bundleDir(t),
		newEngine: func(string) (Authorizer, error) { return notReadyEngine{}, nil },
	}, testLogger())

	require.Error(t, err, "an engine holding no bundle must stop the process, not serve denials")
	assert.Contains(t, err.Error(), "not ready")
}

// A Provider without a mux is a wiring bug, and must not produce a middleware
// that cannot resolve a route.
func TestMiddlewareRequiresTheMux(t *testing.T) {
	provider, err := Setup(t.Context(), Options{BundleDir: bundleDir(t)}, testLogger())
	require.NoError(t, err)

	_, err = provider.Middleware(nil)
	require.Error(t, err)
}
