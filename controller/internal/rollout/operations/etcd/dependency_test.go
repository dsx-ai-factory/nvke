// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package etcd

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/NVIDIA/nke/controller/internal/constants"
)

func TestKubernetesEncryptionKeyReaderRotationRequiresPendingKey(t *testing.T) {
	clusterID := uuid.New()
	namespace := "cloud-workflow-engine"
	k8sClient := fake.NewClientBuilder().WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      constants.EtcdEncryptionKeySecretName(clusterID),
		},
		Data: map[string][]byte{
			constants.EtcdEncryptionKeyFieldActive: []byte("active-key"),
		},
	}).Build()
	reader := NewKubernetesEncryptionKeyReader(k8sClient, namespace)

	enablementKey, err := reader.ReadEnablementKey(context.Background(), clusterID)
	require.NoError(t, err)
	require.Equal(t, "active-key", enablementKey)

	_, err = reader.ReadRotationKey(context.Background(), clusterID)
	require.ErrorContains(t, err, "has no pending rotation key")

	k8sClientWithPending := fake.NewClientBuilder().WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      constants.EtcdEncryptionKeySecretName(clusterID),
		},
		Data: map[string][]byte{
			constants.EtcdEncryptionKeyFieldActive:  []byte("active-key"),
			constants.EtcdEncryptionKeyFieldPending: []byte("pending-key"),
		},
	}).Build()
	readerWithPending := NewKubernetesEncryptionKeyReader(k8sClientWithPending, namespace)

	rotationKey, err := readerWithPending.ReadRotationKey(context.Background(), clusterID)
	require.NoError(t, err)
	require.Equal(t, "pending-key", rotationKey)
}
