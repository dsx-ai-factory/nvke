// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"

	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestPendingKubeAPIEncryptionKeyIsStableAcrossRetries(t *testing.T) {
	ctx := context.Background()
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	k8sClient := fake.NewClientBuilder().Build()
	a := &Activities{
		cfg:       config.Config{PodNamespace: "cloud-workflow-engine"},
		k8sClient: k8sClient,
	}

	pending, err := a.ensurePendingKubeAPIEncryptionKey(ctx, clusterID)
	require.NoError(t, err)
	require.NotEmpty(t, pending)

	retriedPending, err := a.ensurePendingKubeAPIEncryptionKey(ctx, clusterID)
	require.NoError(t, err)
	require.Equal(t, pending, retriedPending)

	require.NoError(t, a.finalizePendingKubeAPIEncryptionKey(ctx, clusterID, true))

	retriedAfterFinalize, err := a.ensurePendingKubeAPIEncryptionKey(ctx, clusterID)
	require.NoError(t, err)
	require.Equal(t, pending, retriedAfterFinalize)

	var secret corev1.Secret
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{
		Namespace: "cloud-workflow-engine",
		Name:      constants.EtcdEncryptionKeySecretName(clusterID),
	}, &secret))
	require.Equal(t, pending, string(secret.Data[constants.EtcdEncryptionKeyFieldActive]))
	require.NotEmpty(t, secret.Data[constants.EtcdEncryptionKeyFieldActiveSince])
	require.NotContains(t, secret.Data, constants.EtcdEncryptionKeyFieldPending)
}

func TestFinalizePendingKubeAPIEncryptionKeyRejectsSecretWithoutAKey(t *testing.T) {
	ctx := context.Background()
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	namespace := "cloud-workflow-engine"
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      constants.EtcdEncryptionKeySecretName(clusterID),
		},
	}
	k8sClient := fake.NewClientBuilder().WithObjects(secret).Build()
	a := &Activities{
		cfg:       config.Config{PodNamespace: namespace},
		k8sClient: k8sClient,
	}

	err := a.finalizePendingKubeAPIEncryptionKey(ctx, clusterID, true)
	require.Error(t, err)
	require.ErrorContains(t, err, "neither pending nor active key")
}

func TestPendingKubeAPIEncryptionRotationKeyIsFreshAndStableAcrossRetries(t *testing.T) {
	ctx := context.Background()
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	namespace := "cloud-workflow-engine"
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      constants.EtcdEncryptionKeySecretName(clusterID),
		},
		Data: map[string][]byte{
			constants.EtcdEncryptionKeyFieldActive: []byte("active-key"),
		},
	}
	k8sClient := fake.NewClientBuilder().WithObjects(secret).Build()
	a := &Activities{
		cfg:       config.Config{PodNamespace: namespace},
		k8sClient: k8sClient,
	}

	withIdentityTestNodes(t, a, clusterID, func(_ db.Node, digest string) (string, error) {
		if digest == identityTestDigest("active-key") {
			return constants.LegacyAPIEncryptionKeyName, nil
		}
		return "", nil
	})
	pending, err := a.ensurePendingKubeAPIEncryptionRotationKey(ctx, clusterID)
	require.NoError(t, err)
	require.NotEmpty(t, pending)
	require.NotEqual(t, "active-key", pending)

	retriedPending, err := a.ensurePendingKubeAPIEncryptionRotationKey(ctx, clusterID)
	require.NoError(t, err)
	require.Equal(t, pending, retriedPending)
	stored, err := a.getKubeAPIEncryptionKeySecret(ctx, clusterID)
	require.NoError(t, err)
	name := string(stored.Data[constants.EtcdEncryptionKeyFieldPendingName])
	require.NotEmpty(t, name)
	require.NotEqual(t, constants.LegacyAPIEncryptionKeyName, name)
	require.NoError(t, a.finalizePendingKubeAPIEncryptionKey(ctx, clusterID, false))
	activeName, err := a.requireKubeAPIEncryptionKeyName(ctx, clusterID)
	require.NoError(t, err)
	require.Equal(t, name, activeName)
}

func TestPendingKubeAPIEncryptionRotationKeyRequiresActiveKey(t *testing.T) {
	ctx := context.Background()
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	namespace := "cloud-workflow-engine"
	k8sClient := fake.NewClientBuilder().WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      constants.EtcdEncryptionKeySecretName(clusterID),
		},
	}).Build()
	a := &Activities{
		cfg:       config.Config{PodNamespace: namespace},
		k8sClient: k8sClient,
	}

	_, err := a.ensurePendingKubeAPIEncryptionRotationKey(ctx, clusterID)
	require.ErrorContains(t, err, "rotation requires encrypted cluster state")
	var applicationErr *temporal.ApplicationError
	require.ErrorAs(t, err, &applicationErr)
	require.True(t, applicationErr.NonRetryable())
}

func TestPendingKubeAPIEncryptionRotationKeyRejectsIdenticalActiveAndPendingKeys(t *testing.T) {
	ctx := context.Background()
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	namespace := "cloud-workflow-engine"
	k8sClient := fake.NewClientBuilder().WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      constants.EtcdEncryptionKeySecretName(clusterID),
		},
		Data: map[string][]byte{
			constants.EtcdEncryptionKeyFieldActive:  []byte("same-key"),
			constants.EtcdEncryptionKeyFieldPending: []byte("same-key"),
		},
	}).Build()
	a := &Activities{
		cfg:       config.Config{PodNamespace: namespace},
		k8sClient: k8sClient,
	}

	_, err := a.ensurePendingKubeAPIEncryptionRotationKey(ctx, clusterID)
	require.ErrorContains(t, err, "identical active and pending keys")
	var applicationErr *temporal.ApplicationError
	require.ErrorAs(t, err, &applicationErr)
	require.True(t, applicationErr.NonRetryable())
}
