// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

// TenantClientProvider returns Kubernetes clients for controller-managed clusters.
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
