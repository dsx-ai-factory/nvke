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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/constants"
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
	adminKubeconfigName := tenantaccess.AdminKubeconfigSecretName(clusterName)
	trustBundleName := tenantaccess.TrustBundleSecretName(clusterName)

	newSecret := func(name string) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Namespace: tenantaccess.SecretsNamespace,
			Name:      name,
			Labels: map[string]string{
				constants.LabelKeyClusterID: clusterID.String(),
			},
		}}
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(tenantAccessScheme(t)).
		WithObjects(newSecret(kubeconfigName), newSecret(adminKubeconfigName), newSecret(trustBundleName)).
		Build()

	mockDB := &testutil.MockDB{}

	a := &Activities{
		cfg:       config.Config{TenantAccessEnabled: true},
		db:        mockDB,
		k8sClient: k8sClient,
	}

	require.NoError(t, a.DeleteTenantAccessSecrets(context.Background(), DeleteTenantAccessSecretsInput{ClusterID: clusterID, ClusterName: clusterName}))
	assert.False(t, tenantAccessSecretExists(t, k8sClient, kubeconfigName), "kubeconfig secret should be deleted")
	assert.False(t, tenantAccessSecretExists(t, k8sClient, adminKubeconfigName), "admin kubeconfig secret should be deleted")
	assert.False(t, tenantAccessSecretExists(t, k8sClient, trustBundleName), "trust-bundle secret should be deleted")

	require.NoError(t, a.DeleteTenantAccessSecrets(context.Background(), DeleteTenantAccessSecretsInput{ClusterID: clusterID, ClusterName: clusterName}))
	mockDB.AssertNotCalled(t, "GetCluster", mock.Anything, mock.Anything)
}

func TestDeleteTenantAccessSecrets_RequiresK8sClient(t *testing.T) {
	a := &Activities{cfg: config.Config{TenantAccessEnabled: true}}
	require.Error(t, a.DeleteTenantAccessSecrets(context.Background(), DeleteTenantAccessSecretsInput{ClusterID: uuid.New(), ClusterName: "tenant-a"}))
}

func TestDeleteTenantAccessSecrets_DeletesWhenTenantAccessDisabled(t *testing.T) {
	clusterID := uuid.New()
	clusterName := "tenant-a"
	kubeconfigName := tenantaccess.KubeconfigSecretName(clusterName)

	k8sClient := fake.NewClientBuilder().
		WithScheme(tenantAccessScheme(t)).
		WithObjects(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Namespace: tenantaccess.SecretsNamespace,
			Name:      kubeconfigName,
			Labels:    map[string]string{constants.LabelKeyClusterID: clusterID.String()},
		}}).
		Build()

	a := &Activities{
		cfg:       config.Config{TenantAccessEnabled: false},
		k8sClient: k8sClient,
	}

	require.NoError(t, a.DeleteTenantAccessSecrets(context.Background(), DeleteTenantAccessSecretsInput{ClusterID: clusterID, ClusterName: clusterName}))
	assert.False(t, tenantAccessSecretExists(t, k8sClient, kubeconfigName), "owned secret must be deleted even when tenant access is disabled")
}

func TestDeleteTenantAccessSecrets_RequiresK8sClientWhenDisabled(t *testing.T) {
	a := &Activities{cfg: config.Config{TenantAccessEnabled: false}}
	require.Error(t, a.DeleteTenantAccessSecrets(context.Background(), DeleteTenantAccessSecretsInput{ClusterID: uuid.New(), ClusterName: "tenant-a"}))
}

func TestDeleteTenantAccessSecrets_RequiresClusterName(t *testing.T) {
	a := &Activities{
		cfg:       config.Config{TenantAccessEnabled: true},
		k8sClient: fake.NewClientBuilder().WithScheme(tenantAccessScheme(t)).Build(),
	}
	require.Error(t, a.DeleteTenantAccessSecrets(context.Background(), DeleteTenantAccessSecretsInput{ClusterID: uuid.New()}))
}

func TestDeleteTenantAccessSecrets_SkipsWrongOwner(t *testing.T) {
	clusterID := uuid.New()
	otherID := uuid.New()
	clusterName := "tenant-a"
	kubeconfigName := tenantaccess.KubeconfigSecretName(clusterName)

	k8sClient := fake.NewClientBuilder().
		WithScheme(tenantAccessScheme(t)).
		WithObjects(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Namespace: tenantaccess.SecretsNamespace,
			Name:      kubeconfigName,
			Labels:    map[string]string{constants.LabelKeyClusterID: otherID.String()},
		}}).
		Build()

	a := &Activities{k8sClient: k8sClient}
	require.NoError(t, a.DeleteTenantAccessSecrets(context.Background(), DeleteTenantAccessSecretsInput{ClusterID: clusterID, ClusterName: clusterName}))
	assert.True(t, tenantAccessSecretExists(t, k8sClient, kubeconfigName), "wrong-owner secret must not be deleted")
}

func TestDeleteTenantAccessSecrets_SkipsUnlabeledSecret(t *testing.T) {
	clusterID := uuid.New()
	clusterName := "tenant-a"
	kubeconfigName := tenantaccess.KubeconfigSecretName(clusterName)

	k8sClient := fake.NewClientBuilder().
		WithScheme(tenantAccessScheme(t)).
		WithObjects(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Namespace: tenantaccess.SecretsNamespace,
			Name:      kubeconfigName,
		}}).
		Build()

	a := &Activities{k8sClient: k8sClient}
	require.NoError(t, a.DeleteTenantAccessSecrets(context.Background(), DeleteTenantAccessSecretsInput{ClusterID: clusterID, ClusterName: clusterName}))
	assert.True(t, tenantAccessSecretExists(t, k8sClient, kubeconfigName), "unlabeled secret must not be deleted")
}

func TestDeleteTenantAccessSecrets_TeardownIsolation(t *testing.T) {
	clusterA := uuid.New()
	clusterB := uuid.New()
	nameA, nameB := "alpha", "beta"

	k8sClient := fake.NewClientBuilder().
		WithScheme(tenantAccessScheme(t)).
		WithObjects(
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: tenantaccess.SecretsNamespace}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Namespace: tenantaccess.SecretsNamespace,
				Name:      tenantaccess.KubeconfigSecretName(nameA),
				Labels:    map[string]string{constants.LabelKeyClusterID: clusterA.String()},
			}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Namespace: tenantaccess.SecretsNamespace,
				Name:      tenantaccess.KubeconfigSecretName(nameB),
				Labels:    map[string]string{constants.LabelKeyClusterID: clusterB.String()},
			}},
		).
		Build()

	a := &Activities{k8sClient: k8sClient}
	require.NoError(t, a.DeleteTenantAccessSecrets(context.Background(), DeleteTenantAccessSecretsInput{ClusterID: clusterA, ClusterName: nameA}))

	assert.False(t, tenantAccessSecretExists(t, k8sClient, tenantaccess.KubeconfigSecretName(nameA)))
	assert.True(t, tenantAccessSecretExists(t, k8sClient, tenantaccess.KubeconfigSecretName(nameB)), "cluster B secret must remain")
	require.NoError(t, k8sClient.Get(context.Background(), types.NamespacedName{Name: tenantaccess.SecretsNamespace}, &corev1.Namespace{}))
}
