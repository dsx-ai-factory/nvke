// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tenantaccess

import (
	"context"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dsx-ai-factory/nke/controller/internal/capi"
	"github.com/dsx-ai-factory/nke/controller/internal/constants"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

func TestUpsertOwnedSecretUpdatesMatchingTypeInPlace(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cluster := db.Cluster{ID: uuid.New(), Name: "existing"}
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      KubeconfigSecretName(cluster.Name),
			Namespace: SecretsNamespace,
			UID:       types.UID("existing-uid"),
			Labels: map[string]string{
				"owner.nke.nvidia.com/cluster": cluster.ID.String(),
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{KubeconfigSecretKey: []byte("old")},
	}
	desired := desiredKubeconfigSecret(cluster, corev1.SecretTypeOpaque, "new")
	k8sClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()

	require.NoError(t, upsertOwnedSecret(ctx, k8sClient, slog.Default(), cluster, desired))

	got := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(existing), got))
	assert.Equal(t, types.UID("existing-uid"), got.UID)
	assert.Equal(t, []byte("new"), got.Data[KubeconfigSecretKey])
	assert.Equal(t, cluster.ID.String(), got.Labels["owner.nke.nvidia.com/cluster"])
	assert.Equal(t, "cloud-workflow-engine", got.Labels[constants.LabelKeyNkeManagedBy])
	assert.Equal(t, cluster.ID.String(), got.Labels[constants.LabelKeyClusterID])
	assert.Equal(t, cluster.Name, got.Labels[clusterv1.ClusterNameLabel])
}

func TestUpsertOwnedSecretReplacesIncompatibleType(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cluster := db.Cluster{ID: uuid.New(), Name: "replace"}
	existing := desiredKubeconfigSecret(cluster, clusterv1.ClusterSecretType, "old")
	existing.UID = types.UID("existing-uid")
	baseClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	k8sClient := &recordingClient{Client: baseClient}

	require.NoError(t, upsertOwnedSecret(
		ctx,
		k8sClient,
		slog.Default(),
		cluster,
		desiredKubeconfigSecret(cluster, corev1.SecretTypeOpaque, "new"),
	))

	got := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(existing), got))
	assert.Equal(t, 1, k8sClient.deletes)
	assert.Equal(t, types.UID("existing-uid"), k8sClient.deleteUID)
	assert.NotEqual(t, types.UID("existing-uid"), got.UID)
	assert.Equal(t, corev1.SecretTypeOpaque, got.Type)
	assert.Equal(t, []byte("new"), got.Data[KubeconfigSecretKey])
	assert.Equal(t, "cloud-workflow-engine", got.Labels[constants.LabelKeyNkeManagedBy])
	assert.Equal(t, cluster.Name, got.Labels[clusterv1.ClusterNameLabel])
}

func TestUpsertOwnedSecretStopsAfterDeletePreconditionConflict(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cluster := db.Cluster{ID: uuid.New(), Name: "delete-conflict"}
	existing := desiredKubeconfigSecret(cluster, clusterv1.ClusterSecretType, "old")
	existing.UID = types.UID("existing-uid")
	baseClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	k8sClient := &recordingClient{
		Client: baseClient,
		deleteErr: apierrors.NewConflict(
			schema.GroupResource{Resource: "secrets"},
			existing.Name,
			field.Invalid(field.NewPath("metadata", "uid"), existing.UID, "precondition failed"),
		),
	}

	err := upsertOwnedSecret(
		ctx,
		k8sClient,
		slog.Default(),
		cluster,
		desiredKubeconfigSecret(cluster, corev1.SecretTypeOpaque, "new"),
	)
	require.Error(t, err)
	assert.True(t, apierrors.IsConflict(err))
	assert.Equal(t, 1, k8sClient.deletes)
	assert.Equal(t, types.UID("existing-uid"), k8sClient.deleteUID)

	got := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(existing), got))
	assert.Equal(t, types.UID("existing-uid"), got.UID)
	assert.Equal(t, clusterv1.ClusterSecretType, got.Type)
	assert.Equal(t, []byte("old"), got.Data[KubeconfigSecretKey])
}

func TestUpsertOwnedSecretDoesNotDeleteWrongOwner(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cluster := db.Cluster{ID: uuid.New(), Name: "wrong-owner"}
	existing := desiredKubeconfigSecret(cluster, clusterv1.ClusterSecretType, "old")
	existing.Labels[constants.LabelKeyClusterID] = uuid.New().String()
	baseClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	k8sClient := &recordingClient{Client: baseClient}

	err := upsertOwnedSecret(
		ctx,
		k8sClient,
		slog.Default(),
		cluster,
		desiredKubeconfigSecret(cluster, corev1.SecretTypeOpaque, "new"),
	)
	require.ErrorIs(t, err, capi.ErrWrongSecretOwner)
	assert.Equal(t, 0, k8sClient.deletes)

	got := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(existing), got))
	assert.Equal(t, clusterv1.ClusterSecretType, got.Type)
	assert.Equal(t, []byte("old"), got.Data[KubeconfigSecretKey])
}

func TestUpsertOwnedSecretDoesNotDeleteOnValidationFailure(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cluster := db.Cluster{ID: uuid.New(), Name: "invalid"}
	existing := desiredKubeconfigSecret(cluster, corev1.SecretTypeOpaque, "old")
	existing.UID = types.UID("existing-uid")
	baseClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	k8sClient := &recordingClient{
		Client: baseClient,
		updateErr: apierrors.NewInvalid(
			schema.GroupKind{Kind: "Secret"},
			existing.Name,
			field.ErrorList{field.Invalid(field.NewPath("metadata", "labels"), "invalid", "invalid label")},
		),
	}

	err := upsertOwnedSecret(
		ctx,
		k8sClient,
		slog.Default(),
		cluster,
		desiredKubeconfigSecret(cluster, corev1.SecretTypeOpaque, "new"),
	)
	require.Error(t, err)
	assert.True(t, apierrors.IsInvalid(err))
	assert.Equal(t, 0, k8sClient.deletes)

	got := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(existing), got))
	assert.Equal(t, types.UID("existing-uid"), got.UID)
	assert.Equal(t, []byte("old"), got.Data[KubeconfigSecretKey])
}

func desiredKubeconfigSecret(cluster db.Cluster, secretType corev1.SecretType, value string) *corev1.Secret {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      KubeconfigSecretName(cluster.Name),
			Namespace: SecretsNamespace,
			Labels: map[string]string{
				clusterv1.ClusterNameLabel: cluster.Name,
			},
		},
		Type: secretType,
		Data: map[string][]byte{KubeconfigSecretKey: []byte(value)},
	}
	capi.SetSecretOwnership(secret, cluster, constants.LabelValueKubeconfigModeAdmin, "cloud-workflow-engine")
	return secret
}

type recordingClient struct {
	client.Client
	deletes   int
	deleteUID types.UID
	deleteErr error
	updateErr error
}

func (c *recordingClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	c.deletes++
	deleteOptions := (&client.DeleteOptions{}).ApplyOptions(opts)
	if deleteOptions.Preconditions != nil && deleteOptions.Preconditions.UID != nil {
		c.deleteUID = *deleteOptions.Preconditions.UID
	}
	if c.deleteErr != nil {
		return c.deleteErr
	}
	return c.Client.Delete(ctx, obj, opts...)
}

func (c *recordingClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if c.updateErr != nil {
		return c.updateErr
	}
	return c.Client.Update(ctx, obj, opts...)
}
