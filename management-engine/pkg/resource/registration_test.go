// SPDX-License-Identifier: Apache-2.0

package resource

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

func TestRegistration(t *testing.T) {
	info := widgetInfo(OpGet)
	api := NewRegistration[widget, internalWidget]("/widgets", info)
	info.Verbs[0] = OpDelete // Registration must retain its original verbs.

	mux := http.NewServeMux()
	require.Error(t, api.Mount(mux, ClientConfig{}))
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/widgets", nil))
	require.Equal(t, http.StatusNotFound, response.Code)
	var calls int
	cfg := fixtureConfig(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		require.Equal(t, "/apis/example.test/v1/namespaces/org-a/widgets/demo", r.URL.Path)
		require.Equal(t, "Bearer example-token", r.Header.Get("Authorization"))
		require.Equal(t, "trusted-service-user", r.Header.Get("Impersonate-User"))
		for name := range r.Header {
			if strings.HasPrefix(strings.ToLower(name), "impersonate-") {
				require.Equal(t, "Impersonate-User", name)
			}
		}
		return jsonResponse(r, 200, map[string]any{
			"apiVersion": "example.test/v1", "kind": "Widget",
			"metadata": map[string]any{"name": "demo", "uid": "widget-uid", "resourceVersion": "1", "namespace": "org-a"},
			"spec":     map[string]any{"value": "public", "hidden": "private"},
		})
	}))
	cfg.Datastore.Impersonate = rest.ImpersonationConfig{UserName: "trusted-service-user"}
	directInfo := widgetInfo(OpGet)
	server, err := New[widget, internalWidget](cfg, directInfo)
	require.NoError(t, err)
	directInfo.Verbs[0] = OpDelete
	_, err = server.Get(t.Context(), "demo")
	require.NoError(t, err)
	statusCode(t, server.Delete(t.Context(), "demo", DeleteOptions{}), http.StatusMethodNotAllowed)
	require.NoError(t, api.Mount(mux, cfg))
	req := httptest.NewRequest(http.MethodGet, "/widgets/demo", nil)
	req.Header.Set("Impersonate-User", "other-user")
	response = httptest.NewRecorder()
	WithLimits(mux, ServerOptions{}).ServeHTTP(&deadlineRecorder{ResponseRecorder: response}, req)
	require.Equal(t, 200, response.Code, response.Body.String())
	require.JSONEq(t, `{"metadata":{"name":"demo","uid":"widget-uid"},"spec":{"value":"public"}}`, response.Body.String())
	require.Equal(t, `"1"`, response.Header().Get("ETag"))
	require.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	require.Equal(t, 2, calls)
}

func TestEmptyOperations(t *testing.T) {
	for name, verbs := range map[string][]Operation{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			server, err := New[widget, internalWidget](fixtureConfig(nil), widgetInfo(verbs...))
			require.NoError(t, err)
			mux := http.NewServeMux()
			Register(mux, "/widgets", server, verbs...)
			ctx := t.Context()
			for _, operation := range []struct {
				method, path string
				call         func() error
			}{
				{"POST", "/widgets", func() error { _, err := server.Create(ctx, input("disabled", "valid")); return err }},
				{"GET", "/widgets/disabled", func() error { _, err := server.Get(ctx, "disabled"); return err }},
				{"GET", "/widgets", func() error { _, err := server.List(ctx, ListOptions{}); return err }},
				{"PUT", "/widgets/disabled", func() error {
					_, err := server.Update(ctx, "disabled", input("disabled", "valid"))
					return err
				}},
				{"DELETE", "/widgets/disabled", func() error { return server.Delete(ctx, "disabled", DeleteOptions{}) }},
			} {
				t.Run(operation.method+operation.path, func(t *testing.T) {
					statusCode(t, operation.call(), http.StatusMethodNotAllowed)
					response := requestHTTP(mux, ctx, operation.method, operation.path, "", "")
					require.Equal(t, http.StatusMethodNotAllowed, response.Code, response.Body.String())
					require.Equal(t, []string{""}, response.Header().Values("Allow"))
				})
			}
		})
	}
}

