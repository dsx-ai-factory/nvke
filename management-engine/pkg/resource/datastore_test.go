// SPDX-License-Identifier: Apache-2.0

package resource

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

// These unrelated, typed resources exercise the same two-method contract as an application.
type widget struct {
	Metadata widgetMetadata `json:"metadata"`
	Spec     widgetSpec     `json:"spec"`
	Status   *widgetStatus  `json:"status,omitempty"`
}
type widgetMetadata struct {
	Name string `json:"name"`
	UID  string `json:"uid,omitempty"`
}
type widgetSpec struct {
	Value   string    `json:"value"`
	Note    *string   `json:"note,omitempty"`
	Count   *int64    `json:"count,omitempty"`
	Enabled *bool     `json:"enabled,omitempty"`
	Items   *[]string `json:"items,omitempty"`
}
type widgetStatus struct {
	Counter int64 `json:"counter,omitempty"`
}
type internalWidget struct {
	metav1.TypeMeta `json:",inline"`
	Metadata        metav1.ObjectMeta `json:"metadata"`
	Spec            struct {
		widgetSpec
		Hidden string `json:"hidden,omitempty"`
	} `json:"spec"`
	Status *widgetStatus `json:"status,omitempty"`
}

var (
	_ ResourceHandler[internalWidget] = widget{}
	_ ResourceHandler[widget]         = internalWidget{}
)

func (w widget) Validate() error {
	if w.Spec.Value == "invalid-public" {
		return Invalid(FieldError{Field: "/spec/value", Code: "invalid", Message: "Invalid widget value."})
	}
	return nil
}

func (w internalWidget) Validate() error {
	if w.Spec.Value == "invalid-internal" {
		return Invalid(FieldError{Field: "/spec/value", Code: "invalid", Message: "Invalid internal value."})
	}
	return nil
}

func (w widget) Convert() (internalWidget, error) {
	var converted internalWidget
	data, err := json.Marshal(w)
	if err == nil {
		err = json.Unmarshal(data, &converted)
	}
	return converted, err
}

func (w internalWidget) Convert() (widget, error) {
	return widget{
		Metadata: widgetMetadata{Name: w.Metadata.Name, UID: string(w.Metadata.UID)},
		Spec:     w.Spec.widgetSpec, Status: w.Status,
	}, nil
}

func input(name, value string) widget {
	return widget{Metadata: widgetMetadata{Name: name}, Spec: widgetSpec{Value: value}}
}

func widgetInfo(verbs ...Operation) ResourceInfo {
	return ResourceInfo{
		GVK:      schema.GroupVersionKind{Group: "example.test", Version: "v1", Kind: "Widget"},
		Resource: "widgets", Scope: Namespaced, HasStatus: true, FieldManager: "facade-widget",
		Verbs: verbs,
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func jsonResponse(request *http.Request, status int, body any) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(string(data))), Request: request,
	}, nil
}

func widgetResources() *metav1.APIResourceList {
	return &metav1.APIResourceList{GroupVersion: "example.test/v1", APIResources: []metav1.APIResource{{
		Name: "widgets", Kind: "Widget", Namespaced: true, Verbs: []string{"get", "list", "patch", "delete"},
	}, {Name: "widgets/status", Kind: "Widget", Namespaced: true, Verbs: []string{"get", "patch", "update"}}}}
}

// fixtureConfig retains the production client-go transport and serialization path, without an API server.
func fixtureConfig(transport http.RoundTripper) ClientConfig {
	return ClientConfig{
		Datastore: &rest.Config{
			Host: "https://datastore.invalid", BearerToken: "example-token",
			WrapTransport: func(http.RoundTripper) http.RoundTripper {
				return roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == "/apis/example.test/v1" {
						return jsonResponse(r, http.StatusOK, widgetResources())
					}
					if transport == nil {
						return nil, fmt.Errorf("unexpected datastore request: %s", r.URL.Path)
					}
					return transport.RoundTrip(r)
				})
			},
		}, Surface: "test",
		Namespace: func(context.Context) (string, error) { return "org-a", nil },
	}
}

func statusCode(t *testing.T, err error, code int) {
	t.Helper()
	var problem *Error
	require.ErrorAs(t, err, &problem)
	require.Equal(t, code, problem.Status)
}

func TestWriteValidation(t *testing.T) {
	client, err := New[widget, internalWidget](fixtureConfig(nil), widgetInfo(OpCreate))
	require.NoError(t, err)
	for name, value := range map[string]widget{
		"public validation":   input("demo", "invalid-public"),
		"internal validation": input("demo", "invalid-internal"),
		"read-only status": {
			Metadata: widgetMetadata{Name: "demo"}, Spec: widgetSpec{Value: "public"},
			Status: &widgetStatus{},
		},
		"read-only UID": {Metadata: widgetMetadata{Name: "demo", UID: "injected"}, Spec: widgetSpec{Value: "public"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := client.Create(t.Context(), value)
			statusCode(t, err, 400)
		})
	}
}

func TestClusterScopeNeedsNoNamespace(t *testing.T) {
	for _, resolver := range []func(context.Context) (string, error){nil, func(context.Context) (string, error) {
		t.Error("cluster-scoped requests must not resolve a namespace")
		return "", Unauthorized()
	}} {
		cfg := fixtureConfig(nil)
		cfg.Namespace = resolver
		cfg.Datastore.WrapTransport = func(http.RoundTripper) http.RoundTripper {
			return roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/apis/example.test/v1" {
					resources := widgetResources()
					for i := range resources.APIResources {
						resources.APIResources[i].Namespaced = false
					}
					return jsonResponse(r, 200, resources)
				}
				require.Equal(t, "/apis/example.test/v1/widgets/demo", r.URL.Path)
				return jsonResponse(r, 200, map[string]any{
					"apiVersion": "example.test/v1", "kind": "Widget",
					"metadata": map[string]any{"name": "demo"}, "spec": map[string]any{"value": "public"},
				})
			})
		}
		info := widgetInfo(OpGet)
		info.Scope = ClusterScoped
		server, err := New[widget, internalWidget](cfg, info)
		require.NoError(t, err)
		_, err = server.Get(t.Context(), "demo")
		require.NoError(t, err)
	}
}

