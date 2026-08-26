// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/api"
)

// apiMux builds a mux carrying the real generated routes.
func apiMux(t *testing.T) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	api.HandlerWithOptions(nil, api.StdHTTPServerOptions{BaseRouter: mux, BaseURL: "/api"})
	return mux
}

func TestNewResolverRejectsANilMux(t *testing.T) {
	_, err := NewResolver(nil)
	require.Error(t, err, "a nil mux is a wiring bug and must not produce a working resolver")
}

// The claim the outer middleware position rests on: from outside the mux, every
// operation resolves to its own action and to the identifier the handler would
// see. A regression here is a request authorized as the wrong operation.
func TestResolveMatchesEveryOperation(t *testing.T) {
	resolver, err := NewResolver(apiMux(t))
	require.NoError(t, err)

	const id = "11111111-1111-1111-1111-111111111111"

	for pattern, op := range operations {
		method, path, ok := strings.Cut(pattern, " ")
		require.True(t, ok)

		concrete := path
		if op.IDParam != "" {
			concrete = strings.ReplaceAll(path, "{"+op.IDParam+"}", id)
		}

		q, err := resolver.Resolve(httptest.NewRequestWithContext(t.Context(), method, concrete, http.NoBody))
		require.NoErrorf(t, err, "%s did not resolve", pattern)

		assert.Equalf(t, op.Action, q.Action, "%s resolved to the wrong action", pattern)
		assert.Equalf(t, method, q.Method, "%s resolved to the wrong method", pattern)

		if op.IDParam == "" {
			assert.Equalf(t, CollectionType, q.ResourceType, "%s should authorize against a collection", pattern)
			assert.Equalf(t, op.Collection, q.ResourceID, "%s named the wrong collection", pattern)
			continue
		}
		assert.Equalf(t, op.ResourceType, q.ResourceType, "%s resolved to the wrong resource type", pattern)
		assert.Equalf(t, id, q.ResourceID, "%s did not recover the path value", pattern)
	}
}

// The identifier the outer position derives must be the one the handler is
// given, or a policy scoped to an object is scoped to a different object than
// the request acts on.
func TestResolvedIdentifierMatchesThePathValue(t *testing.T) {
	const id = "22222222-2222-2222-2222-222222222222"

	var handlerSaw string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/clusters/{clusterID}", func(_ http.ResponseWriter, r *http.Request) {
		handlerSaw = r.PathValue("clusterID")
	})

	resolver, err := NewResolver(mux)
	require.NoError(t, err)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/clusters/"+id, http.NoBody)
	q, err := resolver.Resolve(req)
	require.NoError(t, err)

	mux.ServeHTTP(httptest.NewRecorder(), req)

	assert.Equal(t, id, handlerSaw, "the handler did not see the identifier under test")
	assert.Equal(t, handlerSaw, q.ResourceID,
		"the outer position and the handler disagree about which object the request acts on")
}

func TestResolveRefusesWhatDoesNotRoute(t *testing.T) {
	resolver, err := NewResolver(apiMux(t))
	require.NoError(t, err)

	tests := []struct {
		name   string
		method string
		path   string
	}{
		{"an unknown path", http.MethodGet, "/api/there-is-no-such-thing"},
		{"a method the route does not register", http.MethodDelete, "/api/sites/abc"},
		{"a path above the API", http.MethodGet, "/not-the-api"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := resolver.Resolve(httptest.NewRequestWithContext(t.Context(), tt.method, tt.path, http.NoBody))
			require.ErrorIs(t, err, ErrNoRoute,
				"a request that routes nowhere must be refused, not authorized against a guess")
		})
	}
}

// A route the router serves and the table does not know is the drift AZ-3
// exists to prevent. It is a denial rather than a pass, and it says which.
func TestResolveRefusesARouteMissingFromTheTable(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/somethingNew", func(http.ResponseWriter, *http.Request) {})

	resolver, err := NewResolver(mux)
	require.NoError(t, err)

	_, err = resolver.Resolve(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/somethingNew", http.NoBody))
	require.ErrorIs(t, err, ErrUnknownOperation)
}

func TestPathValuePairsSegments(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		path    string
		param   string
		want    string
	}{
		{"a trailing wildcard", "GET /api/clusters/{clusterID}", "/api/clusters/abc", "clusterID", "abc"},
		{"a wildcard mid-path", "GET /api/nodes/{nodeID}/history", "/api/nodes/n1/history", "nodeID", "n1"},
		{"a parameter the pattern does not carry", "GET /api/clusters/{clusterID}", "/api/clusters/abc", "siteID", ""},
		{"more path than pattern", "GET /api/clusters/{clusterID}", "/api/clusters/abc/extra", "clusterID", ""},
		{"less path than pattern", "GET /api/nodes/{nodeID}/history", "/api/nodes/n1", "nodeID", ""},
		{"a pattern with no method", "/api/clusters/{clusterID}", "/api/clusters/abc", "clusterID", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, pathValue(tt.pattern, tt.path, tt.param))
		})
	}
}
