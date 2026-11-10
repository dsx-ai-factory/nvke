// SPDX-License-Identifier: Apache-2.0

package resource

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
)

func TestReadRegistrationTransport(t *testing.T) {
	info := ResourceInfo{
		GVK: v1.SchemeGroupVersion.WithKind("ControlPlaneClass"), Resource: "controlplaneclasses",
		Scope: ClusterScoped, Verbs: []Operation{OpGet, OpList},
	}
	class := v1.ControlPlaneClass{
		Name: "standard", ResourceVersion: "42",
		Spec: v1.ControlPlaneClassSpec{ControlPlane: v1.ControlPlaneClassConfig{
			Replicas: new(int32(3)), InstanceTypePreferences: []string{"private-instance"},
		}},
	}
	class.SetGroupVersionKind(info.GVK)
	var calls int
	cfg := fixtureConfig(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/apis/"+v1.SchemeGroupVersion.String() {
			return jsonResponse(r, http.StatusOK, &metav1.APIResourceList{
				GroupVersion: v1.SchemeGroupVersion.String(), APIResources: []metav1.APIResource{{
					Name: info.Resource, Kind: info.GVK.Kind, Verbs: metav1.Verbs{"get", "list"},
				}},
			})
		}
		calls++
		require.Equal(t, http.MethodGet, r.Method)
		deadline, bounded := r.Context().Deadline()
		require.True(t, bounded)
		require.Positive(t, time.Until(deadline))
		require.LessOrEqual(t, time.Until(deadline), time.Second)
		path := "/apis/" + v1.SchemeGroupVersion.String() + "/controlplaneclasses"
		if r.URL.Path == path+"/standard" {
			return jsonResponse(r, http.StatusOK, class)
		}
		require.Equal(t, path, r.URL.Path)
		return jsonResponse(r, http.StatusOK, &v1.ControlPlaneClassList{
			APIVersion: v1.SchemeGroupVersion.String(), Kind: "ControlPlaneClassList",
			Items: []v1.ControlPlaneClass{class},
		})
	}))
	mux := http.NewServeMux()
	// Neither the source nor its catalog envelope implements write conversion or validation.
	require.NoError(t, NewReadRegistration[v1.ControlPlaneClass]("/classes", info, "metadata.name").Mount(mux, cfg))
	require.NoError(t, NewReadRegistration[v1.ControlPlaneClass]("/unfiltered-classes", info).Mount(mux, cfg))
	require.NoError(t, NewRegistration[widget, internalWidget]("/widgets", widgetInfo(OpList)).Mount(mux, cfg))
	handler := WithLimits(mux, ServerOptions{MaxBodyBytes: 32, RequestTimeout: time.Second})
	for _, tc := range []struct {
		name, method, path, body string
		status                   int
	}{
		{"unknown query", "GET", "/classes?unknown=x", "", 400},
		{"repeated query", "GET", "/classes?pageSize=1&pageSize=2", "", 400},
		{"empty page size", "GET", "/classes?pageSize=", "", 400},
		{"oversized page", "GET", "/classes?pageSize=501", "", 400},
		{"empty selector", "GET", "/classes?fieldSelector=", "", 400},
		{"repeated selector", "GET", "/classes?fieldSelector=metadata.name=a&fieldSelector=metadata.name=b", "", 400},
		{"unsupported field", "GET", "/classes?fieldSelector=spec.controlPlane.instanceTypePreferences=x", "", 400},
		{"unsupported operator", "GET", "/classes?fieldSelector=metadata.name!=standard", "", 400},
		{"repeated field", "GET", "/classes?fieldSelector=metadata.name=standard,metadata.name=other", "", 400},
		{"selector without opt-in", "GET", "/unfiltered-classes?fieldSelector=metadata.name=standard", "", 400},
		{"empty CRUD selector", "GET", "/widgets?fieldSelector=", "", 400},
		{"named query", "GET", "/classes/standard?fieldSelector=metadata.name=standard", "", 400},
		{"unsupported version", "GET", "/classes?version=1.34", "", 400},
		{"CRUD version", "GET", "/widgets?version=1.34", "", 400},
		{"invalid name", "GET", "/classes/Bad_Name", "", 400},
		{"named body", "GET", "/classes/standard", " ", 400},
		{"list body", "GET", "/classes", strings.Repeat("x", 64), 400},
		{"create", "POST", "/classes", "", 405},
		{"update", "PUT", "/classes/standard", "", 405},
		{"delete", "DELETE", "/classes/standard", "", 405},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := requestHTTP(handler, t.Context(), tc.method, tc.path, tc.body, "")
			require.Equal(t, tc.status, response.Code, response.Body.String())
			require.Equal(t, "no-store", response.Header().Get("Cache-Control"))
			require.Equal(t, "application/problem+json", response.Header().Get("Content-Type"))
			var problem Error
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
			if tc.status == http.StatusMethodNotAllowed {
				require.Equal(t, "methodNotAllowed", problem.Code)
				require.Equal(t, "GET", response.Header().Get("Allow"))
			} else {
				require.Equal(t, "invalidRequest", problem.Code)
			}
			require.Zero(t, calls, "invalid or unsupported requests must not read the datastore")
		})
	}
	for _, path := range []string{
		"/classes/standard",
		"/classes?fieldSelector=metadata.name=standard",
	} {
		response := requestHTTP(handler, t.Context(), http.MethodGet, path, "", "")
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		require.Equal(t, "no-store", response.Header().Get("Cache-Control"))
		require.Empty(t, response.Header().Get("ETag"), "a projected view must not expose the source's resourceVersion")
		if strings.Contains(path, "fieldSelector") {
			var page List[v1.CatalogEntry[v1.CatalogControlPlaneClassSpec]]
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &page))
			require.Len(t, page.Items, 1)
			require.Equal(t, "standard", page.Items[0].Metadata.Name)
		}
	}
	require.Equal(t, 2, calls)
}

