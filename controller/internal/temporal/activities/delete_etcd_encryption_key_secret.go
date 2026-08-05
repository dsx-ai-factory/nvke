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
	"fmt"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/NVIDIA/nke/controller/internal/constants"
)

type DeleteEtcdEncryptionKeySecretInput struct {
	ClusterID uuid.UUID

	ClusterDeletingConditionMessage string
}

func (a *Activities) DeleteEtcdEncryptionKeySecret(ctx context.Context, input DeleteEtcdEncryptionKeySecretInput) error {
	conditionUpdateWrapper := ActivityStatusConditionUpdateWrapper[struct{}]{
		Message:         input.ClusterDeletingConditionMessage,
		UpdateCondition: a.clusterDeletingConditionMessageUpdater(input.ClusterID),
		RenderError:     messageForTemporalError,
		ActivityBody: func() (struct{}, error) {
			return struct{}{}, a.deleteEtcdEncryptionKeySecret(ctx, input.ClusterID)
		},
	}

	_, err := conditionUpdateWrapper.Run(ctx)
	return err
}

func (a *Activities) deleteEtcdEncryptionKeySecret(ctx context.Context, clusterID uuid.UUID) error {
	if a.k8sClient == nil {
		return fmt.Errorf("kubernetes client is required to delete etcd encryption key secret")
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: a.cfg.PodNamespace,
			Name:      constants.EtcdEncryptionKeySecretName(clusterID),
		},
	}
	if err := a.k8sClient.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting etcd encryption key secret %s/%s: %w", secret.Namespace, secret.Name, err)
	}

	return nil
}
