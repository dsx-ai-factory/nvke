// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/dsx-ai-factory/nke/controller/internal/capi"
	"github.com/dsx-ai-factory/nke/controller/internal/constants"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

func TestValidateSecretOwnership(t *testing.T) {
	t.Parallel()

	cluster := db.Cluster{ID: uuid.New(), Name: "alpha"}

	t.Run("missing labels are allowed", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, capi.ValidateSecretOwnership(&corev1.Secret{}, cluster))
		require.NoError(t, capi.ValidateSecretOwnership(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{}}}, cluster))
	})

	t.Run("matching cluster-id passes", func(t *testing.T) {
		t.Parallel()
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
			constants.LabelKeyClusterID:      cluster.ID.String(),
			constants.LabelKeyKubeconfigMode: constants.LabelValueKubeconfigModeSPIRE,
			constants.LabelKeyNkeManagedBy:   "cloud-workflow-engine",
		}}}
		require.NoError(t, capi.ValidateSecretOwnership(secret, cluster))
	})

	t.Run("wrong cluster-id is refused", func(t *testing.T) {
		t.Parallel()
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name:      "alpha-kubeconfig",
			Namespace: constants.NamespaceNKESystem,
			Labels:    map[string]string{constants.LabelKeyClusterID: uuid.New().String()},
		}}
		err := capi.ValidateSecretOwnership(secret, cluster)
		require.Error(t, err)
		assert.ErrorIs(t, err, capi.ErrWrongSecretOwner)
	})

	t.Run("kubeconfig-mode may change for the same cluster", func(t *testing.T) {
		t.Parallel()
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name:      "alpha-kubeconfig",
			Namespace: constants.NamespaceNKESystem,
			Labels: map[string]string{
				constants.LabelKeyClusterID:      cluster.ID.String(),
				constants.LabelKeyKubeconfigMode: constants.LabelValueKubeconfigModeAdmin,
			},
		}}
		require.NoError(t, capi.ValidateSecretOwnership(secret, cluster))
	})
}

func TestSetSecretOwnership(t *testing.T) {
	t.Parallel()

	cluster := db.Cluster{ID: uuid.New(), Name: "alpha"}
	secret := &corev1.Secret{}
	capi.SetSecretOwnership(secret, cluster, constants.LabelValueKubeconfigModeSPIRE, "cloud-workflow-engine")
	assert.Equal(t, "cloud-workflow-engine", secret.Labels[constants.LabelKeyNkeManagedBy])
	assert.Equal(t, cluster.ID.String(), secret.Labels[constants.LabelKeyClusterID])
	assert.Equal(t, constants.LabelValueKubeconfigModeSPIRE, secret.Labels[constants.LabelKeyKubeconfigMode])
}

func TestIsSPIREExecKubeconfig(t *testing.T) {
	t.Parallel()

	t.Run("SPIRE exec without client cert is true", func(t *testing.T) {
		t.Parallel()
		ok, err := capi.IsSPIREExecKubeconfig([]byte(`apiVersion: v1
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
`))
		require.NoError(t, err)
		assert.True(t, ok)
	})

	t.Run("client cert is fail-closed", func(t *testing.T) {
		t.Parallel()
		ok, err := capi.IsSPIREExecKubeconfig([]byte(`apiVersion: v1
kind: Config
users:
- name: admin
  user:
    client-certificate-data: Y2VydA==
    client-key-data: a2V5
`))
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("token kubeconfig is fail-closed", func(t *testing.T) {
		t.Parallel()
		ok, err := capi.IsSPIREExecKubeconfig([]byte(`apiVersion: v1
kind: Config
users:
- name: admin
  user:
    token: not-a-spire-token
`))
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("username password is fail-closed", func(t *testing.T) {
		t.Parallel()
		ok, err := capi.IsSPIREExecKubeconfig([]byte(`apiVersion: v1
kind: Config
users:
- name: admin
  user:
    username: kube
    password: secret
`))
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("auth-provider is fail-closed", func(t *testing.T) {
		t.Parallel()
		ok, err := capi.IsSPIREExecKubeconfig([]byte(`apiVersion: v1
kind: Config
users:
- name: admin
  user:
    auth-provider:
      name: oidc
`))
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("SPIRE exec plus token user is fail-closed", func(t *testing.T) {
		t.Parallel()
		ok, err := capi.IsSPIREExecKubeconfig([]byte(`apiVersion: v1
kind: Config
users:
- name: spiffe-user
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: /opt/spiffe/bin/k8s-spiffe-workload-jwt-exec-auth
- name: leftover-admin
  user:
    token: leftover
`))
		require.NoError(t, err)
		assert.False(t, ok)
	})
}
