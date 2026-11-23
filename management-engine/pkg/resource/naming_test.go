// SPDX-License-Identifier: Apache-2.0

package resource

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	strictjson "sigs.k8s.io/json"
)

func TestNamingFields(t *testing.T) {
	for _, tc := range []struct {
		canonical, snake string
	}{
		{"displayName", "display_name"}, {"vpcID", "vpc_id"}, {"nodeIDs", "node_ids"},
		{"creationTimestamp", "creation_timestamp"}, {"invalidJson", "invalid_json"},
	} {
		require.Equal(t, tc.snake, SnakeCase.FieldName(tc.canonical))
		require.Equal(t, tc.canonical, CamelCase.FieldName(tc.canonical))
	}
	for _, naming := range []Naming{SnakeCase, CamelCase} {
		require.NoError(t, naming.Validate())
	}
	require.Error(t, Naming(99).Validate())
	path, err := SnakeCase.FieldPath(reflect.TypeFor[widget](), "spec.nodePools.2.poolName")
	require.NoError(t, err)
	require.Equal(t, "spec.node_pools.2.pool_name", path)
	_, err = SnakeCase.FieldPath(reflect.TypeFor[widget](), "spec.hidden")
	require.Error(t, err)
}

func TestNamingCollision(t *testing.T) {
	type collision struct {
		VPCID          string `json:"vpcID"`
		VPCIDAlternate string `json:"vpcId"`
	}
	type inline struct{ collision }
	type nested struct {
		Children []inline `json:"children"`
	}
	for _, typ := range []reflect.Type{reflect.TypeFor[collision](), reflect.TypeFor[nested]()} {
		_, err := newJSONShape(typ, SnakeCase)
		require.ErrorContains(t, err, "collide")
		_, err = newJSONShape(typ, CamelCase)
		require.NoError(t, err)
	}
	require.Error(t, Register[collision](http.NewServeMux(), "/collisions", nil, nil, nil, SnakeCase))
	require.Error(t, Register[widget](http.NewServeMux(), "/widgets", nil, nil, nil, Naming(99)))
}

func TestNamingCustomScalar(t *testing.T) {
	type scalarView struct {
		ObservedAt metav1.Time `json:"observedAt"`
		RawBytes   []byte      `json:"rawBytes"`
	}
	shape, err := newJSONShape(reflect.TypeFor[scalarView](), SnakeCase)
	require.NoError(t, err)
	var decoded scalarView
	input := `{"observed_at":"2026-11-15T00:00:00Z","raw_bytes":"eA=="}`
	body, err := decodeNamedStrict(strings.NewReader(input), &decoded, shape)
	require.NoError(t, err)
	require.Equal(t, int64(1794700800), decoded.ObservedAt.Unix())
	require.Equal(t, []byte("x"), decoded.RawBytes)
	require.Equal(t, "2026-11-15T00:00:00Z", body["observedAt"])
	handler := operationHandler[scalarView]{shape: shape}
	data, err := handler.responseJSON(decoded)
	require.NoError(t, err)
	require.JSONEq(t, `{"observed_at":"2026-11-15T00:00:00Z","raw_bytes":"eA=="}`, string(data))
}

type decodeOnlyValue struct {
	DisplayName string `json:"displayName"`
}

func (v *decodeOnlyValue) UnmarshalJSON(data []byte) error {
	var input struct {
		DisplayName string `json:"displayName"`
		Alias       string `json:"alias"`
	}
	if err := json.Unmarshal(data, &input); err != nil {
		return err
	}
	v.DisplayName = input.DisplayName
	if input.Alias != "" {
		v.DisplayName = input.Alias
	}
	return nil
}

type decodeOnlyResource struct {
	Child decodeOnlyValue `json:"child"`
}

type decodeOnlyWriter struct {
	ResourceWriter[decodeOnlyResource]
	called bool
}

func (w *decodeOnlyWriter) Create(_ context.Context, value decodeOnlyResource) (decodeOnlyResource, error) {
	w.called = true
	return value, nil
}

func TestHTTPNamingCustomDecoderRejectsUndeclaredFields(t *testing.T) {
	for _, naming := range []Naming{CamelCase, SnakeCase} {
		writer := &decodeOnlyWriter{}
		mux := http.NewServeMux()
		require.NoError(t, Register(mux, "/custom", nil, writer, nil, naming, OpCreate))
		valid := `{"child":{"` + naming.FieldName("displayName") + `":"declared"}}`
		response := requestHTTP(mux, t.Context(), http.MethodPost, "/custom", valid, "")
		require.Equal(t, http.StatusCreated, response.Code, response.Body.String())
		require.True(t, writer.called)
		writer.called = false
		response = requestHTTP(mux, t.Context(), http.MethodPost, "/custom", `{"child":{"alias":"undeclared"}}`, "")
		require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
		require.False(t, writer.called, "custom decoding must not bypass the declared public fields")
	}
}

