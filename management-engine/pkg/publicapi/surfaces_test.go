// SPDX-License-Identifier: Apache-2.0

// These tests verify the routes exposed by each public API.

package publicapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nvke/auth/authn"
	"github.com/dsx-ai-factory/nvke/auth/authz"
	"github.com/dsx-ai-factory/nvke/auth/authz/localbundle"
	"github.com/dsx-ai-factory/nvke/management-engine/pkg/resource"
)

// TestDefaultRolePolicies checks default permissions and issuer mappings without an HTTP server.
func TestDefaultRolePolicies(t *testing.T) {
	tenantReads := strings.Fields(`getCluster listClusters getControlPlaneClass listControlPlaneClasses
		getClusterReleaseChannel listClusterReleaseChannels getNodeProfile listNodeProfiles`)
	tenantWrites := strings.Fields(`createCluster updateCluster deleteCluster`)
	operatorReads := strings.Fields(`getClusterRelease listClusterReleases getMachineImage listMachineImages
		getControlPlaneClass listControlPlaneClasses getClusterReleaseChannel listClusterReleaseChannels
		getNodeProfile listNodeProfiles`)
	operatorWrites := strings.Fields(`createClusterRelease updateClusterRelease deleteClusterRelease
		createMachineImage updateMachineImage deleteMachineImage createControlPlaneClass updateControlPlaneClass
		deleteControlPlaneClass createClusterReleaseChannel updateClusterReleaseChannel deleteClusterReleaseChannel
		createNodeProfile updateNodeProfile deleteNodeProfile`)
	bundles := map[string]*localbundle.LocalBundle{}
	for _, surface := range []struct {
		name          string
		reads, writes []string
	}{
		{"tenant", tenantReads, tenantWrites},
		{"operator", operatorReads, operatorWrites},
	} {
		bundle, err := localbundle.New(localbundle.Options{
			Dir: filepath.Join("../../config/auth", surface.name), Issuers: []string{"issuer"},
			Actions: append(surface.reads, surface.writes...), ResourceTypes: []string{"Collection", "Cluster"},
		})
		require.NoError(t, err)
		bundles[surface.name] = bundle
	}
	for _, tc := range []struct {
		name, surface, group string
		actions              []string
		want                 authz.Outcome
	}{
		{"viewer reads", "tenant", "viewers", tenantReads, authz.OutcomeAllow},
		{"viewer cannot write", "tenant", "viewers", tenantWrites, authz.OutcomeDeny},
		{"editor inherits reads", "tenant", "editors", tenantReads, authz.OutcomeAllow},
		{"editor writes", "tenant", "editors", tenantWrites, authz.OutcomeAllow},
		{"admin inherits reads", "tenant", "admins", tenantReads, authz.OutcomeAllow},
		{"admin inherits writes", "tenant", "admins", tenantWrites, authz.OutcomeAllow},
		{"unmapped tenant", "tenant", "unmapped", []string{"getCluster"}, authz.OutcomeDeny},
		{"catalog reader reads", "operator", "catalog-readers", operatorReads, authz.OutcomeAllow},
		{"catalog reader cannot write", "operator", "catalog-readers", operatorWrites, authz.OutcomeDeny},
		{"catalog admin inherits reads", "operator", "catalog-admins", operatorReads, authz.OutcomeAllow},
		{"catalog admin writes", "operator", "catalog-admins", operatorWrites, authz.OutcomeAllow},
		{"unmapped operator", "operator", "unmapped", []string{"getMachineImage"}, authz.OutcomeDeny},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caller := authz.Caller{Tenant: "org-a"}
			if tc.surface == "operator" {
				caller = authz.Caller{Tenant: "operator-org", Operator: true}
			}
			for _, action := range tc.actions {
				decision, err := bundles[tc.surface].Authorize(t.Context(), authz.Query{
					Principal: authn.Principal{IssuerName: "issuer", Subject: "alice", Claims: map[string]any{
						"organization": caller.Tenant, "groups": []any{tc.group},
					}},
					Action:   action,
					Resource: authz.Resource{Type: "Collection", ID: "catalog", Scope: authz.ScopeGlobal},
				})
				require.NoError(t, err)
				require.Equal(t, tc.want, decision.Outcome, action)
				require.Equal(t, caller, decision.Caller, action)
			}
		})
	}
	for _, tc := range []struct {
		name, issuer, group string
		tenant              any
		owner               string
		want                authz.Outcome
	}{
		{"own cluster", "issuer", "viewers", "org-a", "org-a", authz.OutcomeAllow},
		{"other tenant", "issuer", "admins", "org-a", "org-b", authz.OutcomeNotFound},
		{"missing tenant", "issuer", "viewers", nil, "org-a", authz.OutcomeNoTenant},
		{"multiple tenants", "issuer", "viewers", []any{"org-a", "org-b"}, "org-a", authz.OutcomeNoTenant},
		{"malformed tenant", "issuer", "viewers", "bad\nvalue", "org-a", authz.OutcomeNoTenant},
		{"operator impostor", "issuer", "admins", "operator-org", "org-a", authz.OutcomeDeny},
		{"unconfigured issuer", "unknown", "admins", "org-a", "org-a", authz.OutcomeNoTenant},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decision, err := bundles["tenant"].Authorize(t.Context(), authz.Query{
				Principal: authn.Principal{IssuerName: tc.issuer, Subject: "alice", Claims: map[string]any{
					"organization": tc.tenant, "groups": []any{tc.group},
				}},
				Action: "getCluster",
				Resource: authz.Resource{
					Type: "Cluster", ID: "example", Scope: authz.ScopeObject, Tenant: tc.owner,
				},
			})
			require.NoError(t, err)
			require.Equal(t, tc.want, decision.Outcome)
		})
	}
}

