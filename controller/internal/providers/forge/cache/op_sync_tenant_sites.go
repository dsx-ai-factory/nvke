// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"context"
	"errors"
	"fmt"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

	"github.com/NVIDIA/nke/controller/internal/providers/forge/api"
)

type OpSyncTenantSites struct {
	*BaseOperation

	// cacheEngine is a reference to the cache engine this operation belongs to
	cacheEngine *ForgeCacheEngine

	// tenant is the tenant associated with the operation
	tenant *forgesdk.Tenant
}

func NewOpSyncTenantSites(cacheEngine *ForgeCacheEngine, tenant *forgesdk.Tenant) *OpSyncTenantSites {
	logger := cacheEngine.logger.With("operation", "SyncTenantSites").With("tenantId", tenant.GetId())

	op := OpSyncTenantSites{
		BaseOperation: NewBaseOperation(logger),
		cacheEngine:   cacheEngine,
		tenant:        tenant,
	}

	cacheTTL := cacheEngine.GetTenantSitesCache(tenant.GetId()).GetTTL()
	op.BaseOperation.retryPolicy.MaxInterval = cacheTTL
	op.BaseOperation.executionInterval = cacheRefreshInterval(cacheTTL)

	return &op
}

func (op *OpSyncTenantSites) Execute(ctx context.Context) {
	var err error
	defer func() { op.onExecutionFinished(err) }()

	op.logger.Debug("Executing operation")
	cache := op.cacheEngine.GetTenantSitesCache(op.tenant.GetId())

	var apiClient *forgesdk.APIClient
	apiClient, ctx, err = op.cacheEngine.ClientAndContext(ctx, op.tenant.GetOrg())
	if err != nil {
		cache.SetLastError(fmt.Errorf("failed to get API client: %w", err))
		return
	}

	sites, err := api.GetSites(ctx, apiClient, op.tenant.GetOrg(), op.tenant.GetId())
	if err != nil {
		// A 403 means the configured API key has no access to this tenant. This is expected
		// when a single key only covers a subset of Forge tenants, so treat it as a benign
		// no-op rather than an Error that logs and retries aggressively every refresh cycle
		// Clearing err lets onExecutionFinished reschedule at the normal cadence.
		if errors.Is(err, api.ErrTenantNotAuthorized) {
			op.logger.Debug("Skipping tenant sync; API key not authorized for tenant", "error", err)
			cache.SetLastError(err)
			op.cacheEngine.RecordTenantSyncAuth(op.tenant.GetId(), false)
			err = nil

			return
		}

		cache.SetLastError(fmt.Errorf("failed to get sites: %w", err))

		return
	}

	op.cacheEngine.RecordTenantSyncAuth(op.tenant.GetId(), true)

	cache.Hydrate(sites)
	op.logger.Debug("Hydrated cache", "numSites", cache.Count())

	// now that we have the latest site data, we need to make sure we setup our regular cadence of
	// sync operations for things like instances, instance types, etc (all of the site-specific
	// resources we want to cache)
	var label string

	for _, site := range sites {
		label = fmt.Sprintf("sync-site-instance-types-%s-%s", op.tenant.GetOrg(), site.GetId())
		op.logger.Debug("Adding site instance types sync operation", "orgId", op.tenant.GetOrg(), "siteId", site.GetId())
		op.cacheEngine.AddOperation(label, NewOpSyncSiteInstanceTypes(op.cacheEngine, op.tenant, site))

		label = fmt.Sprintf("sync-site-instances-%s-%s", op.tenant.GetOrg(), site.GetId())
		op.logger.Debug("Adding site instances sync operation", "orgId", op.tenant.GetOrg(), "siteId", site.GetId())
		op.cacheEngine.AddOperation(label, NewOpSyncSiteInstances(op.cacheEngine, op.tenant, site))

		label = fmt.Sprintf("sync-site-subnets-%s-%s", op.tenant.GetOrg(), site.GetId())
		op.logger.Debug("Adding site subnets sync operation", "orgId", op.tenant.GetOrg(), "siteId", site.GetId())
		op.cacheEngine.AddOperation(label, NewOpSyncSiteSubnets(op.cacheEngine, op.tenant, site))

		label = fmt.Sprintf("sync-site-vpc-prefixes-%s-%s", op.tenant.GetOrg(), site.GetId())
		op.logger.Debug("Adding site VPC prefixes sync operation", "orgId", op.tenant.GetOrg(), "siteId", site.GetId())
		op.cacheEngine.AddOperation(label, NewOpSyncSiteVpcPrefixes(op.cacheEngine, op.tenant, site))

		label = fmt.Sprintf("sync-site-vpcs-%s-%s", op.tenant.GetOrg(), site.GetId())
		op.logger.Debug("Adding site vpcs sync operation", "orgId", op.tenant.GetOrg(), "siteId", site.GetId())
		op.cacheEngine.AddOperation(label, NewOpSyncSiteVPCs(op.cacheEngine, op.tenant, site))

		label = fmt.Sprintf("sync-site-ssh-key-groups-%s-%s", op.tenant.GetOrg(), site.GetId())
		op.logger.Debug("Adding site ssh key groups sync operation", "orgId", op.tenant.GetOrg(), "siteId", site.GetId())
		op.cacheEngine.AddOperation(label, NewOpSyncSiteSSHKeyGroups(op.cacheEngine, op.tenant, site))

		label = fmt.Sprintf("sync-site-ip-blocks-%s-%s", op.tenant.GetOrg(), site.GetId())
		op.logger.Debug("Adding site ip blocks sync operation", "orgId", op.tenant.GetOrg(), "siteId", site.GetId())
		op.cacheEngine.AddOperation(label, NewOpSyncSiteIPBlocks(op.cacheEngine, op.tenant, site))

		label = fmt.Sprintf("sync-site-infiniband-partitions-%s-%s", op.tenant.GetOrg(), site.GetId())
		op.logger.Debug("Adding site infiniband partitions sync operation", "orgId", op.tenant.GetOrg(), "siteId", site.GetId())
		op.cacheEngine.AddOperation(label, NewOpSyncSiteInfiniBandPartitions(op.cacheEngine, op.tenant, site))

		label = fmt.Sprintf("sync-site-nvlink-logical-partitions-%s-%s", op.tenant.GetOrg(), site.GetId())
		op.logger.Debug("Adding site NVLink logical partitions sync operation", "orgId", op.tenant.GetOrg(), "siteId", site.GetId())
		op.cacheEngine.AddOperation(label, NewOpSyncSiteNVLinkLogicalPartitions(op.cacheEngine, op.tenant, site))
	}
}
