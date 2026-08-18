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

type OpSyncSiteVPCs struct {
	*BaseOperation

	// cacheEngine is a reference to the cache engine this operation belongs to
	cacheEngine *ForgeCacheEngine

	// tenant is the tenant associated with the operation
	tenant *forgesdk.Tenant

	// site is the site associated with the operation
	site *forgesdk.Site
}

func NewOpSyncSiteVPCs(cacheEngine *ForgeCacheEngine, tenant *forgesdk.Tenant, site *forgesdk.Site) *OpSyncSiteVPCs {
	logger := cacheEngine.logger.
		With("operation", "SyncSiteVPCs").
		With("orgId", tenant.GetOrg()).
		With("siteId", site.GetId())

	op := OpSyncSiteVPCs{
		BaseOperation: NewBaseOperation(logger),
		cacheEngine:   cacheEngine,
		tenant:        tenant,
		site:          site,
	}

	cacheTTL := cacheEngine.GetSiteVPCsCache(tenant.GetOrg(), site.GetId()).GetTTL()
	op.BaseOperation.retryPolicy.MaxInterval = cacheTTL
	op.BaseOperation.executionInterval = cacheRefreshInterval(cacheTTL)

	return &op
}

func (op *OpSyncSiteVPCs) IsSiteRegistered(ctx context.Context) (bool, error) {
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

func (op *OpSyncSiteVPCs) Execute(ctx context.Context) {
	var err error
	defer func() { op.onExecutionFinished(err) }()

	siteIsRegistered, err := op.IsSiteRegistered(ctx)
	if !siteIsRegistered || err != nil {
		return
	}

	op.logger.Debug("Executing operation")
	cache := op.cacheEngine.GetSiteVPCsCache(op.tenant.GetOrg(), op.site.GetId())

	var apiClient *forgesdk.APIClient
	apiClient, ctx, err = op.cacheEngine.ClientAndContext(ctx, op.tenant.GetOrg())
	if err != nil {
		cache.SetLastError(fmt.Errorf("failed to get API client: %w", err))
		return
	}

	vpcs, err := api.GetVPCs(ctx, apiClient, op.tenant.GetOrg(), op.site.GetId())
	if err != nil {
		cache.SetLastError(fmt.Errorf("failed to get vpcs: %w", err))
		return
	}

	cache.Hydrate(vpcs)
	op.logger.Debug("Hydrated cache", "numVpcs", cache.Count())
}
