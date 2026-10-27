// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func (a *Activities) ensurePendingKubeAPIEncryptionKey(ctx context.Context, clusterID uuid.UUID) (string, error) {
	secret, lookupErr := a.getKubeAPIEncryptionKeySecret(ctx, clusterID)
	if lookupErr != nil && !apierrors.IsNotFound(lookupErr) {
		return "", lookupErr
	}
	if lookupErr == nil {
		if pending := string(secret.Data[constants.EtcdEncryptionKeyFieldPending]); pending != "" {
			return pending, nil
		}
		// Fall back to the already-active key when no pending key exists. This diverges
		// from SWE's pending-only generatePendingKey and is only reachable when a prior
		// run finalized (pending→active, pending deleted) but crashed before setting the
		// EtcdEncrypted flag; a re-run then lands here. Returning the deployed active key
		// is correct for *enabling* encryption — every node already has it, so re-Init is
		// a no-op and the run converges. WARNING: this is NOT safe for key rotation, which
		// requires a fresh key distinct from active; do not reuse this helper there.
		if active := string(secret.Data[constants.EtcdEncryptionKeyFieldActive]); active != "" {
			return active, nil
		}
	}

	key, err := generateEncryptionKey()
	if err != nil {
		return "", fmt.Errorf("generating pending kube-api encryption key: %w", err)
	}

	if apierrors.IsNotFound(lookupErr) {
		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: a.cfg.PodNamespace,
				Name:      constants.EtcdEncryptionKeySecretName(clusterID),
			},
			Data: map[string][]byte{
				constants.EtcdEncryptionKeyFieldPending:     []byte(key),
				constants.EtcdEncryptionKeyFieldPendingName: []byte(constants.LegacyAPIEncryptionKeyName),
			},
		}
		if err := a.k8sClient.Create(ctx, secret); err != nil {
			return "", fmt.Errorf("creating pending kube-api encryption key secret %s/%s: %w", secret.Namespace, secret.Name, err)
		}
		return key, nil
	}

	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	secret.Data[constants.EtcdEncryptionKeyFieldPending] = []byte(key)
	secret.Data[constants.EtcdEncryptionKeyFieldPendingName] = []byte(constants.LegacyAPIEncryptionKeyName)
	if err := a.k8sClient.Update(ctx, secret); err != nil {
		return "", fmt.Errorf("updating pending kube-api encryption key secret %s/%s: %w", secret.Namespace, secret.Name, err)
	}
	return key, nil
}

func (a *Activities) ensurePendingKubeAPIEncryptionRotationKey(ctx context.Context, clusterID uuid.UUID) (string, error) {
	secret, err := a.getKubeAPIEncryptionKeySecret(ctx, clusterID)
	if err != nil {
		return "", fmt.Errorf("getting kube-api encryption key for rotation: %w", err)
	}
	active := string(secret.Data[constants.EtcdEncryptionKeyFieldActive])
	if active == "" {
		return "", etcdEncryptionKeyRotationPreconditionError(
			fmt.Sprintf("kube-api encryption key secret %s/%s has no active key; rotation requires encrypted cluster state", secret.Namespace, secret.Name),
		)
	}
	if pending := string(secret.Data[constants.EtcdEncryptionKeyFieldPending]); pending != "" {
		if pending == active {
			return "", etcdEncryptionKeyRotationPreconditionError(
				fmt.Sprintf("kube-api encryption key secret %s/%s has identical active and pending keys", secret.Namespace, secret.Name),
			)
		}
		if len(secret.Data[constants.EtcdEncryptionKeyFieldPendingName]) == 0 {
			cluster, err := a.db.GetCluster(ctx, clusterID)
			if err != nil {
				return "", fmt.Errorf("getting cluster for pending rotation key: %w", err)
			}
			// Legacy histories staged unnamed keys before identity discovery existed.
			if update := cluster.Conditions.UpdateInProgress; update != nil && update.Type == db.OperationTypeEtcdEncryption {
				return pending, nil
			}
		}
		if err := a.ensureRotationKeyNames(ctx, clusterID, secret); err != nil {
			return "", err
		}
		return pending, nil
	}
	activeName, err := a.discoverEncryptionKeyName(ctx, clusterID, active, false)
	if err != nil {
		return "", err
	}
	if stored := string(secret.Data[constants.EtcdEncryptionKeyFieldActiveName]); stored != "" && stored != activeName {
		return "", encryptionIdentityError("managed active encryption identity disagrees with the control plane")
	}
	secret.Data[constants.EtcdEncryptionKeyFieldActiveName] = []byte(activeName)

	key, err := generateEncryptionKey()
	if err != nil {
		return "", fmt.Errorf("generating pending kube-api encryption rotation key: %w", err)
	}
	if key == active {
		return "", fmt.Errorf("generated kube-api encryption rotation key matches the active key")
	}
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	secret.Data[constants.EtcdEncryptionKeyFieldPending] = []byte(key)
	secret.Data[constants.EtcdEncryptionKeyFieldPendingName] = []byte("kube-api-encryption-key-" + uuid.NewString())
	if err := a.k8sClient.Update(ctx, secret); err != nil {
		return "", fmt.Errorf("updating pending kube-api encryption rotation key secret %s/%s: %w", secret.Namespace, secret.Name, err)
	}
	return key, nil
}

