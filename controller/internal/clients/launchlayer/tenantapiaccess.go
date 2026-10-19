// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package launchlayer

import (
	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
)

// TenantAPIAccessMethod always reaches the tenant API through the Secret
// TenantAccess publishes. LaunchLayer has no node-agent fallback, so
// tenantAccessEnabled does not apply.
func (*Client) TenantAPIAccessMethod(bool) shared.TenantAPIAccessMethod {
	return shared.TenantAPIAccessSecret
}
