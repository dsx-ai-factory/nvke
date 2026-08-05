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

	forgesdk "gitlab-master.nvidia.com/nvmetal/cloud-api-sdk-go"
)

// GetInstanceTypes will retrieve the instance types for the given site. If a healthy cache exists, it will
// be used; otherwise, an error is returned.
func (p *ForgeProvider) GetInstanceTypes(ctx context.Context, orgID, siteID string) ([]*forgesdk.InstanceType, error) {
	cache := p.cacheEngine.GetSiteInstanceTypesCache(orgID, siteID)

	if cache.IsExpired() {
		lastError := cache.GetLastError()
		if lastError == nil {
			return nil, fmt.Errorf("cache expired")
		}

		return nil, fmt.Errorf("cache expired: %w", lastError)
	}

	return cache.Items(), nil
}

// GetInstanceTypeByID retrieves a specific instance type by its ID from the cache (if it exists).
func (p *ForgeProvider) GetInstanceTypeByID(ctx context.Context, orgID, siteID, instanceTypeID string) (*forgesdk.InstanceType, error) {
	cache := p.cacheEngine.GetSiteInstanceTypesCache(orgID, siteID)

	if cache.IsExpired() {
		lastError := cache.GetLastError()
		if lastError == nil {
			return nil, fmt.Errorf("cache expired")
		}

		return nil, fmt.Errorf("cache expired: %w", lastError)
	}

	instanceType, found := cache.Get(instanceTypeID)
	if !found {
		return nil, fmt.Errorf("instance type %s not found in cache", instanceTypeID)
	}

	return instanceType, nil
}
