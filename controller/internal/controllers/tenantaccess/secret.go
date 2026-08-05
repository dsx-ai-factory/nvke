// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package tenantaccess

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
)

// upsertSecret creates or updates an Opaque Secret in the tenant-access
// namespace, merging only the fields this controller owns (the owner labels and
// the single data key) so foreign labels/annotations are preserved.
func (c *Controller) upsertSecret(ctx context.Context, cluster db.Cluster, secretName, dataKey string, value []byte) error {
	// Re-read the cluster immediately before writing. Reconcile decides what to
	// publish from a ListClusters snapshot taken at the top of the pass, and a
	// pass can span minutes (a node-agent probe per cluster, plus a tenant apply
	// on the OIDC path). Without this, a cluster that started deleting mid-pass
	// would be published from the stale snapshot — re-creating Secrets teardown
	// had already removed, and orphaning them for good once the row is gone.
	live, err := c.db.GetCluster(ctx, cluster.ID)
	if errors.Is(err, db.ErrNotFound) {
		c.logger.Debug("Cluster no longer exists; skipping tenant-access secret", "clusterID", cluster.ID, "secret", secretName)
		return nil
	}
	if err != nil {
		return fmt.Errorf("re-reading cluster %s before publishing %s: %w", cluster.ID, secretName, err)
	}
	if live.Conditions.Deleting.Status == db.ConditionStatusTrue {
		c.logger.Debug("Cluster started deleting; skipping tenant-access secret", "clusterID", cluster.ID, "secret", secretName)
		return nil
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: SecretsNamespace,
		},
	}

	op, err := controllerutil.CreateOrUpdate(ctx, c.k8sClient, secret, func() error {
		if secret.Labels == nil {
			secret.Labels = map[string]string{}
		}
		secret.Labels[constants.LabelKeyNkeManagedBy] = constants.LabelValueNkeManagedBy
		secret.Labels[constants.LabelKeyClusterID] = cluster.ID.String()
		secret.Type = corev1.SecretTypeOpaque
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data[dataKey] = value
		return nil
	})
	if err != nil {
		return fmt.Errorf("upserting secret %s/%s: %w", SecretsNamespace, secretName, err)
	}

	// Creation is a one-off worth surfacing. Updates are not: this runs on every
	// poll for every cluster, so logging them at info level scales with fleet size
	// and reconcile frequency rather than with anything an operator cares about.
	switch op {
	case controllerutil.OperationResultCreated:
		c.logger.Info("Tenant-access secret created", "clusterName", cluster.Name, "secret", secretName)
	case controllerutil.OperationResultNone:
		// Steady state: the published Secret already matches. Stay quiet.
	case controllerutil.OperationResultUpdated,
		controllerutil.OperationResultUpdatedStatus,
		controllerutil.OperationResultUpdatedStatusOnly:
		c.logger.Debug("Tenant-access secret updated", "clusterName", cluster.Name, "secret", secretName, "operation", string(op))
	}
	return nil
}
