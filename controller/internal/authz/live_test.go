// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build authz_live

// The sidecar engine, against a real cedar-agent holding the assembled bundle.
// Skipped unless AUTHZ_TEST_ENDPOINT is set, and behind a build tag so it never
// runs in the ordinary unit-test job.
//
//	go generate ./internal/authz/
//	(cd hack/authz-bundle && GOWORK=off go run .)
//	docker run -d --name nke-cedar -p 8190:8180 \
//	  -v "$PWD/deploy/authz/bundle:/bundle:ro" permitio/cedar-agent:0.2.2 \
//	  --schema /bundle/schema.json --data /bundle/entities.json --policies /bundle/policies.json
//	AUTHZ_TEST_ENDPOINT=http://localhost:8190 go test -tags authz_live ./internal/authz/ -v
//
// It runs the same bundleMatrix() that TestLocalMatchesTheBundle runs in-process,
// which is what makes the two engines comparable rather than merely both tested:
// one set of statements about what deploy/authz grants, asserted against each.
package authz

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func liveEndpoint(t *testing.T) string {
	t.Helper()
	endpoint := os.Getenv("AUTHZ_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("set AUTHZ_TEST_ENDPOINT to a running cedar-agent")
	}
	return endpoint
}

// The gate that turns cedar-agent's best-effort file loading into a startup
// failure. A bundle that half-loaded leaves the agent answering 200 OK and Deny
// to everything, which is indistinguishable at request time from a policy nobody
// wrote.
func TestLiveReadyGate(t *testing.T) {
	agent, err := NewCedarAgent(liveEndpoint(t), testToken, 0)
	require.NoError(t, err)

	require.NoError(t, agent.Ready(context.Background()),
		"the bundle did not fully load; check the sidecar's logs for a load error")
}

// The same matrix the in-process engine is held to.
func TestLiveMatchesTheBundle(t *testing.T) {
	agent, err := NewCedarAgent(liveEndpoint(t), testToken, 0)
	require.NoError(t, err)
	require.NoError(t, agent.Ready(context.Background()), "the bundle did not fully load")

	runBundleMatrix(t, agent)
}

// The anonymous surface reaches the handler with no Principal and no decision,
// against a real engine that would otherwise refuse it.
func TestLiveAnonymousSurfaceIsUntouched(t *testing.T) {
	agent, err := NewCedarAgent(liveEndpoint(t), testToken, 0)
	require.NoError(t, err)

	resolver, err := NewResolver(http.NewServeMux())
	require.NoError(t, err)

	middleware, err := NewMiddleware(agent, resolver, ModeEnforcing,
		slog.New(slog.DiscardHandler), nil)
	require.NoError(t, err)

	handler := middleware.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for _, path := range []string{"/healthz", "/readyz", "/api/version", "/openapi.yaml"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, http.NoBody))
		assert.Equalf(t, http.StatusOK, rec.Code, "%s must not need a decision", path)
	}
}
