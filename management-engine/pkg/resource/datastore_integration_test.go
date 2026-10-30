//go:build integration_test

// SPDX-License-Identifier: Apache-2.0

package resource

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// widgetCRD supplies real admission, defaults and SSA ownership without a provider dependency.
func widgetCRD(status bool) *extv1.CustomResourceDefinition {
	crd := &extv1.CustomResourceDefinition{Name: "widgets.example.test", Spec: extv1.CustomResourceDefinitionSpec{
		Group: "example.test", Scope: extv1.NamespaceScoped,
		Names: extv1.CustomResourceDefinitionNames{
			Plural: "widgets", Singular: "widget", Kind: "Widget", ListKind: "WidgetList",
		},
		Versions: []extv1.CustomResourceDefinitionVersion{{
			Name: "v1", Served: true, Storage: true,
			Schema: &extv1.CustomResourceValidation{OpenAPIV3Schema: &extv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"spec": {Type: "object", Required: []string{"value"}, Properties: map[string]extv1.JSONSchemaProps{
						"value": {Type: "string", MinLength: new(int64(1))}, "note": {Type: "string"},
						"count": {Type: "integer", Format: "int64"}, "enabled": {Type: "boolean"},
						"items":  {Type: "array", Items: &extv1.JSONSchemaPropsOrArray{Schema: &extv1.JSONSchemaProps{Type: "string"}}},
						"hidden": {Type: "string", Default: &extv1.JSON{Raw: []byte(`"private-default"`)}},
					}},
					"status": {Type: "object", Properties: map[string]extv1.JSONSchemaProps{
						"counter": {Type: "integer", Format: "int64"},
					}},
				},
			}},
		}},
	}}
	if status {
		crd.Spec.Versions[0].Subresources = &extv1.CustomResourceSubresources{
			Status: &extv1.CustomResourceSubresourceStatus{},
		}
	}
	return crd
}

type (
	namespaceKey struct{}
	fixture      struct {
		cfg   ClientConfig
		info  ResourceInfo
		admin dynamic.Interface
		ctx   context.Context
	}
)

