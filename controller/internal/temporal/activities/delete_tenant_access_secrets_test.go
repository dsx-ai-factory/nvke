// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package activities

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/controllers/tenantaccess"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func tenantAccessScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	return scheme
}

func tenantAccessSecretExists(t *testing.T, c client.Client, name string) bool {
	t.Helper()
	err := c.Get(context.Background(), types.NamespacedName{Namespace: tenantaccess.SecretsNamespace, Name: name}, &corev1.Secret{})
	if apierrors.IsNotFound(err) {
		return false
	}
	require.NoError(t, err)
	return true
}

func TestDeleteTenantAccessSecrets_DeletesBothAndIsIdempotent(t *testing.T) {
	clusterID := uuid.New()
	clusterName := "tenant-a"
	kubeconfigName := tenantaccess.KubeconfigSecretName(clusterName)
	trustBundleName := tenantaccess.TrustBundleSecretName(clusterName)

	newSecret := func(name string) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: tenantaccess.SecretsNamespace, Name: name}}
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(tenantAccessScheme(t)).
		WithObjects(newSecret(kubeconfigName), newSecret(trustBundleName)).
		Build()

	mockDB := &testutil.MockDB{}

	a := &Activities{
		cfg:       config.Config{TenantAccessEnabled: true},
		db:        mockDB,
		k8sClient: k8sClient,
	}

	// Both published Secrets are removed.
	require.NoError(t, a.DeleteTenantAccessSecrets(context.Background(), DeleteTenantAccessSecretsInput{ClusterID: clusterID, ClusterName: clusterName}))
	assert.False(t, tenantAccessSecretExists(t, k8sClient, kubeconfigName), "kubeconfig secret should be deleted")
	assert.False(t, tenantAccessSecretExists(t, k8sClient, trustBundleName), "trust-bundle secret should be deleted")

	// Idempotent: a retry with the Secrets already gone is a no-op, not an error.
	require.NoError(t, a.DeleteTenantAccessSecrets(context.Background(), DeleteTenantAccessSecretsInput{ClusterID: clusterID, ClusterName: clusterName}))

	// The name is carried in, so the step never reads the cluster row -- it runs
	// after that row has been deleted.
	mockDB.AssertNotCalled(t, "GetCluster", mock.Anything, mock.Anything)
}

func TestDeleteTenantAccessSecrets_RequiresK8sClient(t *testing.T) {
	a := &Activities{cfg: config.Config{TenantAccessEnabled: true}}
	require.Error(t, a.DeleteTenantAccessSecrets(context.Background(), DeleteTenantAccessSecretsInput{ClusterID: uuid.New(), ClusterName: "tenant-a"}))
}

// With tenant access disabled the controller never published anything and CWE
// holds no Secret grant in the namespace, so teardown must skip the delete
// outright rather than fail Forbidden and retry forever.
func TestDeleteTenantAccessSecrets_DisabledIsNoOp(t *testing.T) {
	clusterID := uuid.New()
	clusterName := "tenant-a"
	kubeconfigName := tenantaccess.KubeconfigSecretName(clusterName)

	k8sClient := fake.NewClientBuilder().
		WithScheme(tenantAccessScheme(t)).
		WithObjects(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: tenantaccess.SecretsNamespace, Name: kubeconfigName}}).
		Build()

	mockDB := &testutil.MockDB{}

	a := &Activities{
		cfg:       config.Config{TenantAccessEnabled: false},
		db:        mockDB,
		k8sClient: k8sClient,
	}

	require.NoError(t, a.DeleteTenantAccessSecrets(context.Background(), DeleteTenantAccessSecretsInput{ClusterID: clusterID, ClusterName: clusterName}))

	// Nothing was touched: no DB read, no delete.
	mockDB.AssertNotCalled(t, "GetCluster", mock.Anything, mock.Anything)
	assert.True(t, tenantAccessSecretExists(t, k8sClient, kubeconfigName), "secret should be left alone when tenant access is disabled")
}

// The gate must hold before the client guard: a worker with no k8s client and
// tenant access disabled still has nothing to do.
func TestDeleteTenantAccessSecrets_DisabledWithoutK8sClient(t *testing.T) {
	a := &Activities{cfg: config.Config{TenantAccessEnabled: false}}
	require.NoError(t, a.DeleteTenantAccessSecrets(context.Background(), DeleteTenantAccessSecretsInput{ClusterID: uuid.New(), ClusterName: "tenant-a"}))
}

// The Secrets are name-keyed and the row is already gone, so an empty name means
// the workflow failed to carry it -- fail loudly rather than delete "-kubeconfig".
func TestDeleteTenantAccessSecrets_RequiresClusterName(t *testing.T) {
	a := &Activities{
		cfg:       config.Config{TenantAccessEnabled: true},
		k8sClient: fake.NewClientBuilder().WithScheme(tenantAccessScheme(t)).Build(),
	}
	require.Error(t, a.DeleteTenantAccessSecrets(context.Background(), DeleteTenantAccessSecretsInput{ClusterID: uuid.New()}))
}
