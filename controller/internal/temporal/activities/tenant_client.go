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

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/nke/controller/internal/db"
)

// TenantClientProvider returns Kubernetes clients for CWE-managed clusters.
type TenantClientProvider interface {
	ClientForDBCluster(ctx context.Context, cluster db.Cluster) (client.Client, error)
}

func (a *Activities) tenantClientForCluster(ctx context.Context, cluster db.Cluster) (client.Client, error) {
	tenantClient, err := a.tenantClientProvider.ClientForDBCluster(ctx, cluster)
	if err != nil {
		return nil, fmt.Errorf("failed to build tenant client for cluster %s: %w", cluster.ID, err)
	}

	return tenantClient, nil
}
