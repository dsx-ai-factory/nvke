// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/NVIDIA/nke/controller/internal/capi"
	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
	nicov1 "gitlab-master.nvidia.com/nke/cluster-api-provider-nico/api/v1alpha1"
)

const (
	testClusterName       = "test-cluster"
	testKubeconfig        = "kubeconfig-data"
	testNicoClusterKind   = "NicoCluster"
	testNicoClusterSiteID = "site-1"
	testNicoClusterAPI    = "infrastructure.cluster.x-k8s.io/v1alpha1"
)

func TestResources_ApplyClusterScaffold(t *testing.T) {
	nicoClusterManifest := testNicoClusterManifest(t)

	type parameters struct {
		cluster         db.Cluster
		wantNicoCluster bool
	}

	tests := map[string]parameters{
		"applies namespace CAPI cluster and NicoCluster": {
			cluster:         testCluster(nicoClusterManifest),
			wantNicoCluster: true,
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			k8sClient := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()

			err := capi.ApplyClusterScaffold(ctx, k8sClient, params.cluster)
			require.NoError(t, err)

			namespace := capi.ClusterNamespace(params.cluster.Name)

			clusterObj := &clusterv1.Cluster{}
			require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Name: params.cluster.Name, Namespace: namespace}, clusterObj))
			require.NotNil(t, clusterObj)
			assert.Equal(t, namespace, clusterObj.Namespace)
			assert.Equal(t, constants.LabelValueNkeManagedBy, clusterObj.Labels[constants.LabelKeyNkeManagedBy])
			assert.Equal(t, nicov1.GroupVersion.Group, clusterObj.Spec.InfrastructureRef.APIGroup)
			assert.Equal(t, testNicoClusterKind, clusterObj.Spec.InfrastructureRef.Kind)
			assert.Equal(t, params.cluster.Name, clusterObj.Spec.InfrastructureRef.Name)

			require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Name: namespace}, &corev1.Namespace{}))
			assert.Equal(t, params.wantNicoCluster, hasNicoCluster(ctx, t, k8sClient, namespace, params.cluster.Name))
		})
	}
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

func TestResources_ApplyClusterKubeconfigSecret(t *testing.T) {
	nicoClusterManifest := testNicoClusterManifest(t)

	type parameters struct {
		cluster            db.Cluster
		existingObjects    []client.Object
		wantSecret         bool
		wantNotFoundError  bool
		wantKubeconfigData []byte
	}

	cluster := testCluster(nicoClusterManifest)
	namespace := capi.ClusterNamespace(cluster.Name)

	tests := map[string]parameters{
		"applies kubeconfig secret for existing CAPI cluster": {
			cluster: cluster,
			existingObjects: []client.Object{
				&clusterv1.Cluster{
					ObjectMeta: metav1.ObjectMeta{
						Name:      cluster.Name,
						Namespace: namespace,
					},
				},
			},
			wantSecret:         true,
			wantKubeconfigData: []byte(testKubeconfig),
		},
		"leaves existing kubeconfig secret untouched": {
			cluster: cluster,
			existingObjects: []client.Object{
				&clusterv1.Cluster{
					ObjectMeta: metav1.ObjectMeta{
						Name:      cluster.Name,
						Namespace: namespace,
					},
				},
				&corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      cluster.Name + "-kubeconfig",
						Namespace: namespace,
					},
					Data: map[string][]byte{"value": []byte("existing-kubeconfig")},
				},
			},
			wantSecret:         true,
			wantKubeconfigData: []byte("existing-kubeconfig"),
		},
		"returns not found when CAPI cluster is missing": {
			cluster:           cluster,
			wantSecret:        false,
			wantNotFoundError: true,
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			k8sClient := fake.NewClientBuilder().
				WithScheme(testScheme(t)).
				WithObjects(params.existingObjects...).
				Build()

			err := capi.ApplyClusterKubeconfigSecret(ctx, k8sClient, params.cluster, []byte(testKubeconfig))
			if params.wantNotFoundError {
				require.Error(t, err)
				assert.True(t, apierrors.IsNotFound(err))
				return
			}
			require.NoError(t, err)

			secret := &corev1.Secret{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: params.cluster.Name + "-kubeconfig", Namespace: capi.ClusterNamespace(params.cluster.Name)}, secret)
			if !params.wantSecret {
				assert.True(t, apierrors.IsNotFound(err))
				return
			}

			require.NoError(t, err)
			assert.Equal(t, params.wantKubeconfigData, secret.Data["value"])
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

func TestProvisioning_ShouldUseCAPIForNodeProvisioning(t *testing.T) {
	siteID := uuid.New()

	type parameters struct {
		enableForgeCapi bool
		cluster         db.Cluster
		want            bool
	}

	tests := map[string]parameters{
		"uses template-less forge CAPI when flag is enabled for forge site cluster": {
			enableForgeCapi: true,
			cluster:         db.Cluster{SiteID: siteID},
			want:            true,
		},
		"keeps forge direct path when flag is disabled for forge site cluster": {
			cluster: db.Cluster{SiteID: siteID},
			want:    false,
		},
		"uses CAPI for CAPI-backed cluster without site": {
			cluster: db.Cluster{SiteID: uuid.Nil},
			want:    true,
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, params.want, capi.ShouldUseCAPIForNodeProvisioning(
				params.enableForgeCapi,
				params.cluster,
			))
		})
	}
}

func hasNicoCluster(ctx context.Context, t *testing.T, k8sClient client.Client, namespace, name string) bool {
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
	assert.Equal(t, testNicoClusterSiteID, siteID)
	return true
}

func testCluster(manifest json.RawMessage) db.Cluster {
	return db.Cluster{
		ID:               uuid.New(),
		Name:             testClusterName,
		CapiInfraCluster: manifest,
	}
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