// setup reuses envtest to exercise real admission, ownership and resourceVersion conflicts.
func setup(t *testing.T, status bool) *fixture {
	t.Helper()
	environment := &envtest.Environment{
		CRDs: []*extv1.CustomResourceDefinition{widgetCRD(status)}, UseExistingCluster: new(false),
	}
	cfg, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })
	core, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	for _, namespace := range []string{"org-a", "org-b"} {
		_, err = core.CoreV1().Namespaces().Create(t.Context(), &corev1.Namespace{Name: namespace}, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	clientConfig := fixtureConfig(nil)
	clientConfig.Datastore = cfg
	clientConfig.Namespace = func(ctx context.Context) (string, error) {
		namespace, ok := ctx.Value(namespaceKey{}).(string)
		if !ok {
			return "", Unauthorized()
		}
		return namespace, nil
	}
	admin, err := dynamic.NewForConfig(cfg)
	require.NoError(t, err)
	info := widgetInfo(OpCreate, OpGet, OpList, OpUpdate, OpDelete)
	info.HasStatus = status
	return &fixture{cfg: clientConfig, info: info, admin: admin, ctx: t.Context()}
}

func (f *fixture) client(t *testing.T, wrap ...func(http.RoundTripper) http.RoundTripper) ResourceServer[widget] {
	t.Helper()
	cfg := f.cfg
	cfg.Datastore = rest.CopyConfig(cfg.Datastore)
	if len(wrap) != 0 {
		cfg.Datastore.WrapTransport = wrap[0]
	}
	client, err := New[widget, internalWidget](cfg, f.info)
	require.NoError(t, err)
	return client
}

func (f *fixture) as(namespace string) context.Context {
	return context.WithValue(f.ctx, namespaceKey{}, namespace)
}

func (f *fixture) resource() dynamic.ResourceInterface {
	return f.admin.Resource(f.info.GVK.GroupVersion().WithResource(f.info.Resource)).Namespace("org-a")
}

func (f *fixture) stored(t *testing.T, name string) *unstructured.Unstructured {
	t.Helper()
	obj, err := f.resource().Get(f.ctx, name, metav1.GetOptions{})
	require.NoError(t, err)
	return obj
}

func TestResourceCRUDL(t *testing.T) {
	for _, status := range []bool{true, false} {
		t.Run(fmt.Sprintf("status=%t", status), func(t *testing.T) {
			f := setup(t, status)
			client := f.client(t)
			opts := &RequestOptions{}
			ctx := f.as("org-a")
			created, err := client.Create(WithOptions(ctx, opts), input("one", "first"))
			require.NoError(t, err)
			require.NotEmpty(t, created.Metadata.UID)
			require.NotEmpty(t, opts.ETag)
			stored := f.stored(t, "one")
			require.Equal(t, stored.GetResourceVersion(), opts.ETag)
			require.Equal(t, map[string]any{"value": "first", "hidden": "private-default"}, stored.Object["spec"])
			_, err = client.Create(ctx, input("one", "duplicate"))
			statusCode(t, err, 409)
			require.Equal(t, stored.Object, f.stored(t, "one").Object)
			opts = &RequestOptions{}
			got, err := client.Get(WithOptions(ctx, opts), "one")
			require.NoError(t, err)
			require.Equal(t, created, got)
			require.Equal(t, stored.GetResourceVersion(), opts.ETag)
			version := opts.ETag
			_, err = client.Update(WithOptions(ctx, &RequestOptions{IfMatch: version}), "one", input("other", "second"))
			statusCode(t, err, 400)
			wrongUID := input("one", "second")
			wrongUID.Metadata.UID = "wrong"
			_, err = client.Update(WithOptions(ctx, &RequestOptions{IfMatch: version}), "one", wrongUID)
			statusCode(t, err, 400)
			opts = &RequestOptions{IfMatch: version}
			updated, err := client.Update(WithOptions(ctx, opts), "one", input("one", "second"))
			require.NoError(t, err)
			require.Equal(t, "second", updated.Spec.Value)
			_, err = client.Update(WithOptions(ctx, &RequestOptions{IfMatch: version}), "one", input("one", "stale"))
			statusCode(t, err, 412)
			version = opts.ETag
			require.NoError(t, client.Delete(WithOptions(ctx, &RequestOptions{IfMatch: version}), "one", DeleteOptions{}))
			_, err = client.Update(WithOptions(ctx, &RequestOptions{IfMatch: version}), "one", input("one", "must-not-create"))
			statusCode(t, err, 404)
			_, err = client.Create(ctx, input("one", "recreated"))
			require.NoError(t, err)
			_, err = client.Update(WithOptions(ctx, &RequestOptions{IfMatch: version}), "one", input("one", "stale"))
			statusCode(t, err, 412)
			statusCode(t, client.Delete(WithOptions(ctx, &RequestOptions{IfMatch: version}), "one", DeleteOptions{}), 412)
			require.Equal(t, "recreated", f.stored(t, "one").Object["spec"].(map[string]any)["value"])
		})
	}
}

func TestPresenceAndOmission(t *testing.T) {
	f := setup(t, true)
	client := f.client(t)
	opts := &RequestOptions{}
	ctx := f.as("org-a")
	in := input("optional", "public")
	in.Spec.Note, in.Spec.Count = new("owned"), new(int64(1<<53+1))
	in.Spec.Enabled, in.Spec.Items = new(true), new([]string{"old"})
	created, err := client.Create(WithOptions(ctx, opts), in)
	require.NoError(t, err)
	require.Equal(t, in.Spec.Count, created.Spec.Count, "direct writes must preserve integers above 2^53")
	stored := f.stored(t, "optional")
	fields, err := json.Marshal(stored.GetManagedFields())
	require.NoError(t, err)
	require.Contains(t, string(fields), "f:note")
	require.NotContains(t, string(fields), "f:hidden")
	stored.Object["status"] = map[string]any{"counter": int64(7)}
	_, err = f.resource().UpdateStatus(ctx, stored, metav1.UpdateOptions{FieldManager: "controller"})
	require.NoError(t, err)
	_, err = client.Get(WithOptions(ctx, opts), "optional")
	require.NoError(t, err)
	mux := http.NewServeMux()
	Register(mux, "/widgets", client, OpUpdate)
	response := requestHTTP(WithLimits(mux, ServerOptions{}), ctx, "PUT", "/widgets/optional",
		`{"metadata":{"name":"optional"},"spec":{"value":"public","count":0,"enabled":false,"items":[]}}`,
		strconv.Quote(opts.ETag))
	require.Equal(t, 200, response.Code, response.Body.String())
	stored = f.stored(t, "optional")
	require.Equal(t, map[string]any{
		"value": "public", "hidden": "private-default", "count": int64(0), "enabled": false, "items": []any{},
	}, stored.Object["spec"])
	require.Equal(t, map[string]any{"counter": int64(7)}, stored.Object["status"])
	fields, err = json.Marshal(stored.GetManagedFields())
	require.NoError(t, err)
	require.NotContains(t, string(fields), "f:note")
	version, err := strconv.Unquote(response.Header().Get("ETag"))
	require.NoError(t, err)
	zeros := input("optional", "public")
	zeros.Spec.Count, zeros.Spec.Enabled, zeros.Spec.Items = new(int64(0)), new(false), new([]string{})
	opts = &RequestOptions{IfMatch: version}
	_, err = client.Update(WithOptions(ctx, opts), "optional", zeros)
	require.NoError(t, err)
	require.Equal(t, stored.Object["spec"], f.stored(t, "optional").Object["spec"],
		"direct and HTTP writes preserve the same explicit zero values")
	_, err = client.Update(WithOptions(ctx, &RequestOptions{IfMatch: opts.ETag}), "optional", input("optional", "public"))
	require.NoError(t, err)
	require.Equal(t, map[string]any{"value": "public", "hidden": "private-default"},
		f.stored(t, "optional").Object["spec"])
}

func TestScopedPagination(t *testing.T) {
	f := setup(t, true)
	client := f.client(t)
	opts := &RequestOptions{}
	ctx := f.as("org-a")
	for _, name := range []string{"a", "b", "c"} {
		_, err := client.Create(ctx, input(name, "org-a"))
		require.NoError(t, err)
	}
	for _, name := range []string{"a", "b", "c"} {
		_, err := client.Create(f.as("org-b"), input(name, "org-b"))
		require.NoError(t, err)
	}
	first, err := client.List(ctx, ListOptions{PageSize: 1})
	require.NoError(t, err)
	require.Len(t, first.Items, 1)
	require.Equal(t, "a", first.Items[0].Metadata.Name)
	other, err := client.List(f.as("org-b"), ListOptions{PageSize: 2, PageKey: first.PageKey})
	require.NoError(t, err)
	require.Len(t, other.Items, 2)
	for _, item := range other.Items {
		require.Equal(t, "org-b", item.Spec.Value, "a page key must not change the namespace selected by the host")
	}
	larger, err := client.List(ctx, ListOptions{PageSize: 2, PageKey: first.PageKey})
	require.NoError(t, err)
	require.Len(t, larger.Items, 2, "Kubernetes continuation permits changing page size")
	require.False(t, larger.HasMore)
	_, err = client.Get(WithOptions(ctx, opts), "b")
	require.NoError(t, err)
	_, err = client.Update(WithOptions(ctx, &RequestOptions{IfMatch: opts.ETag}), "b", input("b", "after"))
	require.NoError(t, err)
	_, err = client.Create(ctx, input("d", "new"))
	require.NoError(t, err)
	second, err := client.List(ctx, ListOptions{PageSize: 1, PageKey: first.PageKey})
	require.NoError(t, err)
	require.Equal(t, "b", second.Items[0].Metadata.Name)
	require.Equal(t, "org-a", second.Items[0].Spec.Value)
	third, err := client.List(ctx, ListOptions{PageSize: 1, PageKey: second.PageKey})
	require.NoError(t, err)
	require.Equal(t, "c", third.Items[0].Metadata.Name)
	require.False(t, third.HasMore)
	opts = &RequestOptions{}
	_, err = client.Get(WithOptions(ctx, opts), "a")
	require.NoError(t, err)
	_, err = client.Update(WithOptions(f.as("org-b"), &RequestOptions{IfMatch: opts.ETag}), "a", input("a", "stolen"))
	statusCode(t, err, 412)
	statusCode(t, client.Delete(
		WithOptions(f.as("org-b"), &RequestOptions{IfMatch: opts.ETag}), "a", DeleteOptions{},
	), 412)
	_, err = client.List(ctx, ListOptions{PageKey: "not-a-kubernetes-cursor"})
	statusCode(t, err, 400)

	var workers sync.WaitGroup
	failures := make(chan error, 16)
	for index := range 16 {
		workers.Go(func() {
			namespace := []string{"org-a", "org-b"}[index%2]
			got, getErr := client.Get(f.as(namespace), "a")
			if getErr == nil && got.Spec.Value != namespace {
				getErr = fmt.Errorf("response crossed namespaces")
			}
			failures <- getErr
		})
	}
	workers.Wait()
	close(failures)
	for failure := range failures {
		require.NoError(t, failure)
	}
	_, err = client.Get(f.ctx, "a")
	statusCode(t, err, 401)
	unscoped := f.as("")
	for name, op := range map[string]func() error{
		"get":    func() error { _, err := client.Get(unscoped, "a"); return err },
		"list":   func() error { _, err := client.List(unscoped, ListOptions{}); return err },
		"create": func() error { _, err := client.Create(unscoped, input("new", "x")); return err },
		"update": func() error {
			_, err := client.Update(WithOptions(unscoped, &RequestOptions{IfMatch: opts.ETag}), "a", input("a", "x"))
			return err
		},
		"delete": func() error {
			return client.Delete(WithOptions(unscoped, &RequestOptions{IfMatch: opts.ETag}), "a", DeleteOptions{})
		},
	} {
		t.Run("unscoped/"+name, func(t *testing.T) { statusCode(t, op(), 403) })
	}
}

func TestHTTPAdmissionAndPreconditions(t *testing.T) {
	f := setup(t, true)
	mux := http.NewServeMux()
	Register(mux, "/widgets", f.client(t), OpCreate, OpGet, OpList, OpUpdate, OpDelete)
	handler := WithLimits(mux, ServerOptions{})
	ctx := f.as("org-a")
	for _, tc := range []struct {
		method, path, body     string
		want                   int
		code, field, fieldCode string
	}{
		{"GET", "/widgets?labelSelector=x", "", 400, "invalidRequest", "", "invalidQuery"},
		{"GET", "/widgets?pageSize=501", "", 400, "invalidRequest", "pageSize", "invalid"},
		{"GET", "/widgets?pageSize=1&pageSize=2", "", 400, "invalidRequest", "", "invalidQuery"},
		{"GET", "/widgets?page_size=1", "", 400, "invalidRequest", "", "invalidQuery"},
		{"GET", "/widgets?page_key=legacy", "", 400, "invalidRequest", "", "invalidQuery"},
		{"POST", "/widgets", "{", 400, "invalidRequest", "", "invalidJson"},
		{
			"POST", "/widgets", `{"metadata":{"name":"empty"},"spec":{"value":""}}`,
			422, "invalidResource", "/spec/value", "invalid",
		},
		{
			"PUT", "/widgets/missing", `{"metadata":{"name":"missing"},"spec":{"value":"x"}}`,
			428, "preconditionRequired", "", "",
		},
		{"DELETE", "/widgets/missing", "", 428, "preconditionRequired", "", ""},
		{"DELETE", "/widgets/missing?propagationPolicy=orphan", "", 400, "invalidRequest", "propagationPolicy", "invalid"},
		{"DELETE", "/widgets/missing?propagation_policy=foreground", "", 400, "invalidRequest", "", "invalidQuery"},
	} {
		t.Run(tc.method+tc.path+strconv.Itoa(tc.want), func(t *testing.T) {
			response := requestHTTP(handler, ctx, tc.method, tc.path, tc.body, "")
			require.Equal(t, tc.want, response.Code, response.Body.String())
			var problem Error
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
			require.Equal(t, tc.code, problem.Code)
			if tc.fieldCode != "" {
				require.Len(t, problem.Errors, 1)
				require.Equal(t, tc.field, problem.Errors[0].Field)
				require.Equal(t, tc.fieldCode, problem.Errors[0].Code)
			} else {
				require.Empty(t, problem.Errors)
			}
			if tc.want == 422 {
				require.Equal(t, "The field does not satisfy the resource schema.", problem.Errors[0].Message)
			}
		})
	}
	created := requestHTTP(handler, ctx, "POST", "/widgets", `{"metadata":{"name":"http"},"spec":{"value":"x"}}`, "")
	require.Equal(t, 201, created.Code, created.Body.String())
	got := requestHTTP(handler, ctx, "GET", "/widgets/http", "", "")
	require.Equal(t, 200, got.Code)
	require.NotContains(t, got.Body.String(), "private-default")
	updated := requestHTTP(handler, ctx, "PUT", "/widgets/http",
		`{"metadata":{"name":"http"},"spec":{"value":"updated"}}`, got.Header().Get("ETag"))
	require.Equal(t, 200, updated.Code, updated.Body.String())
	deleted := requestHTTP(
		handler, ctx, "DELETE", "/widgets/http?propagationPolicy=foreground", "", updated.Header().Get("ETag"),
	)
	require.Equal(t, 202, deleted.Code, deleted.Body.String())
	require.Contains(t, f.stored(t, "http").GetFinalizers(), metav1.FinalizerDeleteDependents)
}

func TestCreateFailureBoundaries(t *testing.T) {
	f := setup(t, true)
	ctx := f.as("org-a")
	normal := f.client(t)
	for _, after := range []bool{false, true} {
		t.Run(fmt.Sprintf("after=%t", after), func(t *testing.T) {
			name := fmt.Sprintf("failure-%t", after)
			client := f.client(t, func(base http.RoundTripper) http.RoundTripper {
				return roundTripFunc(func(req *http.Request) (*http.Response, error) {
					if req.Method != http.MethodPatch {
						return base.RoundTrip(req)
					}
					if after {
						response, err := base.RoundTrip(req)
						if err != nil {
							return response, err
						}
						_, _ = io.Copy(io.Discard, response.Body)
						_ = response.Body.Close()
					}
					return nil, fmt.Errorf("injected transport interruption")
				})
			})
			_, err := client.Create(ctx, input(name, "original"))
			require.Error(t, err)
			stored, getErr := f.resource().Get(ctx, name, metav1.GetOptions{})
			_, err = normal.Create(ctx, input(name, "replay"))
			if after {
				require.NoError(t, getErr)
				statusCode(t, err, 409)
				require.Equal(t, stored.Object, f.stored(t, name).Object)
			} else {
				require.True(t, apierrors.IsNotFound(getErr))
				require.NoError(t, err)
			}
		})
	}
	results := make(chan error, 2)
	for range 2 {
		go func() { _, err := normal.Create(ctx, input("racing", "same")); results <- err }()
	}
	first, second := <-results, <-results
	if first != nil {
		first, second = second, first
	}
	require.NoError(t, first)
	statusCode(t, second, 409)
}