func TestCatalogVersionFilterTransport(t *testing.T) {
	channel := v1.ClusterReleaseChannel{Name: "b", Spec: v1.ClusterReleaseChannelSpec{
		Releases: map[string]v1.ClusterReleaseReferences{
			"1.34.4-custom.1": {{Name: "release"}},
		},
	}}
	other := channel
	other.Name = "c"
	excluded := v1.ClusterReleaseChannel{Name: "a", Spec: v1.ClusterReleaseChannelSpec{
		Releases: map[string]v1.ClusterReleaseReferences{"1.35": {{Name: "other-release"}}},
	}}
	source := &sourceStub[v1.ClusterReleaseChannel]{
		items: []v1.ClusterReleaseChannel{other, excluded, channel},
		named: channel,
		err:   errors.New("unexpected datastore read"),
	}
	reader := &projectedReader[v1.ClusterReleaseChannel, v1.CatalogEntry[v1.ChannelCatalogSpec]]{
		source: source,
		project: dependentProjector[v1.ClusterReleaseChannel](func(context.Context) (map[string]v1.ClusterRelease, error) {
			return map[string]v1.ClusterRelease{
				"release": {ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{v1.MachineInitKubernetesVersionAnnotation: "1.34.4"},
				}},
				"other-release": {ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{v1.MachineInitKubernetesVersionAnnotation: "1.35.1"},
				}},
			}, nil
		}),
	}
	mux := http.NewServeMux()
	Register(mux, "/channels", reader, nil, nil, OpGet, OpList)
	for _, path := range []string{
		"/channels?version=", "/channels?version=bad", "/channels/b?version=1.34&version=1.35",
		"/channels/b?version=1.34.99999999999999999999",
	} {
		response := requestHTTP(mux, t.Context(), http.MethodGet, path, "", "")
		require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	}
	source.err = nil
	response := requestHTTP(mux, t.Context(), http.MethodGet, "/channels/b?version=1.34", "", "")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var view v1.CatalogEntry[v1.ChannelCatalogSpec]
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &view))
	require.Len(t, view.Spec.Versions, 1)
	require.Equal(t, "1.34.4-custom.1", view.Spec.DefaultVersion)

	response = requestHTTP(mux, t.Context(), http.MethodGet, "/channels?version=1.34&pageSize=1", "", "")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var page List[v1.CatalogEntry[v1.ChannelCatalogSpec]]
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &page))
	require.Len(t, page.Items, 1)
	require.Equal(t, "b", page.Items[0].GetName(), "excluded channels must not consume page slots")
	require.True(t, page.HasMore)
	for _, query := range []string{"version=1.34&pageSize=1&", "version=1.34.4&pageSize=1&", "pageSize=1&"} {
		response = requestHTTP(mux, t.Context(), http.MethodGet, "/channels?"+query+"pageKey="+page.PageKey, "", "")
		if strings.HasPrefix(query, "version=1.34&") {
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			var next List[v1.CatalogEntry[v1.ChannelCatalogSpec]]
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &next))
			require.Equal(t, "c", next.Items[0].GetName())
		} else {
			require.Equal(t, http.StatusBadRequest, response.Code, "identical results cannot authorize switching filters")
		}
	}
	for _, path := range []string{"/channels/b?version=1.36", "/channels?version=1.36"} {
		response = requestHTTP(mux, t.Context(), http.MethodGet, path, "", "")
		if strings.Contains(path, "/b?") {
			require.Equal(t, http.StatusNotFound, response.Code)
		} else {
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &page))
			require.Empty(t, page.Items)
		}
	}
}

