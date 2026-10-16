// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
)

// TenantAPIAccessMethod reaches the tenant API through the Secret
// TenantAccess publishes when tenantAccessEnabled, and otherwise falls back
// to a joined control-plane node's node agent — the disabled-state rollout
// path.
func (*Client) TenantAPIAccessMethod(tenantAccessEnabled bool) shared.TenantAPIAccessMethod {
	if tenantAccessEnabled {
		return shared.TenantAPIAccessSecret
	}
	return shared.TenantAPIAccessNodeAgent
}
