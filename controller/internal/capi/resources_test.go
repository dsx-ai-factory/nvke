// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nicov1 "github.com/dsx-ai-factory/cluster-api-provider-nico/api/v1alpha1"

	"github.com/NVIDIA/nke/controller/internal/capi"
	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
)

const (
	testClusterName       = "test-cluster"
	testNicoClusterKind   = "NicoCluster"
	testNicoClusterSiteID = "site-1"
	testNicoClusterAPI    = "infrastructure.cluster.x-k8s.io/v1alpha1"
)

func TestResources_ApplyClusterScaffold(t *testing.T) {
	type parameters struct {
		cluster         db.Cluster
		wantNicoCluster bool
	}

	tests := map[string]parameters{
		"applies CAPI cluster and NicoCluster": {
			cluster:         testCluster(),
			wantNicoCluster: true,
		},
		"OIDC-mode scaffolds into the shared namespace too": {
			cluster:         testOIDCCluster(),
			wantNicoCluster: true,
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			k8sClient := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()

			err := capi.ApplyClusterScaffold(ctx, k8sClient, params.cluster, false)
			require.NoError(t, err)

			namespace := constants.NamespaceNKESystem

			clusterObj := &clusterv1.Cluster{}
			require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Name: params.cluster.Name, Namespace: namespace}, clusterObj))
			require.NotNil(t, clusterObj)
			assert.Equal(t, namespace, clusterObj.Namespace)
			assert.Equal(t, constants.LabelValueNkeManagedBy, clusterObj.Labels[constants.LabelKeyNkeManagedBy])
			assert.Equal(t, nicov1.GroupVersion.Group, clusterObj.Spec.InfrastructureRef.APIGroup)
			assert.Equal(t, testNicoClusterKind, clusterObj.Spec.InfrastructureRef.Kind)
			assert.Equal(t, params.cluster.Name, clusterObj.Spec.InfrastructureRef.Name)
			require.NotNil(t, clusterObj.Spec.Paused)
			assert.False(t, *clusterObj.Spec.Paused)

			// The chart owns nke-system, so the scaffold must not create it.
			err = k8sClient.Get(ctx, client.ObjectKey{Name: namespace}, &corev1.Namespace{})
			assert.True(t, apierrors.IsNotFound(err), "scaffold must not create the shared namespace")
			assert.Equal(t, params.wantNicoCluster, hasNicoCluster(ctx, t, k8sClient, namespace, params.cluster.Name, params.cluster.Nico.SiteID.String()))
		})
	}
}

func TestResources_ClusterScaffoldManifest(t *testing.T) {
	t.Run("capi provider uses provider payload", func(t *testing.T) {
		want := json.RawMessage(`{"kind":"ForgeCluster"}`)
		cluster := db.Cluster{
			ID:           uuid.New(),
			ProviderType: db.ProviderTypeCAPI,
			Capi:         &db.CAPIClusterProvider{InfraCluster: want},
		}

		got, err := capi.ClusterScaffoldManifest(cluster)
		require.NoError(t, err)
		require.JSONEq(t, string(want), string(got))
	})

	t.Run("nico provider builds NicoCluster manifest from provider payload", func(t *testing.T) {
		siteID := uuid.New()
		cluster := db.Cluster{
			ID:           uuid.New(),
			ProviderType: db.ProviderTypeNico,
			Nico:         &db.NicoClusterProvider{OrgID: "org-1", SiteID: siteID, VpcID: uuid.New()},
			Capi: &db.CAPIClusterProvider{
				InfraCluster: json.RawMessage(`{"kind":"LegacyStale"}`),
			},
		}

		got, err := capi.ClusterScaffoldManifest(cluster)
		require.NoError(t, err)

		obj, err := capi.ParseManifest(got)
		require.NoError(t, err)
		assert.Equal(t, nicov1.GroupVersion.String(), obj.GetAPIVersion())
		assert.Equal(t, testNicoClusterKind, obj.GetKind())
		gotSiteID, found, err := unstructured.NestedString(obj.Object, "spec", "siteID")
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, siteID.String(), gotSiteID)
	})

	t.Run("unsupported provider errors", func(t *testing.T) {
		_, err := capi.ClusterScaffoldManifest(db.Cluster{
			ID:           uuid.New(),
			ProviderType: db.ProviderType("unknown"),
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "unsupported provider type")
	})
}

