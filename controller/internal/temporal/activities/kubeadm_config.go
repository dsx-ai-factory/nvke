// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/kubeadmconfig"
)

type KubeadmConfigClusterInput struct {
	ClusterID uuid.UUID
}

type MergeEtcdEncryptionKubeadmConfigResult struct {
	Changed bool
}

// ValidateEtcdEncryptionKubeadmConfig verifies that the tenant's persisted
// kubeadm ClusterConfiguration can accept the controller's desired API encryption settings.
func (a *Activities) ValidateEtcdEncryptionKubeadmConfig(ctx context.Context, input KubeadmConfigClusterInput) error {
	tenantClient, err := a.tenantClientForClusterID(ctx, input.ClusterID)
	if err != nil {
		return err
	}
	return kubeadmconfig.ValidateEncryptionInClusterConfiguration(ctx, tenantClient)
}

// MergeEtcdEncryptionKubeadmConfig persists the controller's desired API encryption
// settings in the tenant's kubeadm ClusterConfiguration.
func (a *Activities) MergeEtcdEncryptionKubeadmConfig(
	ctx context.Context,
	input KubeadmConfigClusterInput,
) (MergeEtcdEncryptionKubeadmConfigResult, error) {
	tenantClient, err := a.tenantClientForClusterID(ctx, input.ClusterID)
	if err != nil {
		return MergeEtcdEncryptionKubeadmConfigResult{}, err
	}

	changed, err := kubeadmconfig.EnsureEncryptionInClusterConfiguration(ctx, tenantClient)
	if err != nil {
		return MergeEtcdEncryptionKubeadmConfigResult{}, err
	}
	return MergeEtcdEncryptionKubeadmConfigResult{Changed: changed}, nil
}

type UpdateKubeadmKubernetesVersionInput struct {
	ClusterID         uuid.UUID
	KubernetesVersion string
}

type UpdateKubeadmKubernetesVersionResult struct {
	Changed bool
}

// UpdateKubeadmKubernetesVersion persists the desired Kubernetes version in
// the tenant's kubeadm ClusterConfiguration.
func (a *Activities) UpdateKubeadmKubernetesVersion(
	ctx context.Context,
	input UpdateKubeadmKubernetesVersionInput,
) (UpdateKubeadmKubernetesVersionResult, error) {
	tenantClient, err := a.tenantClientForClusterID(ctx, input.ClusterID)
	if err != nil {
		return UpdateKubeadmKubernetesVersionResult{}, err
	}

	changed, err := kubeadmconfig.EnsureKubernetesVersionInClusterConfiguration(
		ctx,
		tenantClient,
		input.KubernetesVersion,
	)
	if err != nil {
		return UpdateKubeadmKubernetesVersionResult{}, err
	}
	return UpdateKubeadmKubernetesVersionResult{Changed: changed}, nil
}

func (a *Activities) tenantClientForClusterID(ctx context.Context, clusterID uuid.UUID) (client.Client, error) {
	cluster, err := a.db.GetCluster(ctx, clusterID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, temporal.NewNonRetryableApplicationError("cluster not found in database", "db.ErrNotFound", err)
		}
		return nil, fmt.Errorf("failed to get cluster %s: %w", clusterID, err)
	}
	return a.tenantClientForCluster(ctx, cluster)
}
