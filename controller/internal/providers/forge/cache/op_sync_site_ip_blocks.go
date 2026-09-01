// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"context"
	"fmt"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

	"github.com/NVIDIA/nke/controller/internal/providers/forge/api"
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

func (op *OpSyncSiteIPBlocks) Execute(ctx context.Context) {
	var err error
	defer func() { op.onExecutionFinished(err) }()

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