func TestReadRegistrationCapabilities(t *testing.T) {
	for _, op := range []Operation{OpCreate, OpUpdate, OpDelete, "unknown"} {
		t.Run(string(op), func(t *testing.T) {
			mux := http.NewServeMux()
			registration := NewReadRegistration[v1.ControlPlaneClass]("/classes", widgetInfo(OpGet, op))
			require.ErrorContains(t, registration.Mount(mux, ClientConfig{}), "read registrations support only get and list")
			_, pattern := mux.Handler(httptest.NewRequest(http.MethodGet, "/classes/standard", nil))
			require.Empty(t, pattern, "failed mounts must not install routes")
		})
	}
	mux := http.NewServeMux()
	Register(mux, "/classes", directReader(nil), nil, nil, OpGet, OpList, OpCreate, OpUpdate, OpDelete)
	for _, tc := range []struct{ method, path string }{
		{"POST", "/classes"}, {"PUT", "/classes/standard"}, {"DELETE", "/classes/standard"},
	} {
		response := requestHTTP(mux, t.Context(), tc.method, tc.path, "", "")
		require.Equal(t, http.StatusMethodNotAllowed, response.Code)
		require.Equal(t, "GET", response.Header().Get("Allow"))
		require.Equal(t, "application/problem+json", response.Header().Get("Content-Type"))
	}
}

func TestDependentReadRegistrationConstruction(t *testing.T) {
	type imageLoader = func(context.Context) (map[string]v1.MachineImage, error)
	failure := errors.New("dependency setup failed")
	for _, tc := range []struct {
		name    string
		prepare func(ClientConfig) (imageLoader, error)
		cause   error
	}{
		{name: "missing preparation"},
		{name: "missing loader", prepare: func(ClientConfig) (imageLoader, error) { return nil, nil }},
		{name: "preparation error", prepare: func(ClientConfig) (imageLoader, error) { return nil, failure }, cause: failure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			registration := NewDependentReadRegistration[v1.NodePoolRelease, map[string]v1.MachineImage,
				v1.CatalogEntry[v1.NodeProfileCatalogSpec]]("/profiles", widgetInfo(OpGet, OpList), tc.prepare)
			err := registration.Mount(mux, ClientConfig{})
			require.Error(t, err)
			if tc.cause != nil {
				require.ErrorIs(t, err, tc.cause)
			} else {
				require.ErrorContains(t, err, "a dependency loader is required")
			}
			_, pattern := mux.Handler(httptest.NewRequest(http.MethodGet, "/profiles", nil))
			require.Empty(t, pattern, "failed mounts must not install routes")
		})
	}
}

