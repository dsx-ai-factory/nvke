// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	capisecret "sigs.k8s.io/cluster-api/util/secret"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestTenantClientFactoryCachesByKubeconfigResourceVersion(t *testing.T) {
	scheme := newTestScheme(t)
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{
		Namespace: testNamespace,
		Name:      testClusterName,
	}}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       cluster.Namespace,
			Name:            capisecret.Name(cluster.Name, capisecret.Kubeconfig),
			ResourceVersion: "1",
		},
		Data: map[string][]byte{capisecret.KubeconfigDataName: testKubeconfig("first-token")},
	}
	managementClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	factory := newKubeconfigTenantClientFactory(managementClient, scheme).(*kubeconfigTenantClientFactory)
	createdClients := []client.Client{
		fake.NewClientBuilder().WithScheme(scheme).Build(),
		fake.NewClientBuilder().WithScheme(scheme).Build(),
	}
	var configs []*rest.Config
	factory.newClient = func(config *rest.Config, _ client.Options) (client.Client, error) {
		configs = append(configs, config)
		return createdClients[len(configs)-1], nil
	}

	first, err := factory.ClientForCluster(context.Background(), cluster)
	require.NoError(t, err)
	second, err := factory.ClientForCluster(context.Background(), cluster)
	require.NoError(t, err)
	require.Same(t, first, second)
	require.Len(t, configs, 1)
	require.Equal(t, tenantAPITimeout, configs[0].Timeout)

	updatedSecret := &corev1.Secret{}
	require.NoError(t, managementClient.Get(context.Background(), client.ObjectKeyFromObject(secret), updatedSecret))
	updatedSecret.Data[capisecret.KubeconfigDataName] = testKubeconfig("second-token")
	require.NoError(t, managementClient.Update(context.Background(), updatedSecret))

	third, err := factory.ClientForCluster(context.Background(), cluster)
	require.NoError(t, err)
	require.NotSame(t, first, third)
	require.Len(t, configs, 2)
}

func testKubeconfig(token string) []byte {
	return []byte(`apiVersion: v1
kind: Config
clusters:
- name: tenant
  cluster:
    server: https://tenant.example.invalid
contexts:
- name: tenant
  context:
    cluster: tenant
    user: tenant
current-context: tenant
users:
- name: tenant
  user:
    token: ` + token + "\n")
}
