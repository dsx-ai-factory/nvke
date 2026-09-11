// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tenantaccess

import (
	"context"
	"fmt"
	"log/slog"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/NVIDIA/nke/controller/internal/capi"
	"github.com/NVIDIA/nke/controller/internal/db"
)

// upsertSecret publishes one Controller-owned Secret.
//
// The live row is re-read here, immediately before the write, and not only once
// per publish: on the OIDC path publishSPIRE applies RBAC into the tenant
// cluster between the publish-time refresh and these writes, and that apply is
// bounded by tenantRequestTimeout, not by anything quick. A cluster that started
// deleting during it would otherwise have its Secrets re-created after teardown
// had already removed them, orphaning them for good once the row is gone.
func (c *Controller) upsertSecret(ctx context.Context, cluster db.Cluster, desired *corev1.Secret) error {
	live, skip, err := c.refreshCluster(ctx, cluster)
	if err != nil || skip {
		return err
	}

	capiCluster := &clusterv1.Cluster{}
	key := types.NamespacedName{Namespace: SecretsNamespace, Name: live.Name}
	if err := c.k8sClient.Get(ctx, key, capiCluster); err != nil {
		if apierrors.IsNotFound(err) {
			c.logger.Debug("CAPI cluster no longer exists; skipping tenant-access publish", "clusterID", live.ID, "clusterName", live.Name)
			return nil
		}
		return fmt.Errorf("getting CAPI cluster %s before publish: %w", key, err)
	}
	if err := controllerutil.SetControllerReference(capiCluster, desired, c.scheme); err != nil {
		return fmt.Errorf("setting owner reference on secret %s/%s: %w", desired.Namespace, desired.Name, err)
	}

	return upsertOwnedSecret(ctx, c.k8sClient, c.logger, live, desired)
}

func upsertOwnedSecret(
	ctx context.Context,
	k8sClient client.Client,
	logger *slog.Logger,
	cluster db.Cluster,
	desired *corev1.Secret,
) error {
	existing := &corev1.Secret{}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(desired), existing); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("getting secret %s/%s before upsert: %w", desired.Namespace, desired.Name, err)
	} else if err == nil && existing.Type != desired.Type {
		if err := capi.ValidateSecretOwnership(existing, cluster); err != nil {
			return err
		}

		logger.Info(
			"Replacing tenant-access secret with incompatible type",
			"clusterName", cluster.Name,
			"secret", desired.Name,
			"existingType", existing.Type,
			"desiredType", desired.Type,
		)
		deleteOptions := &client.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &existing.UID},
		}
		if err := k8sClient.Delete(ctx, existing, deleteOptions); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting secret %s/%s with incompatible type: %w", desired.Namespace, desired.Name, err)
		}
	}

	secret := &corev1.Secret{
		ObjectMeta: desired.ObjectMeta,
	}

	op, err := controllerutil.CreateOrUpdate(ctx, k8sClient, secret, func() error {
		if err := capi.ValidateSecretOwnership(secret, cluster); err != nil {
			return err
		}
		if secret.Labels == nil {
			secret.Labels = map[string]string{}
		}
		for key, value := range desired.Labels {
			secret.Labels[key] = value
		}
		secret.Type = desired.Type
		secret.OwnerReferences = desired.OwnerReferences
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		for key, value := range desired.Data {
			secret.Data[key] = value
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("upserting secret %s/%s: %w", desired.Namespace, desired.Name, err)
	}

	switch op {
	case controllerutil.OperationResultCreated:
		logger.Info("Tenant-access secret created", "clusterName", cluster.Name, "secret", desired.Name)
	case controllerutil.OperationResultNone:
	case controllerutil.OperationResultUpdated,
		controllerutil.OperationResultUpdatedStatus,
		controllerutil.OperationResultUpdatedStatusOnly:
		logger.Debug("Tenant-access secret updated", "clusterName", cluster.Name, "secret", desired.Name, "operation", string(op))
	}
	return nil
}
