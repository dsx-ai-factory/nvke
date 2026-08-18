// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func TestInstallKubeLocalDNSAppliesManifestWithTenantClientProvider(t *testing.T) {
	cluster := db.Cluster{ID: uuid.New(), Name: "cluster-a"}
	database := &testutil.MockDB{}
	database.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil).Twice()

	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	tenantClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	provider := &fakeTenantClientProvider{tenantClient: tenantClient}
	a := &Activities{db: database, tenantClientProvider: provider}

	input := InstallKubeLocalDNSInput{ClusterID: cluster.ID}
	require.NoError(t, executeInstallKubeLocalDNSActivity(t, a, input))
	require.NoError(t, executeInstallKubeLocalDNSActivity(t, a, input), "the activity must be safe to retry")

	var serviceAccount corev1.ServiceAccount
	require.NoError(t, tenantClient.Get(context.Background(), types.NamespacedName{
		Namespace: metav1.NamespaceSystem,
		Name:      "node-local-dns",
	}, &serviceAccount))

	var upstreamService corev1.Service
	require.NoError(t, tenantClient.Get(context.Background(), types.NamespacedName{
		Namespace: metav1.NamespaceSystem,
		Name:      "kube-dns-upstream",
	}, &upstreamService))
	require.Equal(t, map[string]string{"k8s-app": "kube-dns"}, upstreamService.Spec.Selector)

	var configMap corev1.ConfigMap
	require.NoError(t, tenantClient.Get(context.Background(), types.NamespacedName{
		Namespace: metav1.NamespaceSystem,
		Name:      "node-local-dns",
	}, &configMap))
	require.Contains(t, configMap.Data["Corefile"], "bind 169.254.20.10 10.96.0.10")

	var daemonSet appsv1.DaemonSet
	require.NoError(t, tenantClient.Get(context.Background(), types.NamespacedName{
		Namespace: metav1.NamespaceSystem,
		Name:      "node-local-dns",
	}, &daemonSet))
	require.Equal(t, "registry.k8s.io/dns/k8s-dns-node-cache:1.23.1", daemonSet.Spec.Template.Spec.Containers[0].Image)

	var metricsService corev1.Service
	require.NoError(t, tenantClient.Get(context.Background(), types.NamespacedName{
		Namespace: metav1.NamespaceSystem,
		Name:      "node-local-dns",
	}, &metricsService))
	require.Equal(t, corev1.ClusterIPNone, metricsService.Spec.ClusterIP)

	require.Equal(t, 2, provider.calls)
	require.Equal(t, cluster, provider.cluster)
	database.AssertExpectations(t)
}

func TestInstallKubeLocalDNSReturnsTenantClientProviderError(t *testing.T) {
	cluster := db.Cluster{ID: uuid.New(), Name: "cluster-a"}
	database := &testutil.MockDB{}
	database.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil).Once()

	providerErr := errors.New("tenant API unavailable")
	a := &Activities{
		db:                   database,
		tenantClientProvider: &fakeTenantClientProvider{err: providerErr},
	}

	err := executeInstallKubeLocalDNSActivity(t, a, InstallKubeLocalDNSInput{ClusterID: cluster.ID})
	require.ErrorContains(t, err, "failed to build tenant client for cluster")
	require.ErrorContains(t, err, providerErr.Error())
	database.AssertExpectations(t)
}

func executeInstallKubeLocalDNSActivity(t *testing.T, a *Activities, input InstallKubeLocalDNSInput) error {
	t.Helper()

	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestActivityEnvironment()
	env.RegisterActivity(a.InstallKubeLocalDNS)

	_, err := env.ExecuteActivity(a.InstallKubeLocalDNS, input)
	return err
}