func TestDependentReadRegistration(t *testing.T) {
	nodes := ResourceInfo{
		GVK: v1.SchemeGroupVersion.WithKind("NodePoolRelease"), Resource: "nodepoolreleases",
		Scope: ClusterScoped, Verbs: []Operation{OpGet},
	}
	images := ResourceInfo{
		GVK: v1.SchemeGroupVersion.WithKind("MachineImage"), Resource: "machineimages", Scope: ClusterScoped,
	}
	source := v1.NodePoolRelease{
		Name: "hidden", ResourceVersion: "42",
		Spec: v1.NodePoolReleaseSpec{Profile: "ubuntu-gpu", Tag: "2026.11",
			MachineImageRef: v1.MachineImageReference{Name: "image"}, Lifecycle: v1.ReleaseLifecycle{Hidden: true}},
	}
	image := v1.MachineImage{Name: "image", Spec: v1.MachineImageSpec{
		OperatingSystem: v1.OperatingSystem{Family: "ubuntu", Version: "24.04"}, Architecture: "amd64", Variant: "private",
	}}
	source.SetGroupVersionKind(nodes.GVK)
	image.SetGroupVersionKind(images.GVK)
	var prepares, gets, lists int
	cfg := fixtureConfig(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		require.Equal(t, http.MethodGet, r.Method)
		api := "/apis/" + v1.SchemeGroupVersion.String()
		switch r.URL.Path {
		case api:
			return jsonResponse(r, http.StatusOK, &metav1.APIResourceList{
				GroupVersion: v1.SchemeGroupVersion.String(), APIResources: []metav1.APIResource{
					{Name: nodes.Resource, Kind: nodes.GVK.Kind, Verbs: metav1.Verbs{"get"}},
					{Name: images.Resource, Kind: images.GVK.Kind, Verbs: metav1.Verbs{"list"}},
				},
			})
		case api + "/nodepoolreleases/hidden":
			gets++
			return jsonResponse(r, http.StatusOK, source)
		default:
			require.Equal(t, api+"/machineimages", r.URL.Path)
			lists++
			return jsonResponse(r, http.StatusOK, &v1.MachineImageList{
				APIVersion: v1.SchemeGroupVersion.String(), Kind: "MachineImageList",
				Items: []v1.MachineImage{image},
			})
		}
	}))
	type imageLoader = func(context.Context) (map[string]v1.MachineImage, error)
	prepare := func(cfg ClientConfig) (imageLoader, error) {
		prepares++
		list, err := NewSourceLister[v1.MachineImage](cfg, images)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context) (map[string]v1.MachineImage, error) {
			values, err := list(ctx)
			if err != nil {
				return nil, err
			}
			byName := make(map[string]v1.MachineImage, len(values))
			for _, value := range values {
				byName[value.Name] = value
			}
			return byName, nil
		}, nil
	}
	registration := NewDependentReadRegistration[v1.NodePoolRelease]("/profiles", nodes, prepare)
	mux := http.NewServeMux()
	require.NoError(t, registration.Mount(mux, cfg))
	require.Equal(t, 1, prepares)
	require.Zero(t, gets+lists, "mount checks discovery without reading resources")
	response := requestHTTP(WithLimits(mux, ServerOptions{}), t.Context(), http.MethodGet, "/profiles/hidden", "", "")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Empty(t, response.Header().Get("ETag"))
	require.Equal(t, 1, gets)
	require.Equal(t, 1, lists)
}
