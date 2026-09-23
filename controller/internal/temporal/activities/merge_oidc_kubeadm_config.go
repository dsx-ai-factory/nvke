// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"

	"github.com/google/uuid"

	"github.com/dsx-ai-factory/nke/controller/internal/kubeadmconfig"
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
// settings is a no-op (Changed=false). The injected provider supplies the
// long-lived shared tenant client.
func (a *Activities) MergeOIDCKubeadmConfig(ctx context.Context, input MergeOIDCKubeadmConfigInput) (MergeOIDCKubeadmConfigResult, error) {
	tenantClient, err := a.tenantClientForClusterID(ctx, input.ClusterID)
	if err != nil {
		return MergeOIDCKubeadmConfigResult{}, err
	}

	changed, err := kubeadmconfig.EnsureOIDCInClusterConfiguration(ctx, tenantClient)
	if err != nil {
		return MergeOIDCKubeadmConfigResult{}, err
	}
	return MergeOIDCKubeadmConfigResult{Changed: changed}, nil
}
