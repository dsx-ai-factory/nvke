// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package kubeadmconfig

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func kubeadmConfigCM(clusterConfig string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: kubeadmConfigName, Namespace: kubeadmConfigNamespace},
		Data:       map[string]string{clusterConfigKey: clusterConfig},
	}
}

func readClusterConfig(t *testing.T, c client.Client) string {
	t.Helper()
	var cm corev1.ConfigMap
	require.NoError(t, c.Get(context.Background(),
		client.ObjectKey{Namespace: kubeadmConfigNamespace, Name: kubeadmConfigName}, &cm))
	return cm.Data[clusterConfigKey]
}

func TestEnsureOIDCInClusterConfiguration_MergesAndWritesBack(t *testing.T) {
	const v1beta3 = `apiVersion: kubeadm.k8s.io/v1beta3
kind: ClusterConfiguration
apiServer:
  extraArgs:
    profiling: "false"
`
	c := fake.NewClientBuilder().WithObjects(kubeadmConfigCM(v1beta3)).Build()

	changed, err := EnsureOIDCInClusterConfiguration(context.Background(), c)
	require.NoError(t, err)
	assert.True(t, changed, "a cluster missing OIDC settings must be updated")

	// The persisted ClusterConfiguration now carries the OIDC arg + volume, and
	// the v1beta3 map shape is preserved.
	updated := readClusterConfig(t, c)
	merged, _, err := MergeOIDCIntoClusterConfiguration([]byte(v1beta3))
	require.NoError(t, err)
	assert.Equal(t, string(merged), updated)
}

func TestEnsureOIDCInClusterConfiguration_IdempotentNoOp(t *testing.T) {
	const base = `apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
apiServer:
  extraArgs:
  - name: profiling
    value: "false"
`
	already, _, err := MergeOIDCIntoClusterConfiguration([]byte(base))
	require.NoError(t, err)

	c := fake.NewClientBuilder().WithObjects(kubeadmConfigCM(string(already))).Build()

	changed, err := EnsureOIDCInClusterConfiguration(context.Background(), c)
	require.NoError(t, err)
	assert.False(t, changed, "a cluster already carrying OIDC settings must not be rewritten")
	assert.Equal(t, string(already), readClusterConfig(t, c), "ConfigMap must be untouched on a no-op")
}

func TestEnsureOIDCInClusterConfiguration_Errors(t *testing.T) {
	t.Run("missing kubeadm-config ConfigMap", func(t *testing.T) {
		c := fake.NewClientBuilder().Build()
		changed, err := EnsureOIDCInClusterConfiguration(context.Background(), c)
		require.Error(t, err)
		assert.False(t, changed)
	})
	t.Run("missing ClusterConfiguration key", func(t *testing.T) {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: kubeadmConfigName, Namespace: kubeadmConfigNamespace},
			Data:       map[string]string{"OtherKey": "x"},
		}
		c := fake.NewClientBuilder().WithObjects(cm).Build()
		changed, err := EnsureOIDCInClusterConfiguration(context.Background(), c)
		require.Error(t, err)
		assert.False(t, changed)
	})
	t.Run("malformed ClusterConfiguration", func(t *testing.T) {
		c := fake.NewClientBuilder().WithObjects(kubeadmConfigCM("apiServer: [not-a-mapping")).Build()
		changed, err := EnsureOIDCInClusterConfiguration(context.Background(), c)
		require.Error(t, err)
		assert.False(t, changed)
	})
}