func TestNamingUnsignedResponse(t *testing.T) {
	type unsignedView struct {
		LargeCount uint64 `json:"largeCount"`
	}
	shape, err := newJSONShape(reflect.TypeFor[unsignedView](), SnakeCase)
	require.NoError(t, err)
	handler := operationHandler[unsignedView]{shape: shape}
	data, err := handler.responseJSON(unsignedView{LargeCount: 1<<64 - 1})
	require.NoError(t, err)
	require.Equal(t, `{"large_count":18446744073709551615}`, string(data))
}

func TestResponseJSONEnvelope(t *testing.T) {
	type envelope struct {
		Error
		TraceID     string           `json:"traceID"`
		RetryCounts map[string]int64 `json:"retryCounts"`
	}
	value := envelope{
		Status: http.StatusBadRequest, Code: "invalidRequest", Title: "Approved.",
		TraceID: "opaqueTraceID", RetryCounts: map[string]int64{"rawKey": 1<<53 + 1},
	}
	for _, tc := range []struct {
		name     string
		naming   Naming
		expected string
	}{
		{"snake", SnakeCase, `{"status":400,"code":"invalidRequest","title":"Approved.",` +
			`"trace_id":"opaqueTraceID","retry_counts":{"rawKey":9007199254740993}}`},
		{"camel", CamelCase, `{"status":400,"code":"invalidRequest","title":"Approved.",` +
			`"traceID":"opaqueTraceID","retryCounts":{"rawKey":9007199254740993}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shape, err := newJSONShape(reflect.TypeFor[envelope](), tc.naming)
			require.NoError(t, err)
			data, err := responseJSON(value, tc.naming, shape)
			require.NoError(t, err)
			var actual, expected map[string]any
			require.NoError(t, strictjson.UnmarshalCaseSensitivePreserveInts(data, &actual))
			require.NoError(t, strictjson.UnmarshalCaseSensitivePreserveInts([]byte(tc.expected), &expected))
			require.Equal(t, expected, actual)
		})
	}
}

func TestResponseJSONSerializationFailure(t *testing.T) {
	for _, naming := range []Naming{SnakeCase, CamelCase} {
		shape, err := newJSONShape(reflect.TypeFor[float64](), naming)
		require.NoError(t, err)
		data, err := responseJSON(math.Inf(1), naming, shape)
		require.Error(t, err)
		require.Nil(t, data)
	}
}

func TestHTTPNamingEmptyCollections(t *testing.T) {
	for _, tc := range []struct {
		naming Naming
		body   string
	}{
		{SnakeCase, `{"spec":{"node_ids":[],"named_pools":{},"display_name":null}}`},
		{CamelCase, `{"spec":{"nodeIDs":[],"namedPools":{},"displayName":null}}`},
	} {
		server := &namingServer{}
		mux := http.NewServeMux()
		require.NoError(t, Register(mux, "/widgets", server, server, nil, tc.naming, OpCreate))
		response := requestHTTP(mux, t.Context(), "POST", "/widgets", tc.body, "")
		require.Equal(t, http.StatusCreated, response.Code, response.Body.String())
		require.Equal(t, map[string]any{"spec": map[string]any{
			"nodeIDs": []any{}, "namedPools": map[string]any{}, "displayName": nil,
		}}, server.body)
	}
}

type namingServer struct {
	current widget
	body    map[string]any
	listed  ListOptions
	deleted DeleteOptions
	err     error
}

func (s *namingServer) Create(ctx context.Context, in widget) (widget, error) {
	s.body, s.current = requestOptions(ctx).body, in
	requestOptions(ctx).ETag = "canonicalVersion"
	return in, s.err
}

func (s *namingServer) Update(ctx context.Context, _ string, in widget) (widget, error) {
	return s.Create(ctx, in)
}

func (s *namingServer) Get(context.Context, string) (widget, error) { return s.current, s.err }

func (s *namingServer) List(_ context.Context, opts ListOptions) (List[widget], error) {
	s.listed = opts
	return List[widget]{Items: []widget{s.current}, PageKey: "opaqueCamelKey", HasMore: true}, s.err
}

func (s *namingServer) Delete(_ context.Context, _ string, opts DeleteOptions) error {
	s.deleted = opts
	return s.err
}

func TestHTTPNamingContract(t *testing.T) {
	for _, tc := range []struct {
		name                                                                   string
		naming                                                                 Naming
		body, alternateField, listQuery, alternateQuery, deleteQuery           string
		pageKey, hasMore, invalidRequest, invalidJSON, errorPath, invalidQuery string
	}{
		{
			name: "snake default", naming: SnakeCase,
			body: `{"metadata":{"name":"demo"},"spec":{"value":"camelValue","display_name":"display",` +
				`"vpc_id":"vpc","node_ids":["nodeA"],` +
				`"node_count":9007199254740993,"node_pools":[{"pool_name":"poolA","node_ids":["nodeB"]}],` +
				`"named_pools":{"rawKey/~&":{"pool_name":"poolC"}},"count":0,"enabled":false,"items":[],"note":null}}`,
			alternateField: "displayName",
			listQuery:      "page_size=1&page_key=opaqueCamelKey&field_selector=spec.display_name=display",
			alternateQuery: "pageSize=1", deleteQuery: "propagation_policy=foreground",
			pageKey: "page_key", hasMore: "has_more", invalidRequest: "invalid_request", invalidJSON: "invalid_json",
			errorPath: "/spec/named_pools/rawKey~1~0&/pool_name", invalidQuery: "invalid_query",
		},
		{
			name: "camel compatibility", naming: CamelCase,
			body: `{"metadata":{"name":"demo"},"spec":{"value":"camelValue","displayName":"display",` +
				`"vpcID":"vpc","nodeIDs":["nodeA"],` +
				`"nodeCount":9007199254740993,"nodePools":[{"poolName":"poolA","nodeIDs":["nodeB"]}],` +
				`"namedPools":{"rawKey/~&":{"poolName":"poolC"}},"count":0,"enabled":false,"items":[],"note":null}}`,
			alternateField: "display_name",
			listQuery:      "pageSize=1&pageKey=opaqueCamelKey&fieldSelector=spec.displayName=display",
			alternateQuery: "page_size=1", deleteQuery: "propagationPolicy=foreground",
			pageKey: "pageKey", hasMore: "hasMore", invalidRequest: "invalidRequest", invalidJSON: "invalidJson",
			errorPath: "/spec/namedPools/rawKey~1~0&/poolName", invalidQuery: "invalidQuery",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := &namingServer{}
			mux := http.NewServeMux()
			require.NoError(t, Register(mux, "/widgets", server, server, []string{"spec.displayName"}, tc.naming,
				OpCreate, OpGet, OpList, OpUpdate, OpDelete))
			handler := WithLimits(mux, ServerOptions{Naming: tc.naming})
			created := requestHTTP(handler, t.Context(), "POST", "/widgets", tc.body, "")
			require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
			require.Equal(t, `"canonicalVersion"`, created.Header().Get("ETag"))
			assertCanonicalNamingBody(t, server.body)
			var response map[string]any
			require.NoError(t, strictjson.UnmarshalCaseSensitivePreserveInts(created.Body.Bytes(), &response))
			spec := response["spec"].(map[string]any)
			require.Equal(t, int64(1<<53+1), spec[tc.naming.FieldName("nodeCount")])
			// Output matches the explicit wire input except that a nil pointer omits note.
			var expected map[string]any
			require.NoError(t, strictjson.UnmarshalCaseSensitivePreserveInts([]byte(tc.body), &expected))
			delete(expected["spec"].(map[string]any), "note")
			require.Equal(t, expected, response)
			for _, body := range []string{
				`{"spec":{"` + tc.alternateField + `":"alternate"}}`,
				`{"spec":{"value":"first","value":"second"}}`,
				`{"spec":{"Value":"uppercase"}}`,
				`{"spec":{"count":1.0}}`,
				`{"spec":{"count":1e0}}`,
				`{"spec":{"count":9007199254740993.0}}`,
			} {
				invalid := requestHTTP(handler, t.Context(), "POST", "/widgets", body, "")
				require.Equal(t, http.StatusBadRequest, invalid.Code)
				var problem Error
				require.NoError(t, json.Unmarshal(invalid.Body.Bytes(), &problem))
				require.Equal(t, tc.invalidRequest, problem.Code)
				require.Equal(t, tc.invalidJSON, problem.Errors[0].Code)
			}
			listed := requestHTTP(handler, t.Context(), "GET", "/widgets?"+tc.listQuery, "", "")
			require.Equal(t, http.StatusOK, listed.Code, listed.Body.String())
			expectedList := ListOptions{PageSize: 1, PageKey: "opaqueCamelKey", FieldSelector: "spec.displayName=display"}
			require.Equal(t, expectedList, server.listed)
			require.NoError(t, json.Unmarshal(listed.Body.Bytes(), &response))
			require.Equal(t, "opaqueCamelKey", response[tc.pageKey])
			require.Equal(t, true, response[tc.hasMore])
			invalid := requestHTTP(handler, t.Context(), "GET", "/widgets?"+tc.alternateQuery, "", "")
			require.Equal(t, http.StatusBadRequest, invalid.Code)
			selector := tc.naming.FieldName(fieldSelectorParameter) + "=spec." + tc.alternateField + "=display"
			invalid = requestHTTP(handler, t.Context(), "GET", "/widgets?"+selector, "", "")
			require.Equal(t, http.StatusBadRequest, invalid.Code, "selector paths accept only the selected spelling")
			deleted := requestHTTP(handler, t.Context(), "DELETE", "/widgets/demo?"+tc.deleteQuery, "", `"1"`)
			require.Equal(t, http.StatusAccepted, deleted.Code, deleted.Body.String())
			require.Equal(t, DeleteOptions{PropagationPolicy: "foreground"}, server.deleted)
			original := Invalid(FieldError{
				Field: "/spec/namedPools/rawKey~1~0&/poolName", Code: invalidQueryCode, Message: "Approved.",
			})
			server.err = original
			failed := requestHTTP(handler, t.Context(), "GET", "/widgets/demo", "", "")
			var problem Error
			require.NoError(t, json.Unmarshal(failed.Body.Bytes(), &problem))
			require.Equal(t, tc.invalidRequest, problem.Code)
			require.Equal(t, tc.errorPath, problem.Errors[0].Field)
			require.Equal(t, tc.invalidQuery, problem.Errors[0].Code)
			require.Equal(t, invalidRequestCode, original.(*Error).Code, "direct errors retain canonical codes")
			require.Equal(t, "/spec/namedPools/rawKey~1~0&/poolName", original.(*Error).Errors[0].Field)
		})
	}
}

func assertCanonicalNamingBody(t *testing.T, body map[string]any) {
	t.Helper()
	require.Equal(t, map[string]any{
		"metadata": map[string]any{"name": "demo"},
		"spec": map[string]any{
			"value": "camelValue", "displayName": "display", "vpcID": "vpc", "nodeIDs": []any{"nodeA"},
			"nodeCount": int64(1<<53 + 1), "nodePools": []any{map[string]any{"poolName": "poolA", "nodeIDs": []any{"nodeB"}}},
			"namedPools": map[string]any{"rawKey/~&": map[string]any{"poolName": "poolC"}},
			"count":      int64(0), "enabled": false, "items": []any{}, "note": nil,
		},
	}, body)
}

func TestHTTPNamingFallbackErrors(t *testing.T) {
	for _, tc := range []struct {
		naming                   Naming
		methodCode, deadlineCode string
	}{
		{SnakeCase, "method_not_allowed", "deadline_failed"},
		{CamelCase, "methodNotAllowed", "deadlineFailed"},
	} {
		mux := http.NewServeMux()
		require.NoError(t, Register[widget](mux, "/widgets", nil, nil, nil, tc.naming))
		response := requestHTTP(mux, t.Context(), "GET", "/widgets", "", "")
		require.Contains(t, response.Body.String(), tc.methodCode)
		response = httptest.NewRecorder()
		handler := WithLimits(mux, ServerOptions{Naming: tc.naming, RequestTimeout: time.Second})
		handler.ServeHTTP(response, httptest.NewRequest("GET", "/widgets", nil))
		require.Contains(t, response.Body.String(), tc.deadlineCode)
	}
}

func TestStandaloneErrorNaming(t *testing.T) {
	original := Invalid(FieldError{Field: "/metadata/name", Code: readOnlyCode, Message: "Approved."})
	for _, tc := range []struct {
		naming     Naming
		path, code string
	}{
		{SnakeCase, "", "read_only"},
		{CamelCase, "/metadata/name", "readOnly"},
	} {
		response := httptest.NewRecorder()
		WriteError(response, original, tc.naming)
		require.Equal(t, http.StatusBadRequest, response.Code)
		require.Equal(t, "application/problem+json", response.Header().Get("Content-Type"))
		require.Equal(t, "no-store", response.Header().Get("Cache-Control"))
		var problem Error
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
		require.Equal(t, tc.path, problem.Errors[0].Field)
		require.Equal(t, tc.code, problem.Errors[0].Code)
	}
	require.Equal(t, "/metadata/name", original.(*Error).Errors[0].Field)
}
