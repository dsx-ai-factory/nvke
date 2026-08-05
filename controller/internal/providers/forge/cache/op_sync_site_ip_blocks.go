// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package cache

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/providers/forge/api"
	forgesdk "gitlab-master.nvidia.com/nvmetal/cloud-api-sdk-go"
)

type OpSyncSiteIPBlocks struct {
	*BaseOperation

	// cacheEngine is a reference to the cache engine this operation belongs to
	cacheEngine *ForgeCacheEngine

	// tenant is the tenant associated with the operation
	tenant *forgesdk.Tenant

	// site is the site associated with the operation
	site *forgesdk.Site
}

func NewOpSyncSiteIPBlocks(cacheEngine *ForgeCacheEngine, tenant *forgesdk.Tenant, site *forgesdk.Site) *OpSyncSiteIPBlocks {
	logger := cacheEngine.logger.
		With("operation", "SyncSiteIPBlocks").
		With("orgId", tenant.GetOrg()).
		With("siteId", site.GetId())

	op := OpSyncSiteIPBlocks{
		BaseOperation: NewBaseOperation(logger),
		cacheEngine:   cacheEngine,
		tenant:        tenant,
		site:          site,
	}

	cacheTTL := cacheEngine.GetSiteIPBlocksCache(tenant.GetOrg(), site.GetId()).GetTTL()
	op.BaseOperation.retryPolicy.MaxInterval = cacheTTL
	op.BaseOperation.executionInterval = cacheRefreshInterval(cacheTTL)

	return &op
}

func (op *OpSyncSiteIPBlocks) IsSiteRegistered(ctx context.Context) (bool, error) {
	siteID, err := uuid.Parse(op.site.GetId())
	if err != nil {
		op.logger.Error("Failed to parse site ID as UUID")
		return false, err
	}

	_, err = op.cacheEngine.db.GetSite(ctx, siteID, false)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return false, nil
		}

		op.logger.Error("Failed to check if site is registered")
		return false, err
	}

	return true, nil
}

func (op *OpSyncSiteIPBlocks) Execute(ctx context.Context) {
	var err error
	defer func() { op.onExecutionFinished(err) }()

	siteIsRegistered, err := op.IsSiteRegistered(ctx)
	if !siteIsRegistered || err != nil {
		return
	}

	op.logger.Debug("Executing operation")
	cache := op.cacheEngine.GetSiteIPBlocksCache(op.tenant.GetOrg(), op.site.GetId())

	var apiClient *forgesdk.APIClient
	apiClient, ctx, err = op.cacheEngine.ClientAndContext(ctx, op.tenant.GetOrg())
	if err != nil {
		cache.SetLastError(fmt.Errorf("failed to get API client: %w", err))
		return
	}

	ipBlocks, err := api.GetIPBlocks(ctx, apiClient, op.tenant.GetOrg(), op.site.GetId())
	if err != nil {
		cache.SetLastError(fmt.Errorf("failed to get ip blocks: %w", err))
		return
	}

	cache.Hydrate(ipBlocks)
	op.logger.Debug("Hydrated cache", "numIPBlocks", cache.Count())
}
