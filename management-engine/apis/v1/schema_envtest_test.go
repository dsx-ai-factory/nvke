// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// wellKnownCRDResourceNames lists the resource names whose CRDs the shared test API server installs.
var wellKnownCRDResourceNames = []string{
	"nvkeclusters",
	"controlplaneclasses",
	"clusterreleasechannels",
	"machineimages",
	"clusterreleases",
	"nodepoolreleases",
}

// Defaulting has to leave lifecycle present and false, because the immutability
// rule and the resolver both read it without checking for its absence.
func TestReleaseLifecycleDefaults(t *testing.T) {
	tests := []struct {
		name                   string
		inputGVR               schema.GroupVersionResource
		inputResourcePathFn    func(*testing.T) string
		expectedResourcePathFn func(*testing.T) string
	}{
		{
			name:     "ClusterReleaseWithLifecycleOmitted",
			inputGVR: GroupVersion.WithResource("clusterreleases"),
			inputResourcePathFn: func(t *testing.T) string {
				return wellKnownInputResourcePath(t)
			},
			expectedResourcePathFn: func(t *testing.T) string {
				return wellKnownExpectedResourcePath(t)
			},
		},
		{
			name:     "NodePoolReleaseWithLifecycleOmitted",
			inputGVR: GroupVersion.WithResource("nodepoolreleases"),
			inputResourcePathFn: func(t *testing.T) string {
				return wellKnownInputResourcePath(t)
			},
			expectedResourcePathFn: func(t *testing.T) string {
				return wellKnownExpectedResourcePath(t)
			},
		},
	}

	testClient := startTestAPIServerWithCRDsAndCleanup(t)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkResourceCreation(t, testClient, tt.inputGVR, tt.inputResourcePathFn, tt.expectedResourcePathFn)
		})
	}
}

func TestResourceCreationWithRequiredFields(t *testing.T) {
	tests := []struct {
		name                   string
		inputGVR               schema.GroupVersionResource
		inputResourcePathFn    func(*testing.T) string
		expectedResourcePathFn func(*testing.T) string
	}{
		{
			name:     "NVKEClusterWithRequiredFields",
			inputGVR: GroupVersion.WithResource("nvkeclusters"),
			inputResourcePathFn: func(t *testing.T) string {
				return wellKnownInputResourcePath(t)
			},
			expectedResourcePathFn: func(t *testing.T) string {
				return wellKnownExpectedResourcePath(t)
			},
		},
		{
			name:     "MachineImageWithRequiredFields",
			inputGVR: GroupVersion.WithResource("machineimages"),
			inputResourcePathFn: func(t *testing.T) string {
				return wellKnownInputResourcePath(t)
			},
			expectedResourcePathFn: func(t *testing.T) string {
				return wellKnownExpectedResourcePath(t)
			},
		},
		{
			name:     "ClusterReleaseWithRequiredFields",
			inputGVR: GroupVersion.WithResource("clusterreleases"),
			inputResourcePathFn: func(t *testing.T) string {
				return wellKnownInputResourcePath(t)
			},
			expectedResourcePathFn: func(t *testing.T) string {
				return wellKnownExpectedResourcePath(t)
			},
		},
		{
			name:     "NodePoolReleaseWithRequiredFields",
			inputGVR: GroupVersion.WithResource("nodepoolreleases"),
			inputResourcePathFn: func(t *testing.T) string {
				return wellKnownInputResourcePath(t)
			},
			expectedResourcePathFn: func(t *testing.T) string {
				return wellKnownExpectedResourcePath(t)
			},
		},
		{
			name:     "ControlPlaneClassWithRequiredFields",
			inputGVR: GroupVersion.WithResource("controlplaneclasses"),
			inputResourcePathFn: func(t *testing.T) string {
				return wellKnownInputResourcePath(t)
			},
			expectedResourcePathFn: func(t *testing.T) string {
				return wellKnownExpectedResourcePath(t)
			},
		},
	}

	testClient := startTestAPIServerWithCRDsAndCleanup(t)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkResourceCreation(t, testClient, tt.inputGVR, tt.inputResourcePathFn, tt.expectedResourcePathFn)
		})
	}
}

func startTestAPIServerWithCRDsAndCleanup(t *testing.T) dynamic.Interface {
	t.Helper()

	environment := &envtest.Environment{
		UseExistingCluster: new(false),
	}
	for _, resource := range wellKnownCRDResourceNames {
		crd := loadCRD(t, resource)
		environment.CRDs = append(environment.CRDs, &crd)
	}
	config, err := environment.Start()
	t.Cleanup(func() { require.NoError(t, environment.Stop(), "stop envtest") })
	require.NoError(t, err, "start envtest; if running from an IDE, set KUBEBUILDER_ASSETS")
	testClient, err := dynamic.NewForConfig(config)
	require.NoError(t, err, "create dynamic client")

	return testClient
}

func checkResourceCreation(t *testing.T, testClient dynamic.Interface, gvr schema.GroupVersionResource, inputResourcePathFn func(*testing.T) string, expectedResourcePathFn func(*testing.T) string) {
	t.Helper()

	inputResource := readResource(t, inputResourcePathFn(t))
	expectedResource := readResource(t, expectedResourcePathFn(t))
	var resourceClient dynamic.ResourceInterface = testClient.Resource(gvr)
	if namespace := inputResource.GetNamespace(); namespace != "" {
		ensureTestNamespace(t, testClient, namespace)
		resourceClient = testClient.Resource(gvr).Namespace(namespace)
	}
	_, err := resourceClient.Create(t.Context(), inputResource, metav1.CreateOptions{})
	require.NoError(t, err, "create resource")

	actualResource, err := resourceClient.Get(t.Context(), inputResource.GetName(), metav1.GetOptions{})
	require.NoError(t, err, "get persisted resource")

	// Ignore only metadata generated by the API server.
	for _, field := range []string{"creationTimestamp", "generation", "managedFields", "resourceVersion", "uid"} {
		unstructured.RemoveNestedField(actualResource.Object, "metadata", field)
	}
	require.Equal(t, expectedResource.Object, actualResource.Object, "persisted resource should match expected YAML")
}

func ensureTestNamespace(t *testing.T, testClient dynamic.Interface, namespace string) {
	t.Helper()

	resource := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata":   map[string]any{"name": namespace},
	}}
	_, err := testClient.Resource(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}).Create(t.Context(), resource, metav1.CreateOptions{})
	if !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err, "create namespace")
	}
}

func wellKnownInputResourcePath(t *testing.T) string {
	t.Helper()
	return filepath.Join("testdata", filepath.FromSlash(t.Name()), "input.yaml")
}

func wellKnownExpectedResourcePath(t *testing.T) string {
	t.Helper()
	return filepath.Join("testdata", filepath.FromSlash(t.Name()), "expected.yaml")
}

func readResource(t *testing.T, path string) *unstructured.Unstructured {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err, "read resource YAML %s", path)
	resource := &unstructured.Unstructured{}
	require.NoError(t, k8syaml.UnmarshalStrict(data, &resource.Object), "decode resource YAML %s", path)
	return resource
}
