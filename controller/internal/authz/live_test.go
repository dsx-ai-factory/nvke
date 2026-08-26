// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build authz_live

// End-to-end against a real cedar-agent holding the real bundle. Skipped unless
// AUTHZ_TEST_ENDPOINT is set, and behind a build tag so it never runs in the
// ordinary unit-test job.
//
//	go run ./hack/gen-authz-actions
//	(cd hack/authz-bundle && GOWORK=off go run .)
//	docker run -d --name nke-cedar -p 8190:8180 \
//	  -v "$PWD/deploy/authz/bundle:/bundle:ro" permitio/cedar-agent:0.2.2 \
//	  --schema /bundle/schema.json --data /bundle/entities.json --policies /bundle/policies.json
//	AUTHZ_TEST_ENDPOINT=http://localhost:8190 go test -tags authz_live ./internal/authz/ -v
//
// What it proves that the unit tests cannot: the query this package builds is
// one the sidecar accepts, and the bundle under deploy/authz says what its
// authors think it says.
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

	"github.com/NVIDIA/nke/controller/api"
	"github.com/NVIDIA/nke/controller/internal/auth"
)

const (
	reader       = "system:serviceaccount:nke-system:nke-cli"
	operator     = "spiffe://nvidia.com/ns/nke-system/sa/nke-controller"
	nodeOperator = "spiffe://nvidia.com/ns/nke-system/sa/node-lifecycle"
	stranger     = "spiffe://nvidia.com/ns/default/sa/nobody"
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
	agent, err := NewCedarAgent(liveEndpoint(t), 0)
	require.NoError(t, err)

	require.NoError(t, agent.Ready(context.Background()),
		"the bundle did not fully load; check the sidecar's logs for a load error")
}

// The whole chain over a real engine: a request enters the middleware carrying a
// Principal, and the bundle under deploy/authz decides.
func TestLiveDecisionMatrix(t *testing.T) {
	endpoint := liveEndpoint(t)

	agent, err := NewCedarAgent(endpoint, 0)
	require.NoError(t, err)
	require.NoError(t, agent.Ready(context.Background()), "the bundle did not fully load")

	mux := http.NewServeMux()
	api.HandlerWithOptions(nil, api.StdHTTPServerOptions{BaseRouter: mux, BaseURL: "/api"})

	resolver, err := NewResolver(mux)
	require.NoError(t, err)

	middleware, err := NewMiddleware(agent, resolver, ModeEnforcing,
		slog.New(slog.DiscardHandler), nil)
	require.NoError(t, err)

	var reached bool
	handler := middleware.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	const clusterID = "11111111-1111-1111-1111-111111111111"
	const nodeID = "22222222-2222-2222-2222-222222222222"

	tests := []struct {
		name    string
		subject string
		method  string
		path    string
		want    int
	}{
		// The reader role covers every GET through the read action group,
		// including endpoints the policy never names.
		{"reader lists clusters", reader, http.MethodGet, "/api/clusters", http.StatusOK},
		{"reader gets a cluster", reader, http.MethodGet, "/api/clusters/" + clusterID, http.StatusOK},
		{"reader reads node history", reader, http.MethodGet, "/api/nodes/" + nodeID + "/history", http.StatusOK},
		{"reader lists OS profiles", reader, http.MethodGet, "/api/osProfiles", http.StatusOK},

		// ...and no write.
		{"reader cannot update a cluster", reader, http.MethodPatch, "/api/clusters/" + clusterID, http.StatusForbidden},
		{"reader cannot create a cluster", reader, http.MethodPost, "/api/clusters", http.StatusForbidden},

		// The operator role nests reader, so it reads as well as writes.
		{"operator updates a cluster", operator, http.MethodPatch, "/api/clusters/" + clusterID, http.StatusOK},
		{"operator creates a node pool", operator, http.MethodPost, "/api/nodePools", http.StatusOK},
		{"operator reads, via the nested reader role", operator, http.MethodGet, "/api/clusters", http.StatusOK},

		// deleteNode is granted on its own rather than folded into write, so an
		// operator does not get it and a nodeOperator does.
		{"operator cannot delete a node", operator, http.MethodDelete, "/api/nodes/" + nodeID, http.StatusForbidden},
		{"node operator deletes a node", nodeOperator, http.MethodDelete, "/api/nodes/" + nodeID, http.StatusOK},
		{"node operator cannot write otherwise", nodeOperator, http.MethodPatch, "/api/clusters/" + clusterID, http.StatusForbidden},

		// A forbid beats every permit, so nobody deletes a cluster.
		{"operator cannot delete a cluster", operator, http.MethodDelete, "/api/clusters/" + clusterID, http.StatusForbidden},
		{"node operator cannot delete a cluster", nodeOperator, http.MethodDelete, "/api/clusters/" + clusterID, http.StatusForbidden},

		// A subject the bundle does not mention holds no role and matches no
		// permit.
		{"a stranger reads nothing", stranger, http.MethodGet, "/api/clusters", http.StatusForbidden},

		// Not a route, so never an operation, so refused before the engine.
		{"an unrouted path", operator, http.MethodGet, "/api/invented", http.StatusForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reached = false

			r := httptest.NewRequestWithContext(t.Context(), tt.method, tt.path, http.NoBody)
			r = r.WithContext(auth.ContextWithPrincipal(r.Context(), &auth.Principal{
				IssuerName: "kube-apiserver",
				Issuer:     "https://kubernetes.default.svc.cluster.local",
				Subject:    tt.subject,
			}))

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, r)

			assert.Equal(t, tt.want, rec.Code)
			assert.Equal(t, tt.want == http.StatusOK, reached,
				"whether the handler ran must agree with the status")
		})
	}
}

// The anonymous surface reaches the handler with no Principal and no decision,
// against a real engine that would otherwise refuse it.
func TestLiveAnonymousSurfaceIsUntouched(t *testing.T) {
	agent, err := NewCedarAgent(liveEndpoint(t), 0)
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
