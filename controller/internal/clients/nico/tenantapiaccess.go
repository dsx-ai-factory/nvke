// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// TenantAPIAccessMethod requires cluster's Nico payload, then reaches the
// tenant API through the Secret TenantAccess publishes when
// tenantAccessEnabled, and otherwise falls back to a joined control-plane
// node's node agent — the disabled-state rollout path.
func (*Client) TenantAPIAccessMethod(cluster db.Cluster, tenantAccessEnabled bool) (shared.TenantAPIAccessMethod, error) {
	if _, err := cluster.NicoProvider(); err != nil {
		return "", err
	}
	if tenantAccessEnabled {
		return shared.TenantAPIAccessSecret, nil
	}
	return shared.TenantAPIAccessNodeAgent, nil
}
