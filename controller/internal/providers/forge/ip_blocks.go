// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"context"
	"fmt"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"
)

// GetIPBlocks will retrieve the IP blocks for a given site. If a healthy cache exists, it will
// be used; otherwise, an error is returned.
func (p *ForgeProvider) GetIPBlocks(ctx context.Context, orgID, siteID string) ([]*forgesdk.IpBlock, error) {
	cache := p.cacheEngine.GetSiteIPBlocksCache(orgID, siteID)

	if cache.IsExpired() {
		lastError := cache.GetLastError()
		if lastError == nil {
			return nil, fmt.Errorf("cache expired")
		}

		return nil, fmt.Errorf("cache expired: %w", lastError)
	}

	return cache.Items(), nil
}
