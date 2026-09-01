// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"context"
	"fmt"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

	"github.com/NVIDIA/nke/controller/internal/providers/forge/api"
)

type OpSyncSiteSubnets struct {
	*BaseOperation

	// cacheEngine is a reference to the cache engine this operation belongs to
	cacheEngine *ForgeCacheEngine

	// tenant is the tenant associated with the operation
	tenant *forgesdk.Tenant

	// site is the site associated with the operation
	site *forgesdk.Site
}

func NewOpSyncSiteSubnets(cacheEngine *ForgeCacheEngine, tenant *forgesdk.Tenant, site *forgesdk.Site) *OpSyncSiteSubnets {
	logger := cacheEngine.logger.
		With("operation", "SyncSiteSubnets").
		With("orgId", tenant.GetOrg()).
		With("siteId", site.GetId())

	op := OpSyncSiteSubnets{
		BaseOperation: NewBaseOperation(logger),
		cacheEngine:   cacheEngine,
		tenant:        tenant,
		site:          site,
	}

	cacheTTL := cacheEngine.GetSiteSubnetsCache(tenant.GetOrg(), site.GetId()).GetTTL()
	op.BaseOperation.retryPolicy.MaxInterval = cacheTTL
	op.BaseOperation.executionInterval = cacheRefreshInterval(cacheTTL)

	return &op
}

func (op *OpSyncSiteSubnets) Execute(ctx context.Context) {
	var err error
	defer func() { op.onExecutionFinished(err) }()

	op.logger.Debug("Executing operation")
	cache := op.cacheEngine.GetSiteSubnetsCache(op.tenant.GetOrg(), op.site.GetId())

	var apiClient *forgesdk.APIClient
	apiClient, ctx, err = op.cacheEngine.ClientAndContext(ctx, op.tenant.GetOrg())
	if err != nil {
		cache.SetLastError(fmt.Errorf("failed to get API client: %w", err))
		return
	}

	subnets, err := api.GetSubnets(ctx, apiClient, op.tenant.GetOrg(), op.site.GetId())
	if err != nil {
		cache.SetLastError(fmt.Errorf("failed to get subnets: %w", err))
		return
	}

	cache.Hydrate(subnets)
	op.logger.Debug("Hydrated cache", "numSubnets", cache.Count())
}