// pendingKeyProtocol identifies the workflow that staged the pending key, which
// determines how finalization records its storage name.
type pendingKeyProtocol int

const (
	// pendingKeyEnablement distributes an unnamed pending key under the legacy name.
	pendingKeyEnablement pendingKeyProtocol = iota
	// pendingKeyLegacyRotation is a rotation history recorded before ownership.
	pendingKeyLegacyRotation
	// pendingKeyOwnedRotation always stages a named pending key.
	pendingKeyOwnedRotation
)

// finalizePendingKubeAPIEncryptionKey promotes the pending key to active. A
// recorded pending name is promoted with it. Without one, enablement records
// the legacy name, and a legacy rotation keeps its original protocol: its agents
// auto-named the key and may lack the identity RPC, so it records no name and
// the next init, join, or rotation discovers it. An owned rotation discovers it.
func (a *Activities) finalizePendingKubeAPIEncryptionKey(ctx context.Context, clusterID uuid.UUID, protocol pendingKeyProtocol) error {
	secret, err := a.getKubeAPIEncryptionKeySecret(ctx, clusterID)
	if err != nil {
		return err
	}
	pending := secret.Data[constants.EtcdEncryptionKeyFieldPending]
	if len(pending) == 0 {
		if len(secret.Data[constants.EtcdEncryptionKeyFieldActive]) == 0 {
			return fmt.Errorf("kube-api encryption key secret %s/%s has neither pending nor active key", secret.Namespace, secret.Name)
		}
		return nil
	}
	name := string(secret.Data[constants.EtcdEncryptionKeyFieldPendingName])
	switch {
	case name != "":
	case protocol == pendingKeyEnablement:
		name = constants.LegacyAPIEncryptionKeyName
	case protocol == pendingKeyLegacyRotation:
	default:
		if name, err = a.adoptEncryptionKeyName(ctx, clusterID, secret, constants.EtcdEncryptionKeyFieldPending, constants.EtcdEncryptionKeyFieldPendingName); err != nil {
			return err
		}
	}
	secret.Data[constants.EtcdEncryptionKeyFieldActive] = pending
	if name == "" {
		// The previous active name belongs to the replaced key.
		delete(secret.Data, constants.EtcdEncryptionKeyFieldActiveName)
	} else {
		secret.Data[constants.EtcdEncryptionKeyFieldActiveName] = []byte(name)
	}
	secret.Data[constants.EtcdEncryptionKeyFieldActiveSince] = []byte(strconv.FormatInt(time.Now().Unix(), 10))
	delete(secret.Data, constants.EtcdEncryptionKeyFieldPending)
	delete(secret.Data, constants.EtcdEncryptionKeyFieldPendingName)
	if err := a.k8sClient.Update(ctx, secret); err != nil {
		return fmt.Errorf("finalizing kube-api encryption key secret %s/%s: %w", secret.Namespace, secret.Name, err)
	}
	return nil
}

func (a *Activities) getKubeAPIEncryptionKeySecret(ctx context.Context, clusterID uuid.UUID) (*corev1.Secret, error) {
	if a.k8sClient == nil {
		return nil, fmt.Errorf("kubernetes client is required for kube-api encryption keys")
	}
	secret := &corev1.Secret{}
	if err := a.k8sClient.Get(ctx, client.ObjectKey{
		Namespace: a.cfg.PodNamespace,
		Name:      constants.EtcdEncryptionKeySecretName(clusterID),
	}, secret); err != nil {
		return nil, err
	}
	return secret, nil
}
