// SPDX-License-Identifier: Apache-2.0

package resource

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func publicSpec(t *testing.T, surface string) *openapi3.T {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromFile("../../api/" + surface + ".openapi.yaml")
	require.NoError(t, err)
	return doc
}

func specParameter(t *testing.T, operation *openapi3.Operation, location, name string) *openapi3.Parameter {
	t.Helper()
	parameter := operation.Parameters.GetByInAndName(location, name)
	require.NotNil(t, parameter, "missing %s parameter %s", location, name)
	return parameter
}

// Exercise the same validators used by HTTP dispatch, using the artifact consumers receive.
func TestOpenAPITransportConstraints(t *testing.T) {
	for surface, path := range map[string]string{"tenant": "/v1/clusters", "operator": "/v1/machine-images"} {
		t.Run(surface, func(t *testing.T) {
			doc := publicSpec(t, surface)
			require.Empty(t, doc.Security)
			require.Empty(t, doc.Components.SecuritySchemes)
			limits := WithLimits(nil, ServerOptions{}).(limitsHandler).opts
			require.EqualValues(t, limits.MaxBodyBytes, doc.Extensions["x-max-request-body-bytes"])
			require.EqualValues(t, limits.RequestTimeout.Seconds(), doc.Extensions["x-request-timeout-seconds"])
			for _, name := range []string{
				"a", "a.b-1", strings.Repeat("a", 253), "", "A", "a_b", "-a", "a-", "a..b", "é", strings.Repeat("a", 254),
			} {
				err := doc.Components.Schemas["ResourceName"].Value.VisitJSON(name)
				require.Equal(t, validName(name) == nil, err == nil, "name %q: %v", name, err)
			}
			precondition := specParameter(t, doc.Paths.Value(path+"/{name}").Put, openapi3.ParameterInHeader, "If-Match")
			require.True(t, precondition.Required)
			for _, value := range []string{
				`"1"`, `"opaque-value"`, `"010"`, `"0"`, `"000"`, `""`, `*`, `W/"1"`,
				`"1", "2"`, `"with space"`, "\"a\tb\"", "unquoted",
			} {
				request := httptest.NewRequest(http.MethodPut, path+"/demo", nil)
				request.Header.Set("If-Match", value)
				version, err := versionHeader(request)
				if err == nil {
					err = expectedVersion(version)
				}
				schemaErr := precondition.Schema.Value.VisitJSON(value)
				require.Equal(t, err == nil, schemaErr == nil, "If-Match %q: API %v, schema %v", value, err, schemaErr)
			}
			pageSize := specParameter(t, doc.Paths.Value(path).Get, openapi3.ParameterInQuery, "page_size").Schema.Value
			defaults := ListOptions{}
			_, err := listSelector(&defaults, nil)
			require.NoError(t, err)
			require.EqualValues(t, defaults.PageSize, pageSize.Default)
			for _, size := range []int{-1, 1, 100, 500, 501} {
				_, err := listSelector(&ListOptions{PageSize: size}, nil)
				require.Equal(t, err == nil, pageSize.VisitJSON(size) == nil, "pageSize %d", size)
			}
			cfg := fixtureConfig(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return jsonResponse(r, http.StatusOK, map[string]any{})
			}))
			client, err := New[widget, internalWidget](cfg, widgetInfo(OpDelete))
			require.NoError(t, err)
			policy := specParameter(t, doc.Paths.Value(path+"/{name}").Delete, openapi3.ParameterInQuery, "propagation_policy")
			require.True(t, policy.AllowEmptyValue)
			require.Equal(t, "background", policy.Schema.Value.Default)
			for _, value := range []string{"", "background", "foreground", "orphan", "Foreground"} {
				err := client.Delete(WithOptions(t.Context(), &RequestOptions{IfMatch: "1"}), "demo",
					DeleteOptions{PropagationPolicy: value})
				require.Equal(t, err == nil, policy.Schema.Value.VisitJSON(value) == nil, "propagationPolicy %q", value)
			}
		})
	}
}

