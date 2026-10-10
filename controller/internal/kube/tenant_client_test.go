// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"bytes"
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/testutil"
)

// A CAPI Cluster resolves through its controller cluster row, which is what says the
// kubeconfig comes from the Secret TenantAccess publishes.
func TestDBClusterTenantClientProviderResolvesCAPIClusterViaClusterRow(t *testing.T) {
	ctx := context.Background()
	cluster := newSiteDBClusterForTenantClientProviderTest()
	managementClient := fake.NewClientBuilder().
		WithScheme(newTenantClientProviderTestScheme(t)).
		WithObjects(newPublishedTenantKubeconfigSecret(cluster.Name)).
		Build()
	database := &testutil.MockDB{}
	database.On("ListClusters", mock.Anything, db.ClusterFilters{Name: &cluster.Name}).
		Return([]db.Cluster{cluster}, nil)
	provider := NewDBClusterTenantClientProvider(database, managementClient, nil, true)

	capiCluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{
		Name:      cluster.Name,
		Namespace: tenantAccessKubeconfigNamespace,
	}}

	tenantClient, err := provider.ClientForCluster(ctx, capiCluster)
	require.NoError(t, err)
	require.NotNil(t, tenantClient)
}

func TestDBClusterTenantClientProviderErrorsWhenCAPIClusterHasNoRow(t *testing.T) {
	name := "orphan"
	database := &testutil.MockDB{}
	database.On("ListClusters", mock.Anything, db.ClusterFilters{Name: &name}).
		Return([]db.Cluster{}, nil)
	provider := NewDBClusterTenantClientProvider(database, nil, nil, true)

	_, err := provider.ClientForCluster(context.Background(), &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "nke-system"},
	})
	require.ErrorIs(t, err, db.ErrNotFound)
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

func TestDBClusterTenantClientProviderReturnsNotReadyForEmptyCAPIPublishedSecret(t *testing.T) {
	cluster := db.Cluster{
		ID:           uuid.New(),
		Name:         "test-cluster",
		ProviderType: db.ProviderTypeCAPI,
		Capi: &db.CAPIClusterProvider{
			InfraCluster: []byte(`{"kind":"test"}`),
		},
	}
	managementClient := fake.NewClientBuilder().
		WithScheme(newTenantClientProviderTestScheme(t)).
		WithObjects(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: tenantAccessKubeconfigNamespace,
				Name:      cluster.Name + "-kubeconfig",
			},
		}).
		Build()
	provider := NewDBClusterTenantClientProvider(nil, managementClient, nil, false)

	_, err := provider.ClientForDBCluster(context.Background(), cluster)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrTenantClientNotReady)
	require.Contains(t, err.Error(), "published tenant kubeconfig")
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

func TestDBClusterTenantClientProviderCAPIUsesPublishedSecret(t *testing.T) {
	cluster := db.Cluster{
		ID:              uuid.New(),
		Name:            "test-cluster",
		OIDCAuthEnabled: true,
		ProviderType:    db.ProviderTypeCAPI,
		Capi: &db.CAPIClusterProvider{
			InfraCluster: []byte(`{"kind":"DockerCluster"}`),
		},
	}
	published := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: tenantAccessKubeconfigNamespace,
			Name:      cluster.Name + "-kubeconfig",
			Labels: map[string]string{
				"nke.nvidia.com/cluster-id":      cluster.ID.String(),
				"nke.nvidia.com/kubeconfig-mode": "spire",
			},
			ResourceVersion: "1",
		},
		Data: map[string][]byte{tenantAccessKubeconfigKey: newTenantClientProviderExecKubeconfig()},
	}

	t.Run("SPIRE published secret is used", func(t *testing.T) {
		managementClient := fake.NewClientBuilder().
			WithScheme(newTenantClientProviderTestScheme(t)).
			WithObjects(published).
			Build()
		provider := NewDBClusterTenantClientProvider(nil, managementClient, nil, false)
		tenantClient, err := provider.ClientForDBCluster(context.Background(), cluster)
		require.NoError(t, err)
		require.NotNil(t, tenantClient)
	})

	t.Run("no published secret is not ready", func(t *testing.T) {
		managementClient := fake.NewClientBuilder().
			WithScheme(newTenantClientProviderTestScheme(t)).
			Build()
		provider := NewDBClusterTenantClientProvider(nil, managementClient, nil, false)
		_, err := provider.ClientForDBCluster(context.Background(), cluster)
		require.Error(t, err)
		require.ErrorIs(t, err, ErrTenantClientNotReady)
		require.Contains(t, err.Error(), "published tenant kubeconfig")
	})
}

func newTenantClientProviderTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, clusterv1.AddToScheme(scheme))
	return scheme
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
