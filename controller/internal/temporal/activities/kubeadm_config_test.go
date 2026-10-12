// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/kubeadmconfig"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestKubeadmConfigActivitiesUseTenantClientProvider(t *testing.T) {
	ctx := context.Background()
	cluster := db.Cluster{ID: uuid.New(), Name: "tenant-a"}
	database := &testutil.MockDB{}
	database.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil).Times(3)

	tenantClient := newKubeadmConfigTenantClient(t, `apiVersion: kubeadm.k8s.io/v1beta3
kind: ClusterConfiguration
kubernetesVersion: v1.30.9
apiServer:
  extraArgs:
    profiling: "false"
`)
	provider := &fakeTenantClientProvider{tenantClient: tenantClient}
	a := &Activities{db: database, tenantClientProvider: provider}
	input := KubeadmConfigClusterInput{ClusterID: cluster.ID}

	require.NoError(t, a.ValidateEtcdEncryptionKubeadmConfig(ctx, input))
	configAfterValidation := readTenantKubeadmConfig(t, tenantClient)
	assert.NotContains(t, configAfterValidation, "encryption-provider-config")

	mergeResult, err := a.MergeEtcdEncryptionKubeadmConfig(ctx, input)
	require.NoError(t, err)
	assert.True(t, mergeResult.Changed)
	assert.Contains(t, readTenantKubeadmConfig(t, tenantClient), "encryption-provider-config")

	updateResult, err := a.UpdateKubeadmKubernetesVersion(ctx, UpdateKubeadmKubernetesVersionInput{
		ClusterID:         cluster.ID,
		KubernetesVersion: "1.31.4",
	})
	require.NoError(t, err)
	assert.True(t, updateResult.Changed)
	assert.Contains(t, readTenantKubeadmConfig(t, tenantClient), "kubernetesVersion: v1.31.4")

	assert.Equal(t, 3, provider.calls)
	assert.Equal(t, cluster, provider.cluster)
	database.AssertExpectations(t)
}

func TestValidateEtcdQuotaReadsMirrorPodsAndConfigMapWithOneTenantClient(t *testing.T) {
	ctx := context.Background()
	cluster := db.Cluster{ID: uuid.New(), Name: "tenant-a"}
	nodes := []db.Node{
		{
			ID: uuid.New(), ClusterID: cluster.ID, Name: "cp-0", IP: "192.0.2.10", ControlPlane: true,
			Conditions: db.NodeConditions{Joined: db.Condition{Status: db.ConditionStatusTrue}},
		},
		{
			ID: uuid.New(), ClusterID: cluster.ID, Name: "cp-1", IP: "192.0.2.11", ControlPlane: true,
			Conditions: db.NodeConditions{Joined: db.Condition{Status: db.ConditionStatusTrue}},
		},
	}

	database := &testutil.MockDB{}
	database.On("ListNodes", mock.Anything, mock.MatchedBy(func(filters db.NodeFilters) bool {
		return filters.ClusterID != nil && *filters.ClusterID == cluster.ID &&
			filters.ControlPlane != nil && *filters.ControlPlane
	})).Return(nodes, nil).Once()

	tenantClient := newKubeadmConfigTenantClient(t, `apiVersion: kubeadm.k8s.io/v1beta3
kind: ClusterConfiguration
etcd:
  local:
    dataDir: /var/lib/etcd
    extraArgs:
      quota-backend-bytes: "17179869184"
`)
	for _, node := range nodes {
		require.NoError(t, tenantClient.Create(ctx, etcdMirrorPod(node.Name, "--quota-backend-bytes="+kubeadmconfig.EtcdQuotaBackendBytes)))
	}
	provider := &fakeTenantClientProvider{tenantClient: tenantClient}
	a := &Activities{db: database, tenantClientProvider: provider}

	require.NoError(t, a.validateEtcdQuota(ctx, cluster))
	assert.Equal(t, 1, provider.calls)
	assert.Equal(t, cluster, provider.cluster)
	database.AssertExpectations(t)
}

func TestEtcdQuotaFromPodAcceptsSplitFlag(t *testing.T) {
	pod := etcdMirrorPod("cp-0", "--quota-backend-bytes", kubeadmconfig.EtcdQuotaBackendBytes)
	quota, err := etcdQuotaFromPod(*pod)
	require.NoError(t, err)
	assert.Equal(t, kubeadmconfig.EtcdQuotaBackendBytes, quota)
}

func newKubeadmConfigTenantClient(t *testing.T, clusterConfiguration string) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kubeadm-config", Namespace: "kube-system"},
		Data:       map[string]string{"ClusterConfiguration": clusterConfiguration},
	}).Build()
}

func readTenantKubeadmConfig(t *testing.T, tenantClient client.Client) string {
	t.Helper()
	var cm corev1.ConfigMap
	require.NoError(t, tenantClient.Get(context.Background(), client.ObjectKey{
		Namespace: "kube-system",
		Name:      "kubeadm-config",
	}, &cm))
	return cm.Data["ClusterConfiguration"]
}

func etcdMirrorPod(nodeName string, args ...string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "etcd-" + nodeName, Namespace: "kube-system"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:    "etcd",
			Command: []string{"etcd"},
			Args:    args,
		}}},
	}
}
