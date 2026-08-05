// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package forge

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/NVIDIA/nke/controller/api"
	"github.com/NVIDIA/nke/controller/internal/config"
	forgeapi "github.com/NVIDIA/nke/controller/internal/providers/forge/api"
	forgesdk "gitlab-master.nvidia.com/nvmetal/cloud-api-sdk-go"
)

// GetSites will retrieve the sites for a given org. If a healthy cache exists, it will
// be used; otherwise, an error is returned.
func (p *ForgeProvider) GetSites(ctx context.Context, orgID string) ([]*forgesdk.Site, error) {
	tenant, err := findTenantByOrgID(orgID, p.cfg.ForgeEnvironment == config.ForgeEnvironmentTest)
	if err != nil {
		return nil, fmt.Errorf("failed to find tenant for orgID %s: %w", orgID, err)
	}

	tenantID := tenant.ID.String()
	cache := p.cacheEngine.GetTenantSitesCache(tenantID)

	if cache.IsExpired() {
		lastError := cache.GetLastError()
		if lastError == nil {
			return nil, fmt.Errorf("cache expired")
		}

		return nil, fmt.Errorf("cache expired: %w", lastError)
	}

	return cache.Items(), nil
}

// GetSiteByID will retrieve a specific site by its ID for a given org. If a healthy cache exists, it will
// be used; otherwise, an error is returned.
func (p *ForgeProvider) GetSiteByID(ctx context.Context, orgID, siteID string) (*forgesdk.Site, error) {
	sites, err := p.GetSites(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("failed to get sites: %w", err)
	}

	for _, site := range sites {
		if site.GetId() == siteID {
			return site, nil
		}
	}

	return nil, fmt.Errorf("site %s not found", siteID)
}

func getTenants(dockerTest bool) []api.ForgeTenant {
	tenants := forgeapi.GetTenants(dockerTest)
	response := make([]api.ForgeTenant, 0, len(tenants))

	for _, tenant := range tenants {
		response = append(response, api.ForgeTenant{
			ID:    uuid.MustParse(tenant.GetId()),
			OrgID: tenant.GetOrg(),
			Name:  tenant.GetOrgDisplayName(),
		})
	}

	return response
}

func findTenantByOrgID(orgID string, dockerTest bool) (*api.ForgeTenant, error) {
	for _, tenant := range getTenants(dockerTest) {
		if tenant.OrgID == orgID {
			return &tenant, nil
		}
	}
	return nil, fmt.Errorf("tenant not found for orgID: %s", orgID)
}
