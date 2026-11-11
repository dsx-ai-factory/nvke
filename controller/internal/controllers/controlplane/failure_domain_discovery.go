// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"fmt"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/failuredomains"
)

// readFailureDomains discovers the cluster's control-plane failure domains. A
// discovery error is returned with a nil snapshot.
func (r *reconciler) readFailureDomains(ctx context.Context, cluster db.Cluster) (*failuredomains.Snapshot, error) {
	if r.k8sClient == nil {
		return nil, fmt.Errorf("kubernetes client is not configured")
	}
	return failuredomains.Discover(ctx, r.k8sClient, r.cfg.ManagementNamespaceOrDefault(), cluster.Name)
}
