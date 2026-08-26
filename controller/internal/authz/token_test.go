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

// The sidecar's API is read/write and it does not bind loopback — cedar-agent
// 0.2.2 ignores both --addr and ROCKET_ADDRESS — so the token is what keeps its
// policy store from being rewritable by anything that can reach the pod IP. It
// has to reach every request.
//
// This is a regression test in the literal sense. The token was added and then
// silently not sent, and every unit test stayed green, because they all stub an
// agent that does not check it. Only the live suite caught it, and the live
// suite does not run in CI.
func TestTheTokenReachesEveryRequest(t *testing.T) {
	seen := map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen[r.URL.Path] = r.Header.Get("Authorization")
		switch r.URL.Path {
		case policiesPath:
			_, _ = w.Write([]byte(`[{"id":"p"}]`))
		case dataPath:
			_, _ = w.Write([]byte(`[{"uid":{"type":"NKE::Role","id":"r"},"attrs":{},"parents":[]}]`))
		default:
			_, _ = w.Write([]byte(`{"decision":"Deny","diagnostics":{"reason":[],"errors":[]}}`))
		}
	}))
	defer server.Close()

	agent, err := NewCedarAgent(server.URL, "s3cret", 0)
	require.NoError(t, err)

	_, err = agent.Authorize(context.Background(), validQuery())
	require.NoError(t, err)
	require.NoError(t, agent.Ready(context.Background()))

	require.Len(t, seen, 3, "authorize, policies and data should all have been called")
	for path, header := range seen {
		// Bare, not "Bearer": cedar-agent compares the header value against its
		// --authentication argument and answers 401 to the Bearer form.
		assert.Equalf(t, "s3cret", header, "%s went out without the token", path)
	}
}

// An empty token sends no header at all, rather than an empty one. Setup refuses
// to build a sidecar engine without a token, so this pins the shape of the
// guard, not a supported configuration.
func TestAnEmptyTokenSendsNoHeader(t *testing.T) {
	var had bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, had = r.Header["Authorization"]
		_, _ = w.Write([]byte(`{"decision":"Deny","diagnostics":{"reason":[],"errors":[]}}`))
	}))
	defer server.Close()

	agent, err := NewCedarAgent(server.URL, "", 0)
	require.NoError(t, err)
	_, err = agent.Authorize(context.Background(), validQuery())
	require.NoError(t, err)

	assert.False(t, had, "an empty token must not become an empty Authorization header")
}

// Setup refuses a sidecar engine with no token, so the requirement does not live
// only in a chart value that an operator can unset.
func TestSetupRefusesASidecarWithoutAToken(t *testing.T) {
	_, err := Options{Endpoint: "http://127.0.0.1:8180"}.engine()
	require.Error(t, err, "an unauthenticated policy store on the pod IP is not a supported configuration")
	assert.Contains(t, err.Error(), "token")
}

// And the in-process engine needs no token, so the requirement is scoped to the
// engine that has the problem.
func TestSetupDoesNotRequireATokenInProcess(t *testing.T) {
	engine, err := Options{BundleDir: bundleDir(t)}.engine()
	require.NoError(t, err)
	require.NotNil(t, engine)
	assert.Equal(t, "cedar-go", engine.Name())
}

// Both sources set is refused rather than resolved by precedence: an operator
// who set both has a belief about which is running, and a silent winner makes
// that belief unfalsifiable.
func TestSetupRefusesTwoEngines(t *testing.T) {
	_, err := Options{BundleDir: bundleDir(t), Endpoint: "http://127.0.0.1:8180", EndpointToken: "t"}.engine()
	require.Error(t, err)
}

// Neither source set is "disabled", which is a nil engine and no error — not an
// engine that allows everyone.
func TestSetupWithNoEngineIsDisabled(t *testing.T) {
	engine, err := Options{}.engine()
	require.NoError(t, err)
	assert.Nil(t, engine, "disabled must be a nil engine, never a permissive one")
}
