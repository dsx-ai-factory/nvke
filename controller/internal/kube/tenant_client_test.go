// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package kube

import (
	"bytes"
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	capikubeconfig "sigs.k8s.io/cluster-api/util/kubeconfig"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/NVIDIA/nke/controller/internal/capi"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func TestCAPIKubeconfigTenantClientProviderReturnsNotFoundForMissingSecret(t *testing.T) {
	scheme := newTenantClientProviderTestScheme(t)
	managementClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	provider := NewCAPIKubeconfigTenantClientProvider(managementClient)

	_, err := provider.ClientForCluster(context.Background(), newCAPIClusterForTenantClientProviderTest())
	require.Error(t, err)
	require.True(t, apierrors.IsNotFound(err))
}

func TestCAPIKubeconfigTenantClientProviderReturnsParseErrorForInvalidSecret(t *testing.T) {
	cluster := newCAPIClusterForTenantClientProviderTest()
	secret := capikubeconfig.GenerateSecret(cluster, []byte("not kubeconfig"))
	scheme := newTenantClientProviderTestScheme(t)
	managementClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, secret).Build()
	provider := NewCAPIKubeconfigTenantClientProvider(managementClient)

	_, err := provider.ClientForCluster(context.Background(), cluster)
	require.Error(t, err)
	require.False(t, apierrors.IsNotFound(err))
}

func TestCAPIKubeconfigTenantClientProviderAppliesAdditionalSchemes(t *testing.T) {
	cluster := newCAPIClusterForTenantClientProviderTest()
	kubeconfigBytes := newTenantClientProviderTestKubeconfig(t)
	secret := capikubeconfig.GenerateSecret(cluster, kubeconfigBytes)
	scheme := newTenantClientProviderTestScheme(t)
	managementClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, secret).Build()
	addToSchemeCalled := false
	provider := NewCAPIKubeconfigTenantClientProvider(managementClient)

	tenantClient, err := provider.ClientForCluster(context.Background(), cluster, func(*runtime.Scheme) error {
		addToSchemeCalled = true
		return nil
	})
	require.NoError(t, err)
	require.NotNil(t, tenantClient)
	require.True(t, addToSchemeCalled)
}

func TestNewRESTConfigFromKubeconfigPreservesExecProvider(t *testing.T) {
	restConfig, err := NewRESTConfigFromKubeconfig(newTenantClientProviderExecKubeconfig())
	require.NoError(t, err)
	require.Equal(t, "/opt/spiffe/bin/k8s-spiffe-workload-jwt-exec-auth", restConfig.ExecProvider.Command)
	require.Equal(t, []clientcmdapi.ExecEnvVar{
		{Name: "SPIFFE_ENDPOINT_SOCKET", Value: "unix:///spiffe-workload-api/spire-agent.sock"},
		{Name: "SPIFFE_JWT_AUDIENCE", Value: "tenant-a"},
	}, restConfig.ExecProvider.Env)
}

func TestDBClusterTenantClientProviderReturnsNotReadyForMissingPublishedSecret(t *testing.T) {
	managementClient := fake.NewClientBuilder().WithScheme(newTenantClientProviderTestScheme(t)).Build()
	provider := NewDBClusterTenantClientProvider(nil, managementClient, nil, true)

	_, err := provider.ClientForDBCluster(context.Background(), newSiteDBClusterForTenantClientProviderTest())
	require.Error(t, err)
	require.ErrorIs(t, err, ErrTenantClientNotReady)
}

func TestDBClusterTenantClientProviderReturnsNotReadyForEmptyPublishedSecret(t *testing.T) {
	cluster := newSiteDBClusterForTenantClientProviderTest()
	managementClient := fake.NewClientBuilder().
		WithScheme(newTenantClientProviderTestScheme(t)).
		WithObjects(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: tenantAccessKubeconfigNamespace,
				Name:      cluster.Name + "-kubeconfig",
			},
		}).
		Build()
	provider := NewDBClusterTenantClientProvider(nil, managementClient, nil, true)

	_, err := provider.ClientForDBCluster(context.Background(), cluster)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrTenantClientNotReady)
}

func TestDBClusterTenantClientProviderReturnsNotReadyForEmptyCAPISecret(t *testing.T) {
	cluster := db.Cluster{
		ID:               uuid.New(),
		Name:             "test-cluster",
		CapiInfraCluster: []byte(`{"kind":"test"}`),
	}
	cluster.SetCAPIProvider(db.CAPIClusterProvider{InfraCluster: cluster.CapiInfraCluster})
	capiCluster := newCAPIClusterForTenantClientProviderTest()
	capiCluster.Namespace = capi.ClusterNamespace(cluster.Name)
	kubeconfigSecret := capikubeconfig.GenerateSecret(capiCluster, nil)
	managementClient := fake.NewClientBuilder().
		WithScheme(newTenantClientProviderTestScheme(t)).
		WithObjects(capiCluster, kubeconfigSecret).
		Build()
	provider := NewDBClusterTenantClientProvider(nil, managementClient, nil, false)

	_, err := provider.ClientForDBCluster(context.Background(), cluster)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrTenantClientNotReady)
}

func TestDBClusterTenantClientProviderErrorsForUnsupportedProviderType(t *testing.T) {
	cluster := db.Cluster{
		ID:           uuid.New(),
		Name:         "test-cluster",
		ProviderType: db.ProviderType("unknown"),
	}
	provider := NewDBClusterTenantClientProvider(nil, nil, nil, false)

	_, err := provider.ClientForDBCluster(context.Background(), cluster)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported provider type")
}

