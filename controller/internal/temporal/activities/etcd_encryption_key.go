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
	"strconv"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (a *Activities) ensurePendingKubeAPIEncryptionKey(ctx context.Context, clusterID uuid.UUID) (string, error) {
	secret, lookupErr := a.getKubeAPIEncryptionKeySecret(ctx, clusterID)
	if lookupErr != nil && !apierrors.IsNotFound(lookupErr) {
		return "", lookupErr
	}
	if lookupErr == nil {
		if pending := string(secret.Data[etcdEncryptionFieldPending]); pending != "" {
			return pending, nil
		}
		// Fall back to the already-active key when no pending key exists. This diverges
		// from SWE's pending-only generatePendingKey and is only reachable when a prior
		// run finalized (pending→active, pending deleted) but crashed before setting the
		// EtcdEncrypted flag; a re-run then lands here. Returning the deployed active key
		// is correct for *enabling* encryption — every node already has it, so re-Init is
		// a no-op and the run converges. WARNING: this is NOT safe for key rotation, which
		// requires a fresh key distinct from active; do not reuse this helper there.
		if active := string(secret.Data[etcdEncryptionFieldActive]); active != "" {
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
				Name:      etcdEncryptionSecretName(clusterID),
			},
			Data: map[string][]byte{etcdEncryptionFieldPending: []byte(key)},
		}
		if err := a.k8sClient.Create(ctx, secret); err != nil {
			return "", fmt.Errorf("creating pending kube-api encryption key secret %s/%s: %w", secret.Namespace, secret.Name, err)
		}
		return key, nil
	}

	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	secret.Data[etcdEncryptionFieldPending] = []byte(key)
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
	active := string(secret.Data[etcdEncryptionFieldActive])
	if active == "" {
		return "", etcdEncryptionKeyRotationPreconditionError(
			fmt.Sprintf("kube-api encryption key secret %s/%s has no active key; rotation requires encrypted cluster state", secret.Namespace, secret.Name),
		)
	}
	if pending := string(secret.Data[etcdEncryptionFieldPending]); pending != "" {
		if pending == active {
			return "", etcdEncryptionKeyRotationPreconditionError(
				fmt.Sprintf("kube-api encryption key secret %s/%s has identical active and pending keys", secret.Namespace, secret.Name),
			)
		}
		return pending, nil
	}

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
	secret.Data[etcdEncryptionFieldPending] = []byte(key)
	if err := a.k8sClient.Update(ctx, secret); err != nil {
		return "", fmt.Errorf("updating pending kube-api encryption rotation key secret %s/%s: %w", secret.Namespace, secret.Name, err)
	}
	return key, nil
}

func (a *Activities) finalizePendingKubeAPIEncryptionKey(ctx context.Context, clusterID uuid.UUID) error {
	secret, err := a.getKubeAPIEncryptionKeySecret(ctx, clusterID)
	if err != nil {
		return err
	}
	pending := secret.Data[etcdEncryptionFieldPending]
	if len(pending) == 0 {
		if len(secret.Data[etcdEncryptionFieldActive]) == 0 {
			return fmt.Errorf("kube-api encryption key secret %s/%s has neither pending nor active key", secret.Namespace, secret.Name)
		}
		return nil
	}
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	secret.Data[etcdEncryptionFieldActive] = pending
	secret.Data[etcdEncryptionFieldActiveSince] = []byte(strconv.FormatInt(time.Now().Unix(), 10))
	delete(secret.Data, etcdEncryptionFieldPending)
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
		Name:      etcdEncryptionSecretName(clusterID),
	}, secret); err != nil {
		return nil, err
	}
	return secret, nil
}
