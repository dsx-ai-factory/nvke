// SPDX-License-Identifier: Apache-2.0

package public

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nvke/management-engine/pkg/resource"
)

func TestSurfaces(t *testing.T) {
	responses := map[string]any{}
	discovery := metav1.APIResourceList{GroupVersion: "internal.nvke.nvidia.com/v1"}
	for _, name := range []string{"nvkeclusters", "clusterreleases", "machineimages"} {
		data, err := os.ReadFile("../config/crd/bases/internal.nvke.nvidia.com_" + name + ".yaml")
		require.NoError(t, err)
		var crd apiextensionsv1.CustomResourceDefinition
		require.NoError(t, yaml.Unmarshal(data, &crd))
		discovery.APIResources = append(discovery.APIResources, metav1.APIResource{
			Name: name, Kind: crd.Spec.Names.Kind, Namespaced: crd.Spec.Scope == apiextensionsv1.NamespaceScoped,
			Verbs: metav1.Verbs{"get", "list", "patch", "delete"},
		})
		for _, version := range crd.Spec.Versions {
			if version.Storage && version.Subresources != nil && version.Subresources.Status != nil {
				discovery.APIResources = append(discovery.APIResources, metav1.APIResource{Name: name + "/status"})
			}
		}
	}
	responses["/apis/internal.nvke.nvidia.com/v1"] = discovery
	for _, surface := range []struct {
		name                               string
		registrations                      []resource.Registration
		clusters, releases, writes, images bool
	}{
		{name: "tenant", registrations: Tenant(), clusters: true},
		{name: "operator", registrations: Operator(), releases: true, writes: true, images: true},
		{name: "custom read-only selection", registrations: []resource.Registration{
			ClusterRelease(resource.OpGet, resource.OpList),
		}, releases: true},
		{name: "empty selection", registrations: []resource.Registration{ClusterRelease()}},
	} {
		t.Run(surface.name, func(t *testing.T) {
			mux := http.NewServeMux()
			for _, registration := range surface.registrations {
				require.NoError(t, registration.Mount(mux, resource.ClientConfig{
					Datastore: &rest.Config{Host: "https://datastore.invalid", Transport: discoveryTransport(responses)},
					Surface:   surface.name,
					Namespace: func(context.Context) (string, error) { return "org", nil },
				}))
			}
			for _, route := range []struct {
				method, path string
				enabled      bool
			}{
				{"POST", "/v1/clusters", surface.clusters},
				{"GET", "/v1/clusters", surface.clusters},
				{"GET", "/v1/cluster-releases", surface.releases},
				{"POST", "/v1/cluster-releases", surface.writes},
				{"GET", "/v1/machine-images", surface.images},
				{"POST", "/v1/machine-images", surface.images},
			} {
				_, pattern := mux.Handler(httptest.NewRequest(route.method, route.path, nil))
				require.Equal(t, route.enabled, pattern == route.method+" "+route.path, "%s %s", route.method, route.path)
			}
		})
	}
}

// discoveryTransport serves CRD-derived discovery through the real client's HTTP boundary.
type discoveryTransport map[string]any

func (responses discoveryTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	response, ok := responses[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
	} else {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}
	return w.Result(), nil
}