func TestDBClusterTenantClientProviderSharesClientByCluster(t *testing.T) {
	cluster := newSiteDBClusterForTenantClientProviderTest()
	managementClient := fake.NewClientBuilder().
		WithScheme(newTenantClientProviderTestScheme(t)).
		WithObjects(newPublishedTenantKubeconfigSecret(cluster.Name)).
		Build()
	provider := NewDBClusterTenantClientProvider(nil, managementClient, nil, true)

	firstClient, err := provider.ClientForDBCluster(context.Background(), cluster)
	require.NoError(t, err)
	secondClient, err := provider.ClientForDBCluster(context.Background(), cluster)
	require.NoError(t, err)
	require.Same(t, firstClient, secondClient)
}

func TestDBClusterTenantClientProviderRebuildsClientAfterSecretRotation(t *testing.T) {
	ctx := context.Background()
	cluster := newSiteDBClusterForTenantClientProviderTest()
	managementClient := fake.NewClientBuilder().
		WithScheme(newTenantClientProviderTestScheme(t)).
		WithObjects(newPublishedTenantKubeconfigSecret(cluster.Name)).
		Build()
	provider := NewDBClusterTenantClientProvider(nil, managementClient, nil, true)

	firstClient, err := provider.ClientForDBCluster(ctx, cluster)
	require.NoError(t, err)

	secret := &corev1.Secret{}
	require.NoError(t, managementClient.Get(ctx, types.NamespacedName{
		Namespace: tenantAccessKubeconfigNamespace,
		Name:      cluster.Name + "-kubeconfig",
	}, secret))
	previousResourceVersion := secret.ResourceVersion
	secret.Data[tenantAccessKubeconfigKey] = bytes.ReplaceAll(
		secret.Data[tenantAccessKubeconfigKey],
		[]byte("tenant.example"),
		[]byte("rotated-tenant.example"),
	)
	require.NoError(t, managementClient.Update(ctx, secret))
	require.NoError(t, managementClient.Get(ctx, types.NamespacedName{
		Namespace: tenantAccessKubeconfigNamespace,
		Name:      cluster.Name + "-kubeconfig",
	}, secret))
	require.NotEqual(t, previousResourceVersion, secret.ResourceVersion)

	rotatedClient, err := provider.ClientForDBCluster(ctx, cluster)
	require.NoError(t, err)
	require.NotSame(t, firstClient, rotatedClient)

	cachedRotatedClient, err := provider.ClientForDBCluster(ctx, cluster)
	require.NoError(t, err)
	require.Same(t, rotatedClient, cachedRotatedClient)
}

func TestDBClusterTenantClientProviderDoesNotCacheFailure(t *testing.T) {
	ctx := context.Background()
	cluster := newSiteDBClusterForTenantClientProviderTest()
	managementClient := fake.NewClientBuilder().WithScheme(newTenantClientProviderTestScheme(t)).Build()
	provider := NewDBClusterTenantClientProvider(nil, managementClient, nil, true)

	_, err := provider.ClientForDBCluster(ctx, cluster)
	require.Error(t, err)
	secret := newPublishedTenantKubeconfigSecret(cluster.Name)
	secret.ResourceVersion = ""
	require.NoError(t, managementClient.Create(ctx, secret))

	tenantClient, err := provider.ClientForDBCluster(ctx, cluster)
	require.NoError(t, err)
	require.NotNil(t, tenantClient)
}

func TestDBClusterTenantClientProviderKeepsNodeAgentFallbackWhenTenantAccessDisabled(t *testing.T) {
	cluster := newSiteDBClusterForTenantClientProviderTest()
	database := &testutil.MockDB{}
	database.On("ListNodes", mock.Anything, db.NodeFilters{ClusterID: &cluster.ID}).Return([]db.Node{}, nil)
	provider := NewDBClusterTenantClientProvider(database, nil, nil, false)

	_, err := provider.ClientForDBCluster(context.Background(), cluster)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrTenantClientNotReady)
	database.AssertExpectations(t)
}

func newTenantClientProviderTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, clusterv1.AddToScheme(scheme))
	return scheme
}

func newTenantClientProviderTestKubeconfig(t *testing.T) []byte {
	t.Helper()

	return []byte(`apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:6443
  name: test-cluster
contexts:
- context:
    cluster: test-cluster
    user: test-user
  name: test-context
current-context: test-context
users:
- name: test-user
  user:
    token: test-token
`)
}

func newTenantClientProviderExecKubeconfig() []byte {
	return []byte(`apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://tenant.example:6443
  name: tenant
contexts:
- context:
    cluster: tenant
    user: spiffe-user
  name: tenant
current-context: tenant
users:
- name: spiffe-user
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: /opt/spiffe/bin/k8s-spiffe-workload-jwt-exec-auth
      interactiveMode: Never
      env:
      - name: SPIFFE_ENDPOINT_SOCKET
        value: unix:///spiffe-workload-api/spire-agent.sock
      - name: SPIFFE_JWT_AUDIENCE
        value: tenant-a
`)
}

func newSiteDBClusterForTenantClientProviderTest() db.Cluster {
	cluster := db.Cluster{
		ID:   uuid.New(),
		Name: "tenant-a",
	}
	cluster.SetNicoProvider(db.NicoClusterProvider{OrgID: "org-1", SiteID: uuid.New(), VpcID: uuid.New()})
	return cluster
}

func newPublishedTenantKubeconfigSecret(clusterName string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       tenantAccessKubeconfigNamespace,
			Name:            clusterName + "-kubeconfig",
			ResourceVersion: "1",
		},
		Data: map[string][]byte{
			tenantAccessKubeconfigKey: newTenantClientProviderExecKubeconfig(),
		},
	}
}

func newCAPIClusterForTenantClientProviderTest() *clusterv1.Cluster {
	return &clusterv1.Cluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "Cluster",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "cluster-test",
			UID:       types.UID("test-cluster-uid"),
		},
	}
}
