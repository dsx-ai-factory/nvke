// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/NVIDIA/nke/controller/internal/controllers/tenantaccess"
)

type DeleteTenantAccessSecretsInput struct {
	ClusterID uuid.UUID

	// ClusterName keys the Secrets to delete. It is carried from the workflow
	// rather than looked up here because this step runs after the cluster row has
	// been deleted.
	ClusterName string
}

// DeleteTenantAccessSecrets removes the per-cluster Secrets the tenant-access
// controller publishes into the management-cluster secret plane (the admin/SPIRE
// kubeconfig and the CA trust bundle). Missing Secrets are ignored, so the step
// is idempotent across retries.
//
// Unlike the other teardown steps this one does not wrap itself in a cluster
// condition update: it runs after the cluster row is deleted, so there is no row
// left to write a Deleting message to.
func (a *Activities) DeleteTenantAccessSecrets(ctx context.Context, input DeleteTenantAccessSecretsInput) error {
	// Same gate as the publishing controller: with tenant access disabled nothing
	// was ever written, and CWE is not granted Secret access in the tenant-access
	// namespace. Attempting the delete anyway would fail Forbidden — which is not
	// NotFound — and this activity retries without bound, stalling teardown.
	if !a.cfg.TenantAccessEnabled {
		return nil
	}

	if a.k8sClient == nil {
		return fmt.Errorf("kubernetes client is required to delete tenant-access secrets")
	}

	if input.ClusterName == "" {
		return fmt.Errorf("cluster name is required to delete tenant-access secrets for cluster %s", input.ClusterID)
	}

	names := []string{
		tenantaccess.KubeconfigSecretName(input.ClusterName),
		tenantaccess.TrustBundleSecretName(input.ClusterName),
	}
	for _, name := range names {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: tenantaccess.SecretsNamespace,
				Name:      name,
			},
		}
		if err := a.k8sClient.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting tenant-access secret %s/%s: %w", secret.Namespace, name, err)
		}
	}

	return nil
}
