// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oapi-codegen/runtime"
	openapi_types "github.com/oapi-codegen/runtime/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/api"
)

// apiMux builds a mux carrying the real generated routes.
func apiMux(t *testing.T) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	api.HandlerWithOptions(nil, api.StdHTTPServerOptions{BaseRouter: mux, BaseURL: api.BasePath})
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

func TestMatchedSegmentPairsSegments(t *testing.T) {
	tests := []struct {
		name     string
		template string
		path     string
		param    string
		want     string
		found    bool
	}{
		{"a trailing wildcard", "/api/clusters/{clusterID}", "/api/clusters/abc", "clusterID", "abc", true},
		{"a wildcard mid-path", "/api/nodes/{nodeID}/history", "/api/nodes/n1/history", "nodeID", "n1", true},
		{"a segment kept whole by its escaping", "/api/clusters/{clusterID}", "/api/clusters/a%2Fb", "clusterID", "a%2Fb", true},
		{"a parameter the template does not carry", "/api/clusters/{clusterID}", "/api/clusters/abc", "siteID", "", false},
		{"more path than template", "/api/clusters/{clusterID}", "/api/clusters/abc/extra", "clusterID", "", false},
		{"less path than template", "/api/nodes/{nodeID}/history", "/api/nodes/n1", "nodeID", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := matchedSegment(tt.template, tt.path, tt.param)
			assert.Equal(t, tt.found, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

// The claim this middleware's position rests on, stated against the thing that
// actually decides it: the value the generated wrapper binds and hands the
// handler. Not r.PathValue — that is one decode short of the handler's argument,
// and the gap between them is where a request names two objects.
func TestTheQueryNamesTheObjectTheHandlerActsOn(t *testing.T) {
	const canonical = "11111111-1111-1111-1111-111111111111"

	tests := []struct {
		name string
		path string
	}{
		{"the canonical spelling", "/api/clusters/" + canonical},
		{"brace-wrapped", "/api/clusters/%7B" + canonical + "%7D"},
		{"bare 32 hex digits", "/api/clusters/11111111111111111111111111111111"},
		{"urn:uuid-prefixed", "/api/clusters/urn:uuid:" + canonical},
	}

	resolver, err := NewResolver(apiMux(t))
	require.NoError(t, err)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodDelete, tt.path, http.NoBody)

			q, err := resolver.Resolve(req)
			require.NoError(t, err)

			// Exactly what api/server.gen.go does before calling the handler.
			var bound openapi_types.UUID
			require.NoError(t, runtime.BindStyledParameterWithOptions(
				"simple", "clusterID", pathValueFor(t, req, "clusterID"), &bound,
				runtime.BindStyledParameterOptions{
					ParamLocation: runtime.ParamLocationPath, Explode: false, Required: true,
				},
			))

			assert.Equal(t, canonical, bound.String(), "the handler did not act on the object under test")
			assert.Equal(t, bound.String(), q.ResourceID,
				"the policy names %q and the handler acts on %q, so a forbid naming one misses the other",
				q.ResourceID, bound.String())
		})
	}
}

// A caller that escapes the escaping makes the two decodes disagree: the mux
// hands the wrapper one identifier and the wrapper parses another. Refused,
// because choosing between them is choosing which object the audit record lies
// about.
func TestResolveRefusesAPathThatNamesTwoObjects(t *testing.T) {
	// "%2531" is "%31" once decoded and "1" twice: the mux's value and the
	// wrapper's value differ by exactly the leading character.
	const doubled = "/api/clusters/%25311111111-1111-1111-1111-111111111111"

	resolver, err := NewResolver(apiMux(t))
	require.NoError(t, err)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodDelete, doubled, http.NoBody)

	// The premise: this is a request the router serves, and the handler acts on
	// a real cluster. If that ever stops being true this test is asserting
	// nothing, so it is checked rather than assumed.
	var bound openapi_types.UUID
	require.NoError(t, runtime.BindStyledParameterWithOptions(
		"simple", "clusterID", pathValueFor(t, req, "clusterID"), &bound,
		runtime.BindStyledParameterOptions{
			ParamLocation: runtime.ParamLocationPath, Explode: false, Required: true,
		},
	))
	require.Equal(t, "11111111-1111-1111-1111-111111111111", bound.String())

	_, err = resolver.Resolve(req)
	require.ErrorIs(t, err, ErrAmbiguousPath)
}

// An identifier that is not the format the route declares cannot name an
// object, so there is no question to ask about it.
func TestResolveRefusesAnIdentifierThatIsNotOne(t *testing.T) {
	resolver, err := NewResolver(apiMux(t))
	require.NoError(t, err)

	for _, path := range []string{
		"/api/clusters/not-a-uuid",
		// An encoded slash is one segment to the mux, so the request routes —
		// it just cannot name a cluster.
		"/api/clusters/11111111-1111-1111-1111-111111111111%2Fextra",
	} {
		t.Run(path, func(t *testing.T) {
			_, err := resolver.Resolve(httptest.NewRequestWithContext(t.Context(), http.MethodDelete, path, http.NoBody))
			require.ErrorIs(t, err, ErrInvalidQuery)
		})
	}
}

// http.ServeMux reports the pattern a dirty path would have matched and then
// answers 301 without running a handler. Authorizing that records an allow for
// an operation nobody performed.
func TestResolveRefusesWhatTheMuxWillOnlyRedirect(t *testing.T) {
	mux := apiMux(t)
	resolver, err := NewResolver(mux)
	require.NoError(t, err)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api//clusters", http.NoBody)

	// The premise, again measured rather than assumed: the mux does report a
	// pattern here, and does not serve the request.
	_, pattern := mux.Handler(req)
	require.Equal(t, http.MethodGet+" "+api.BasePath+"/clusters", pattern)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.GreaterOrEqual(t, rec.Code, http.StatusMultipleChoices, "the mux served this rather than redirecting it")
	require.Less(t, rec.Code, http.StatusBadRequest)
	require.Equal(t, api.BasePath+"/clusters", rec.Header().Get("Location"))

	_, err = resolver.Resolve(req)
	require.ErrorIs(t, err, ErrNoRoute)
}

// http.ServeMux serves HEAD off a GET pattern, so the action is the GET
// operation's. The query says so rather than carrying a method that contradicts
// its own action.
func TestHeadCarriesTheMethodOfTheRouteItIsServedBy(t *testing.T) {
	resolver, err := NewResolver(apiMux(t))
	require.NoError(t, err)

	q, err := resolver.Resolve(httptest.NewRequestWithContext(t.Context(), http.MethodHead, "/api/clusters", http.NoBody))
	require.NoError(t, err)

	assert.Equal(t, "listClusters", q.Action)
	assert.Equal(t, http.MethodGet, q.Method,
		"the action is the GET operation's, so the method in the same query must be too")
}

// pathValueFor gets the value the mux would give the handler, by letting the mux
// dispatch. r.PathValue is empty until then, which is the whole reason Resolve
// has to recover it another way.
func pathValueFor(t *testing.T, r *http.Request, param string) string {
	t.Helper()

	var value string
	mux := http.NewServeMux()
	_, pattern := apiMux(t).Handler(r)
	require.NotEmptyf(t, pattern, "%s does not route", r.URL)
	mux.HandleFunc(pattern, func(_ http.ResponseWriter, served *http.Request) {
		value = served.PathValue(param)
	})
	mux.ServeHTTP(httptest.NewRecorder(), r)
	return value
}
