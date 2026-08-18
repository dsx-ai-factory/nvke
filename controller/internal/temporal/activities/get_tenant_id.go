// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"fmt"

	"github.com/google/uuid"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/providers/forge/api"
)

// Util method, not a registered activity. Gets the tenant ID for a given org ID.
func (a *Activities) getTenantID(orgID string) (uuid.UUID, error) {
	for _, tenant := range api.GetTenants(a.cfg.ForgeEnvironment == config.ForgeEnvironmentTest) {
		if tenant.GetOrg() == orgID {
			tenantID, err := uuid.Parse(tenant.GetId())
			if err != nil {
				return uuid.Nil, fmt.Errorf("failed to parse Forge tenant ID: %w", err)
			}

			return tenantID, nil
		}
	}

	return uuid.Nil, fmt.Errorf("failed to find tenant by org id: %s", orgID)
}