func TestSurfaces(t *testing.T) {
	responses := map[string]any{}
	discovery := metav1.APIResourceList{GroupVersion: "service.nvke.nvidia.com/v1"}
	for _, name := range []string{
		"nvkeclusters", "clusterreleases", "machineimages",
		"controlplaneclasses", "clusterreleasechannels", "nodeprofiles",
	} {
		data, err := os.ReadFile("../../config/crd/bases/service.nvke.nvidia.com_" + name + ".yaml")
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
	responses["/apis/service.nvke.nvidia.com/v1"] = discovery
	for _, surface := range []struct {
		Surface
		clusters, releases, writes, images bool
		catalog                            bool
	}{
		{Surface: Tenant(), clusters: true, catalog: true},
		{Surface: Operator(), releases: true, writes: true, images: true},
		{Name: "custom read-only selection", Registrations: []resource.Registration{
			ClusterRelease(resource.OpGet, resource.OpList),
		}, releases: true},
		{Name: "empty selection", Registrations: []resource.Registration{ClusterRelease()}},
	} {
		t.Run(surface.Name, func(t *testing.T) {
			mux := http.NewServeMux()
			for _, registration := range surface.Registrations {
				require.NoError(t, registration.Mount(mux, resource.ClientConfig{
					Datastore: &rest.Config{Host: "https://datastore.invalid", Transport: discoveryTransport(responses)},
					Surface:   surface.Name,
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
				{"GET", "/v1/control-plane-classes", surface.catalog || surface.Name == "operator"},
				{"POST", "/v1/control-plane-classes", surface.Name == "operator"},
				{"GET", "/v1/control-plane-classes/{name}", surface.catalog || surface.Name == "operator"},
				{"PUT", "/v1/control-plane-classes/{name}", surface.Name == "operator"},
				{"DELETE", "/v1/control-plane-classes/{name}", surface.Name == "operator"},
				{"GET", "/v1/cluster-release-channels", surface.catalog || surface.Name == "operator"},
				{"POST", "/v1/cluster-release-channels", surface.Name == "operator"},
				{"GET", "/v1/cluster-release-channels/{name}", surface.catalog || surface.Name == "operator"},
				{"PUT", "/v1/cluster-release-channels/{name}", surface.Name == "operator"},
				{"DELETE", "/v1/cluster-release-channels/{name}", surface.Name == "operator"},
				{"GET", "/v1/node-profiles", surface.catalog || surface.Name == "operator"},
				{"POST", "/v1/node-profiles", surface.Name == "operator"},
				{"GET", "/v1/node-profiles/{name}", surface.catalog || surface.Name == "operator"},
				{"PUT", "/v1/node-profiles/{name}", surface.Name == "operator"},
				{"DELETE", "/v1/node-profiles/{name}", surface.Name == "operator"},
				{"GET", "/v1/node-pool-releases", false},
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
