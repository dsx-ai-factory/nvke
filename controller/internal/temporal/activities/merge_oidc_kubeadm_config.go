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
	"errors"
	"fmt"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"

	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/kubeadmconfig"
)

// MergeOIDCKubeadmConfigInput identifies the cluster whose persisted
// kube-system/kubeadm-config ClusterConfiguration is augmented with the OIDC
// apiserver settings (the authentication-config extraArg and the oidc-auth-config
// extraVolume), so a later kubeadm upgrade re-render does not strip them.
type MergeOIDCKubeadmConfigInput struct {
	ClusterID uuid.UUID
}

// MergeOIDCKubeadmConfigResult reports whether the ConfigMap was updated. Changed
// is false when the settings were already present (the activity skipped the write).
type MergeOIDCKubeadmConfigResult struct {
	Changed bool
}

// MergeOIDCKubeadmConfig reads the tenant kubeadm-config, merges the OIDC
// apiserver settings into its ClusterConfiguration, and writes it back only when
// the merge changed something. It is idempotent: a cluster already carrying the
// settings is a no-op (Changed=false). The tenant client is built from the
// admin.conf fetched from a control-plane node-agent (GetKubeconfig), which is
// the reliable source; the cluster row's own Kubeconfig field is often empty.
func (a *Activities) MergeOIDCKubeadmConfig(ctx context.Context, input MergeOIDCKubeadmConfigInput) (MergeOIDCKubeadmConfigResult, error) {
	cluster, err := a.db.GetCluster(ctx, input.ClusterID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return MergeOIDCKubeadmConfigResult{}, temporal.NewNonRetryableApplicationError("cluster not found in database", "db.ErrNotFound", err)
		}
		return MergeOIDCKubeadmConfigResult{}, fmt.Errorf("failed to get cluster %s: %w", input.ClusterID, err)
	}

	tenantClient, err := a.tenantClientForCluster(ctx, cluster)
	if err != nil {
		return MergeOIDCKubeadmConfigResult{}, err
	}

	changed, err := kubeadmconfig.EnsureOIDCInClusterConfiguration(ctx, tenantClient)
	if err != nil {
		return MergeOIDCKubeadmConfigResult{}, err
	}
	return MergeOIDCKubeadmConfigResult{Changed: changed}, nil
}