func TestIsDockerMachineTemplate(t *testing.T) {
	tests := map[string]struct {
		raw     []byte
		want    bool
		wantErr bool
	}{
		"docker machine template": {
			raw:  []byte(`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1beta2","kind":"DockerMachineTemplate"}`),
			want: true,
		},
		"nico machine template": {
			raw:  []byte(`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1alpha1","kind":"NicoMachineTemplate"}`),
			want: false,
		},
		"invalid manifest": {
			raw:     []byte(`{`),
			wantErr: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := capi.IsDockerMachineTemplate(tc.raw)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestResources_ParseManifest(t *testing.T) {
	nicoClusterManifest := testNicoClusterManifest(t)

	type parameters struct {
		raw      []byte
		wantKind string
		wantErr  bool
	}

	tests := map[string]parameters{
		"valid manifest": {
			raw:      nicoClusterManifest,
			wantKind: testNicoClusterKind,
		},
		"invalid JSON": {
			raw:     []byte(`{`),
			wantErr: true,
		},
		"missing kind": {
			raw:     []byte(`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1alpha1"}`),
			wantErr: true,
		},
		"missing apiVersion": {
			raw:     []byte(`{"kind":"NicoCluster"}`),
			wantErr: true,
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			obj, err := capi.ParseManifest(params.raw)
			if params.wantErr {
				require.Error(t, err)
				assert.Nil(t, obj)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, testNicoClusterAPI, obj.GetAPIVersion())
			assert.Equal(t, params.wantKind, obj.GetKind())
		})
	}
}

func hasNicoCluster(ctx context.Context, t *testing.T, k8sClient client.Client, namespace, name, wantSiteID string) bool {
	t.Helper()

	nicoCluster := &unstructured.Unstructured{}
	nicoCluster.SetAPIVersion(nicov1.GroupVersion.String())
	nicoCluster.SetKind(testNicoClusterKind)
	err := k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, nicoCluster)
	if apierrors.IsNotFound(err) {
		return false
	}
	require.NoError(t, err)
	siteID, found, err := unstructured.NestedString(nicoCluster.Object, "spec", "siteID")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, wantSiteID, siteID)
	return true
}

func testCluster() db.Cluster {
	siteID := uuid.New()
	vpcID := uuid.New()
	cluster := db.Cluster{
		ID:           uuid.New(),
		Name:         testClusterName,
		ProviderType: db.ProviderTypeNico,
		Nico:         &db.NicoClusterProvider{OrgID: "org-1", SiteID: siteID, VpcID: vpcID},
	}
	return cluster
}

func testOIDCCluster() db.Cluster {
	cluster := testCluster()
	cluster.OIDCAuthEnabled = true
	return cluster
}

func testNicoClusterManifest(t *testing.T) json.RawMessage {
	t.Helper()

	manifest, err := json.Marshal(map[string]any{
		"apiVersion": nicov1.GroupVersion.String(),
		"kind":       testNicoClusterKind,
		"spec": map[string]any{
			"siteID": testNicoClusterSiteID,
		},
	})
	require.NoError(t, err)
	return manifest
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, clusterv1.AddToScheme(scheme))
	scheme.AddKnownTypeWithName(nicov1.GroupVersion.WithKind(testNicoClusterKind), &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(nicov1.GroupVersion.WithKind(testNicoClusterKind+"List"), &unstructured.UnstructuredList{})
	return scheme
}