func TestRequiredResourceVersion(t *testing.T) {
	client, err := New[widget, internalWidget](fixtureConfig(nil), widgetInfo(OpUpdate, OpDelete))
	require.NoError(t, err)
	mux := http.NewServeMux()
	Register(mux, "/widgets", client, OpUpdate, OpDelete)
	for _, tc := range []struct {
		name, version, header string
		status                int
	}{
		{"missing", "", "", 428},
		{"zero", "0", `"0"`, 400},
		{"all zeroes", "000", `"000"`, 400},
		{"wildcard", "*", "*", 400},
		{"weak", `W/"1"`, `W/"1"`, 400},
		{"multiple", "1,2", `"1", "2"`, 400},
		{"whitespace", " 1", `" 1"`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := WithOptions(t.Context(), &RequestOptions{IfMatch: tc.version})
			_, err := client.Update(ctx, "demo", input("demo", "new"))
			statusCode(t, err, tc.status)
			statusCode(t, client.Delete(ctx, "demo", DeleteOptions{}), tc.status)
			for _, method := range []string{http.MethodPut, http.MethodDelete} {
				body := ""
				if method == http.MethodPut {
					body = `{"metadata":{"name":"demo"},"spec":{"value":"new"}}`
				}
				response := requestHTTP(mux, t.Context(), method, "/widgets/demo", body, tc.header)
				require.Equal(t, tc.status, response.Code, response.Body.String())
			}
		})
	}
	for _, header := range []string{`""`, "1"} {
		response := requestHTTP(mux, t.Context(), http.MethodDelete, "/widgets/demo", "", header)
		require.Equal(t, 400, response.Code, response.Body.String())
	}
	r := httptest.NewRequest(http.MethodDelete, "/widgets/demo", nil)
	r.Header.Add("If-Match", `"1"`)
	r.Header.Add("If-Match", `"2"`)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, r)
	require.Equal(t, 400, response.Code, response.Body.String())
}

func TestDatastorePageKeys(t *testing.T) {
	for _, tc := range []struct {
		name, pageKey, next string
		reason              metav1.StatusReason
		status, want        int
		code, field         string
	}{
		{name: "unchanged", pageKey: "opaque+/=", next: "next+/="},
		{name: "last page", pageKey: "last"},
		{
			name: "malformed", pageKey: "malformed", reason: metav1.StatusReasonBadRequest, status: 400, want: 400,
			code: "invalidRequest", field: "pageKey",
		},
		{
			name: "expired", pageKey: "expired", reason: metav1.StatusReasonExpired, status: 410, want: 410,
			code: "pageExpired",
		},
		{
			name: "too long", pageKey: strings.Repeat("x", maxPageKeyBytes+1), want: 400,
			code: "invalidRequest", field: "pageKey",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			cfg := fixtureConfig(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				require.Equal(t, "/apis/example.test/v1/namespaces/org-a/widgets", r.URL.Path)
				require.Equal(t, tc.pageKey, r.URL.Query().Get("continue"))
				require.Equal(t, "2", r.URL.Query().Get("limit"))
				if tc.status != 0 {
					status := &metav1.Status{Status: "Failure", Reason: tc.reason, Code: int32(tc.status)}
					status.APIVersion = "v1"
					status.Kind = "Status"
					return jsonResponse(r, tc.status, status)
				}
				return jsonResponse(r, 200, map[string]any{
					"apiVersion": "example.test/v1", "kind": "WidgetList",
					"metadata": map[string]any{"continue": tc.next}, "items": []any{},
				})
			}))
			client, err := New[widget, internalWidget](cfg, widgetInfo(OpList))
			require.NoError(t, err)
			page, err := client.List(t.Context(), ListOptions{PageSize: 2, PageKey: tc.pageKey})
			mux := http.NewServeMux()
			Register(mux, "/widgets", client, OpList)
			path := "/widgets?pageSize=2&pageKey=" + url.QueryEscape(tc.pageKey)
			response := requestHTTP(mux, t.Context(), http.MethodGet, path, "", "")
			if tc.want == 0 {
				require.NoError(t, err)
				require.Equal(t, tc.next, page.PageKey)
				require.Equal(t, tc.next != "", page.HasMore)
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
				wantJSON := fmt.Sprintf(`{"items":[],"pageKey":%q,"hasMore":%t}`, tc.next, tc.next != "")
				require.JSONEq(t, wantJSON, response.Body.String())
			} else {
				statusCode(t, err, tc.want)
				require.Equal(t, tc.want, response.Code, response.Body.String())
				var problem Error
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
				require.Equal(t, tc.code, problem.Code)
				if tc.field != "" {
					require.Len(t, problem.Errors, 1)
					require.Equal(t, tc.field, problem.Errors[0].Field)
					require.Equal(t, "invalid", problem.Errors[0].Code)
				} else {
					require.Empty(t, problem.Errors)
				}
			}
			if tc.name == "too long" {
				require.Zero(t, calls)
			} else {
				require.Equal(t, 2, calls)
			}
		})
	}
}
