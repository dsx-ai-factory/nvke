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
	// Record the old key's storage name too, as on the staging path:
	// finalization overwrites it, and pre-rotation backups need it. An earlier
	// rotation that failed in remove_old_keys may have already dropped the old
	// key from some nodes, so a name that cannot be proven is left unrecorded.
	activeName, err := a.discoverEncryptionKeyName(ctx, clusterID, string(secret.Data[constants.EtcdEncryptionKeyFieldActive]), true)
	if err != nil {
		return err
	}
	if activeName != "" {
		if stored := string(secret.Data[constants.EtcdEncryptionKeyFieldActiveName]); stored != "" && stored != activeName {
			return encryptionIdentityError("managed active encryption identity disagrees with the control plane")
		}
		secret.Data[constants.EtcdEncryptionKeyFieldActiveName] = []byte(activeName)
	}
	if err := a.k8sClient.Update(ctx, secret); err != nil {
		return encryptionIdentityError("cannot persist pending encryption storage identity")
	}
	return nil
}
