// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"

	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
)

func (a *Activities) ensureRotationKeyNames(ctx context.Context, clusterID uuid.UUID, secret *corev1.Secret) error {
	// Pending bytes may predate this upgrade and already be installed under an
	// auto-derived name. Adopt that name, including partially distributed keys.
	pendingName, err := a.discoverEncryptionKeyName(ctx, clusterID, string(secret.Data[constants.EtcdEncryptionKeyFieldPending]), true)
	if err != nil {
		return err
	}
	stored := string(secret.Data[constants.EtcdEncryptionKeyFieldPendingName])
	if stored != "" && pendingName != "" && stored != pendingName {
		return encryptionIdentityError("managed pending encryption identity disagrees with the control plane")
	}
	if stored == "" {
		if pendingName == "" {
			pendingName = "kube-api-encryption-key-" + uuid.NewString()
		}
		secret.Data[constants.EtcdEncryptionKeyFieldPendingName] = []byte(pendingName)
	}
	if err := a.k8sClient.Update(ctx, secret); err != nil {
		return encryptionIdentityError("cannot persist pending encryption storage identity")
	}
	return nil
}
