// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/providers/forge/api"
)

type OpSyncSiteInfiniBandPartitions struct {
	*BaseOperation

	// cacheEngine is a reference to the cache engine this operation belongs to
	cacheEngine *ForgeCacheEngine

	// tenant is the tenant associated with the operation
	tenant *forgesdk.Tenant

	// site is the site associated with the operation
	site *forgesdk.Site
}

func NewOpSyncSiteInfiniBandPartitions(cacheEngine *ForgeCacheEngine, tenant *forgesdk.Tenant, site *forgesdk.Site) *OpSyncSiteInfiniBandPartitions {
	logger := cacheEngine.logger.
		With("operation", "SyncSiteInfiniBandPartitions").
		With("orgId", tenant.GetOrg()).
		With("siteId", site.GetId())

	op := OpSyncSiteInfiniBandPartitions{
		BaseOperation: NewBaseOperation(logger),
		cacheEngine:   cacheEngine,
		tenant:        tenant,
		site:          site,
	}

	cacheTTL := cacheEngine.GetSiteInfiniBandPartitionsCache(tenant.GetOrg(), site.GetId()).GetTTL()
	op.BaseOperation.retryPolicy.MaxInterval = cacheTTL
	op.BaseOperation.executionInterval = cacheRefreshInterval(cacheTTL)

	return &op
}

func (op *OpSyncSiteInfiniBandPartitions) IsSiteRegistered(ctx context.Context) (bool, error) {
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

func (op *OpSyncSiteInfiniBandPartitions) Execute(ctx context.Context) {
	var err error
	defer func() { op.onExecutionFinished(err) }()

	siteIsRegistered, err := op.IsSiteRegistered(ctx)
	if !siteIsRegistered || err != nil {
		return
	}

	op.logger.Debug("Executing operation")
	cache := op.cacheEngine.GetSiteInfiniBandPartitionsCache(op.tenant.GetOrg(), op.site.GetId())

	var apiClient *forgesdk.APIClient
	apiClient, ctx, err = op.cacheEngine.ClientAndContext(ctx, op.tenant.GetOrg())
	if err != nil {
		cache.SetLastError(fmt.Errorf("failed to get API client: %w", err))
		return
	}

	infiniBandPartitions, err := api.GetInfiniBandPartitions(ctx, apiClient, op.tenant.GetOrg(), op.site.GetId())
	if err != nil {
		cache.SetLastError(fmt.Errorf("failed to get infiniband partitions: %w", err))
		return
	}

	cache.Hydrate(infiniBandPartitions)
	op.logger.Debug("Hydrated cache", "numInfiniBandPartitions", cache.Count())
}
