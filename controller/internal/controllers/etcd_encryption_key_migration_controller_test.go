// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package controllers

import (
	"context"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func TestEtcdEncryptionKeyMigrationControllerCopiesLegacySecret(t *testing.T) {
	ctx := context.Background()
	siteID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	clusterID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	secretName := constants.EtcdEncryptionKeySecretName(clusterID)
	mockDB := &testutil.MockDB{}
	mockDB.On("ListClusters", mock.Anything, db.ClusterFilters{SiteID: &siteID}).Return([]db.Cluster{{
		ID:            clusterID,
		Name:          "encrypted-cluster",
		SiteID:        siteID,
		EtcdEncrypted: true,
	}}, nil).Once()

	k8sClient := newEtcdKeyMigrationClient(t, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "site-workflow-engine",
			Name:      secretName,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			constants.EtcdEncryptionKeyFieldActive:      []byte("active-key"),
			constants.EtcdEncryptionKeyFieldActiveSince: []byte("123"),
			constants.EtcdEncryptionKeyFieldPending:     []byte("pending-key"),
		},
	})
	controller := newTestEtcdKeyMigrationController(mockDB, k8sClient)

	require.NoError(t, controller.Reconcile(ctx, db.Site{ID: siteID}))

	copied := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Namespace: "cloud-workflow-engine", Name: secretName}, copied))
	assert.Equal(t, corev1.SecretTypeOpaque, copied.Type)
	assert.Equal(t, "active-key", string(copied.Data[constants.EtcdEncryptionKeyFieldActive]))
	assert.Equal(t, "123", string(copied.Data[constants.EtcdEncryptionKeyFieldActiveSince]))
	assert.Equal(t, "pending-key", string(copied.Data[constants.EtcdEncryptionKeyFieldPending]))
	mockDB.AssertExpectations(t)
}

func TestEtcdEncryptionKeyMigrationControllerDoesNotOverwriteDestination(t *testing.T) {
	ctx := context.Background()
	siteID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	clusterID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	secretName := constants.EtcdEncryptionKeySecretName(clusterID)
	mockDB := &testutil.MockDB{}
	mockDB.On("ListClusters", mock.Anything, db.ClusterFilters{SiteID: &siteID}).Return([]db.Cluster{{
		ID:            clusterID,
		Name:          "encrypted-cluster",
		SiteID:        siteID,
		EtcdEncrypted: true,
	}}, nil).Once()

	k8sClient := newEtcdKeyMigrationClient(t,
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "site-workflow-engine",
				Name:      secretName,
			},
			Data: map[string][]byte{constants.EtcdEncryptionKeyFieldActive: []byte("legacy-key")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "cloud-workflow-engine",
				Name:      secretName,
			},
			Data: map[string][]byte{constants.EtcdEncryptionKeyFieldActive: []byte("existing-key")},
		},
	)
	controller := newTestEtcdKeyMigrationController(mockDB, k8sClient)

	require.NoError(t, controller.Reconcile(ctx, db.Site{ID: siteID}))

	existing := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Namespace: "cloud-workflow-engine", Name: secretName}, existing))
	assert.Equal(t, "existing-key", string(existing.Data[constants.EtcdEncryptionKeyFieldActive]))
	mockDB.AssertExpectations(t)
}

func TestEtcdEncryptionKeyMigrationControllerMissingLegacySecretIsNonFatal(t *testing.T) {
	ctx := context.Background()
	siteID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	clusterID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	secretName := constants.EtcdEncryptionKeySecretName(clusterID)
	mockDB := &testutil.MockDB{}
	mockDB.On("ListClusters", mock.Anything, db.ClusterFilters{SiteID: &siteID}).Return([]db.Cluster{{
		ID:            clusterID,
		Name:          "encrypted-cluster",
		SiteID:        siteID,
		EtcdEncrypted: true,
	}}, nil).Once()

	k8sClient := newEtcdKeyMigrationClient(t)
	controller := newTestEtcdKeyMigrationController(mockDB, k8sClient)

	require.NoError(t, controller.Reconcile(ctx, db.Site{ID: siteID}))

	missing := &corev1.Secret{}
	err := k8sClient.Get(ctx, client.ObjectKey{Namespace: "cloud-workflow-engine", Name: secretName}, missing)
	assert.True(t, apierrors.IsNotFound(err))
	mockDB.AssertExpectations(t)
}

func newTestEtcdKeyMigrationController(dbInstance db.DB, k8sClient client.Client) *EtcdEncryptionKeyMigrationController {
	return NewEtcdEncryptionKeyMigrationController(
		config.Config{
			PodNamespace:                     "cloud-workflow-engine",
			EtcdEncryptionKeyLegacyNamespace: "site-workflow-engine",
		},
		slog.Default(),
		dbInstance,
		k8sClient,
	)
}

func newEtcdKeyMigrationClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		Build()
}
