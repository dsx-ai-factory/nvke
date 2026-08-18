// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"context"
	"fmt"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

	"github.com/NVIDIA/nke/controller/internal/providers/forge/api"
)

// GetVpcPrefixes will retrieve the VPC prefixes for a given site. If a healthy cache exists, it will
// be used; otherwise, an error is returned.
func (p *ForgeProvider) GetVpcPrefixes(ctx context.Context, orgID, siteID string) ([]*forgesdk.VpcPrefix, error) {
	cache := p.cacheEngine.GetSiteVpcPrefixesCache(orgID, siteID)

	if cache.IsExpired() {
		lastError := cache.GetLastError()
		if lastError == nil {
			return nil, fmt.Errorf("cache expired")
		}

		return nil, fmt.Errorf("cache expired: %w", lastError)
	}

	return cache.Items(), nil
}

// GetVpcPrefixByID retrieves a specific VPC prefix by its ID from the cache (if it exists).
func (p *ForgeProvider) GetVpcPrefixByID(ctx context.Context, orgID, siteID, vpcPrefixID string) (*forgesdk.VpcPrefix, error) {
	cache := p.cacheEngine.GetSiteVpcPrefixesCache(orgID, siteID)

	if cache.IsExpired() {
		lastError := cache.GetLastError()
		if lastError == nil {
			return nil, fmt.Errorf("cache expired")
		}

		return nil, fmt.Errorf("cache expired: %w", lastError)
	}

	vpcPrefix, found := cache.Get(vpcPrefixID)
	if !found {
		return nil, fmt.Errorf("vpc prefix %s not found in cache", vpcPrefixID)
	}

	return vpcPrefix, nil
}

// GetVpcPrefixByIDFromAPI retrieves a specific VPC prefix directly from Forge, bypassing the cache.
func (p *ForgeProvider) GetVpcPrefixByIDFromAPI(ctx context.Context, orgID, siteID, vpcPrefixID string) (*forgesdk.VpcPrefix, error) {
	apiClient, ctx, err := p.cacheEngine.ClientAndContext(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("failed to get Forge API client: %w", err)
	}

	vpcPrefixes, err := api.GetVpcPrefixes(ctx, apiClient, orgID, siteID)
	if err != nil {
		return nil, fmt.Errorf("failed to list Forge VPC prefixes: %w", err)
	}

	for _, vpcPrefix := range vpcPrefixes {
		if vpcPrefix.GetId() == vpcPrefixID {
			return vpcPrefix, nil
		}
	}

	return nil, fmt.Errorf("VPC prefix %s not found in Forge", vpcPrefixID)
}
