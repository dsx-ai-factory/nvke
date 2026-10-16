// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"fmt"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// TenantAPIAccessMethod requires cluster to define infra.cluster, then
// always reaches the tenant API through the Secret TenantAccess publishes.
// CAPI has no node-agent fallback, so tenantAccessEnabled does not apply.
func (*Client) TenantAPIAccessMethod(cluster db.Cluster, _ bool) (shared.TenantAPIAccessMethod, error) {
	provider, err := cluster.CAPIProvider()
	if err != nil {
		return "", err
	}
	if isEmptyManifestBytes(provider.InfraCluster) {
		return "", fmt.Errorf("cluster %s must define infra.cluster", cluster.ID)
	}
	return shared.TenantAPIAccessSecret, nil
}