func TestOpenAPIByteLimits(t *testing.T) {
	doc := publicSpec(t, "tenant")
	list := doc.Paths.Value("/v1/node-profiles").Get
	for _, name := range []string{pageKeyParameter, fieldSelectorParameter} {
		schema := specParameter(t, list, openapi3.ParameterInQuery, SnakeCase.FieldName(name)).Schema.Value
		limit := int(schema.Extensions["x-max-utf8-bytes"].(float64))
		for _, extra := range []int{0, 1} {
			opts := ListOptions{}
			if name == pageKeyParameter {
				opts.PageKey = strings.Repeat("x", limit+extra)
			} else {
				prefix := "spec.tag="
				length := limit + extra - len(prefix)
				opts.FieldSelector = prefix + strings.Repeat("é", length/2) + strings.Repeat("x", length%2)
			}
			_, err := listSelector(&opts, []string{"spec.tag"})
			require.Equal(t, extra == 0, err == nil, "%s at limit + %d: %v", name, extra, err)
		}
	}
}

func TestOpenAPIWriteMetadata(t *testing.T) {
	doc := publicSpec(t, "tenant")
	server := &resourceServer[widget, internalWidget]{datastore: &datastore{info: widgetInfo(OpCreate, OpUpdate)}}
	for _, operation := range []Operation{OpCreate, OpUpdate} {
		request := doc.Paths.Value("/v1/clusters").Post.RequestBody
		if operation == OpUpdate {
			request = doc.Paths.Value("/v1/clusters/{name}").Put.RequestBody
		}
		metadataSchema := request.Value.Content["application/json"].Schema.Value.Properties["metadata"].Value
		for _, metadata := range []string{
			`{"name":"demo"}`, `{"name":"demo","uid":""}`, `{"name":"demo","uid":null}`,
			`{"name":"demo","uid":"identity"}`, `{"name":"demo","namespace":"other"}`, `{"name":""}`, `{}`,
			`{"name":"demo","creationTimestamp":"2026-01-01T00:00:00Z"}`,
		} {
			var input widget
			body, err := decodeStrict(bytes.NewBufferString(`{"metadata":`+metadata+`,"spec":{"value":"x"}}`), &input)
			if err == nil {
				_, err = server.intent(WithOptions(t.Context(), &RequestOptions{body: body}), operation, input)
			}
			var object map[string]any
			require.NoError(t, json.Unmarshal([]byte(metadata), &object))
			schemaErr := metadataSchema.VisitJSON(object, openapi3.VisitAsRequest())
			require.Equal(t, err == nil, schemaErr == nil, "%s %s: API %v, schema %v", operation, metadata, err, schemaErr)
		}
	}
}

func TestOpenAPIProblemResponses(t *testing.T) {
	for _, surface := range []string{"tenant", "operator"} {
		t.Run(surface, func(t *testing.T) {
			problemSchema := publicSpec(t, surface).Components.Schemas["Problem"].Value
			check := func(response *httptest.ResponseRecorder) {
				t.Helper()
				require.GreaterOrEqual(t, response.Code, http.StatusBadRequest)
				var body map[string]any
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
				require.NoError(t, problemSchema.VisitJSON(body), response.Body.String())
			}
			for _, reason := range []metav1.StatusReason{
				metav1.StatusReasonUnauthorized, metav1.StatusReasonForbidden, metav1.StatusReasonNotFound,
				metav1.StatusReasonAlreadyExists, metav1.StatusReasonExpired, metav1.StatusReasonConflict,
				metav1.StatusReasonInvalid, metav1.StatusReasonBadRequest, metav1.StatusReasonServiceUnavailable,
			} {
				response := httptest.NewRecorder()
				WriteError(response, translate[widget](&apierrors.StatusError{ErrStatus: metav1.Status{Reason: reason}}), SnakeCase)
				check(response)
			}
			for _, err := range []error{
				Invalid(), invalidSelector(), expectedVersion(""), expectedVersion("0"),
				projectionFailure(errors.New("private projection details")), errors.New("private failure details"),
			} {
				response := httptest.NewRecorder()
				WriteError(response, err, SnakeCase)
				check(response)
			}
			server := &namingServer{}
			mux := http.NewServeMux()
			require.NoError(t, Register(mux, "/widgets", server, server, nil, SnakeCase, OpCreate, OpGet, OpUpdate))
			for _, tc := range []struct{ method, path, body, etag string }{
				{http.MethodPost, "/widgets", `{"unknown":true}`, ""},
				{http.MethodGet, "/widgets/demo", `{}`, ""},
				{http.MethodGet, "/widgets/demo?unknown=value", "", ""},
				{http.MethodPut, "/widgets/demo", `{}`, "unquoted"},
				{http.MethodDelete, "/widgets/demo", "", ""},
			} {
				check(requestHTTP(mux, t.Context(), tc.method, tc.path, tc.body, tc.etag))
			}
		})
	}
}
