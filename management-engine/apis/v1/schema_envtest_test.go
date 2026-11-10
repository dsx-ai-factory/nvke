// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// Defaulting has to leave lifecycle present and false, because the immutability
// rule and the resolver both read it without checking for its absence.
func TestReleaseLifecycleDefaults(t *testing.T) {
	const version = "1.37.0"
	environment := &envtest.Environment{
		UseExistingCluster:          new(false),
		DownloadBinaryAssets:        true,
		DownloadBinaryAssetsVersion: version,
		BinaryAssetsDirectory:       filepath.Join("..", "..", "bin", "envtest"),
	}
	for _, resource := range releaseResources {
		crd := loadCRD(t, resource)
		environment.CRDs = append(environment.CRDs, &crd)
	}
	config, err := environment.Start()
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })
	require.NoError(t, err)
	client, err := dynamic.NewForConfig(config)
	require.NoError(t, err)

	for _, resource := range releaseResources {
		t.Run(resource, func(t *testing.T) {
			obj := &unstructured.Unstructured{Object: baseResource(t, resource)}
			require.NotContains(t, specOf(t, obj.Object), "lifecycle")
			endpoint := client.Resource(GroupVersion.WithResource(resource))
			_, err := endpoint.Create(t.Context(), obj, metav1.CreateOptions{})
			require.NoError(t, err)

			stored, err := endpoint.Get(t.Context(), obj.GetName(), metav1.GetOptions{})
			require.NoError(t, err)
			lifecycle, found := specOf(t, stored.Object)["lifecycle"].(map[string]any)
			require.True(t, found, "lifecycle default missing")
			require.Equal(t, false, lifecycle["deprecated"])
		})
	}
}
