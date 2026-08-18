// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"context"
	"fmt"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

	"github.com/NVIDIA/nke/controller/internal/providers/forge/api"
)

// GetSubnets will retrieve the subnets for a given site. If a healthy cache exists, it will
// be used; otherwise, an error is returned.
func (p *ForgeProvider) GetSubnets(ctx context.Context, orgID, siteID string) ([]*forgesdk.Subnet, error) {
	cache := p.cacheEngine.GetSiteSubnetsCache(orgID, siteID)

	if cache.IsExpired() {
		lastError := cache.GetLastError()
		if lastError == nil {
			return nil, fmt.Errorf("cache expired")
		}

		return nil, fmt.Errorf("cache expired: %w", lastError)
	}

	return cache.Items(), nil
}

// GetSubnetByID retrieves a specific subnet by its ID from the cache (if it exists).
func (p *ForgeProvider) GetSubnetByID(ctx context.Context, orgID, siteID, subnetID string) (*forgesdk.Subnet, error) {
	cache := p.cacheEngine.GetSiteSubnetsCache(orgID, siteID)

	if cache.IsExpired() {
		lastError := cache.GetLastError()
		if lastError == nil {
			return nil, fmt.Errorf("cache expired")
		}

		return nil, fmt.Errorf("cache expired: %w", lastError)
	}

	subnet, found := cache.Get(subnetID)
	if !found {
		return nil, fmt.Errorf("subnet %s not found in cache", subnetID)
	}

	return subnet, nil
}

// GetSubnetByIDFromAPI retrieves a specific subnet directly from Forge, bypassing the cache.
func (p *ForgeProvider) GetSubnetByIDFromAPI(ctx context.Context, orgID, siteID, subnetID string) (*forgesdk.Subnet, error) {
	apiClient, ctx, err := p.cacheEngine.ClientAndContext(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("failed to get Forge API client: %w", err)
	}

	subnets, err := api.GetSubnets(ctx, apiClient, orgID, siteID)
	if err != nil {
		return nil, fmt.Errorf("failed to list Forge subnets: %w", err)
	}

	for _, subnet := range subnets {
		if subnet.GetId() == subnetID {
			return subnet, nil
		}
	}

	return nil, fmt.Errorf("subnet %s not found in Forge", subnetID)
}