func ExampleNew() {
	cfg := fixtureConfig(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(r, 200, map[string]any{
			"apiVersion": "example.test/v1", "kind": "Widget",
			"metadata": map[string]any{"name": "demo", "uid": "widget-uid", "resourceVersion": "1"},
			"spec":     map[string]any{"value": "public"},
		})
	}))
	server, err := New[widget, internalWidget](cfg, widgetInfo(OpGet))
	if err != nil {
		panic(err)
	}
	opts := &RequestOptions{}
	value, err := server.Get(WithOptions(context.Background(), opts), "demo")
	if err != nil {
		panic(err)
	}
	fmt.Println(value.Spec.Value, opts.ETag != "")

	mux := http.NewServeMux()
	api := NewRegistration[widget, internalWidget]("/widgets", widgetInfo(OpGet))
	if err := api.Mount(mux, cfg); err != nil {
		panic(err)
	}
	response := requestHTTP(WithLimits(mux, ServerOptions{}), context.Background(), "GET", "/widgets/demo", "", "")
	fmt.Println(response.Code)
	// Output:
	// public true
	// 200
}

func TestDiscoveryCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*metav1.APIResourceList)
		verbs  []Operation
		status int
		want   string
	}{
		{name: "compatible", verbs: []Operation{OpGet}},
		{name: "unserved version", status: 404, want: "discovering resource"},
		{
			name: "wrong version", want: "not discovered",
			change: func(r *metav1.APIResourceList) { r.GroupVersion = "example.test/v2" },
		},
		{
			name: "resource absent", want: "not discovered",
			change: func(r *metav1.APIResourceList) { r.APIResources = nil },
		},
		{
			name: "kind mismatch", want: "scope or kind",
			change: func(r *metav1.APIResourceList) { r.APIResources[0].Kind = "Other" },
		},
		{
			name: "scope mismatch", want: "scope or kind",
			change: func(r *metav1.APIResourceList) { r.APIResources[0].Namespaced = false },
		},
		{
			name: "missing status", want: "status capability",
			change: func(r *metav1.APIResourceList) { r.APIResources = r.APIResources[:1] },
		},
		{
			name: "patch unsupported", verbs: []Operation{OpCreate}, want: "does not support",
			change: func(r *metav1.APIResourceList) { r.APIResources[0].Verbs = []string{"get", "list", "delete"} },
		},
		{
			name: "update needs get", verbs: []Operation{OpUpdate}, want: "required reads",
			change: func(r *metav1.APIResourceList) { r.APIResources[0].Verbs = []string{"patch"} },
		},
		{
			name: "delete needs no get", verbs: []Operation{OpDelete},
			change: func(r *metav1.APIResourceList) { r.APIResources[0].Verbs = []string{"delete"} },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resources := widgetResources()
			if tc.change != nil {
				tc.change(resources)
			}
			var calls int
			cfg := fixtureConfig(nil)
			cfg.Datastore.WrapTransport = func(http.RoundTripper) http.RoundTripper {
				return roundTripFunc(func(r *http.Request) (*http.Response, error) {
					calls++
					require.Equal(t, "/apis/example.test/v1", r.URL.Path)
					if tc.status != 0 {
						return jsonResponse(r, tc.status, &metav1.Status{
							Status: "Failure", Reason: metav1.StatusReasonNotFound, Code: int32(tc.status),
						})
					}
					return jsonResponse(r, 200, resources)
				})
			}
			info := widgetInfo(tc.verbs...)
			_, err := New[widget, internalWidget](cfg, info)
			if tc.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.want)
			}
			require.Equal(t, 1, calls, "discovery needs no CRD read permission")
		})
	}
}
