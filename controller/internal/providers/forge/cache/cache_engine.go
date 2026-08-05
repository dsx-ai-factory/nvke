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
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/providers/forge/api"
	forgesdksupport "github.com/NVIDIA/nke/controller/internal/providers/forge/sdk"
	forgesdk "gitlab-master.nvidia.com/nvmetal/cloud-api-sdk-go"
)

const (
	opPrefixSyncTenantSites = "sync-tenant-sites-"
)

// Operation represents an independent task that can be executed concurrently and should not have
// dependencies on any other Operation.
type Operation interface {
	GetNextExecutionTime() time.Time
	Execute(ctx context.Context)
}

type ForgeCacheEngine struct {
	cfg       config.Config
	sdkCfg    *forgesdk.Configuration
	serverIdx int
	db        db.DB
	logger    *slog.Logger
	stopCh    chan struct{}

	// ops holds a record of all operations that should be performed by the cache
	// engine on a regular cadence
	ops map[string]Operation

	// opsMu is a mutex to protect access to the ops map
	opsMu sync.RWMutex

	// bearerTokens holds all cached bearer tokens
	bearerTokens map[string]*api.BearerToken

	// bearerTokenMu is a mutex to protect access to the bearerTokens map
	bearerTokenMu sync.RWMutex

	// tenants holds the current state of all cached tenant resources
	tenants *ResourceCache[forgesdk.Tenant, *forgesdk.Tenant]

	// tenantSyncAuth records, per tenant id, whether the most recent tenant-sites sync was
	// authorized (true) or rejected with a 403 (false). It is used to surface a warning when
	// the configured API key can access no tenants at all (see NKX-13835).
	tenantSyncAuth map[string]bool

	// tenantSyncAuthMu protects tenantSyncAuth and tenantSyncAuthWarned
	tenantSyncAuthMu sync.Mutex

	// tenantSyncAuthWarned guards the "no accessible tenants" warning so it is only emitted
	// once until at least one tenant becomes accessible again
	tenantSyncAuthWarned bool

	// tenantSites is a map of all tenant's site resources where the key is the tenant id
	tenantSites map[string]*ResourceCache[forgesdk.Site, *forgesdk.Site]

	// tenantSitesMu is a mutex to protect access to the tenantSites map
	tenantSitesMu sync.RWMutex

	// siteInstances is a map of all site instance resources where the key is the site id
	siteInstances map[string]*ResourceCache[forgesdk.Instance, *forgesdk.Instance]

	// siteInstancesMu is a mutex to protect access to the siteInstances map
	siteInstancesMu sync.RWMutex

	// siteInstanceTypes is a map of all site instance type resources where the key is the site id
	siteInstanceTypes map[string]*ResourceCache[forgesdk.InstanceType, *forgesdk.InstanceType]

	// siteInstanceTypesMu is a mutex to protect access to the siteInstanceTypes map
	siteInstanceTypesMu sync.RWMutex

	// siteSubnets is a map of all site subnet resources where the key is the site id
	siteSubnets map[string]*ResourceCache[forgesdk.Subnet, *forgesdk.Subnet]

	// siteSubnetsMu is a mutex to protect access to the siteSubnets map
	siteSubnetsMu sync.RWMutex

	// siteVpcPrefixes is a map of all site VPC prefix resources where the key is the site id
	siteVpcPrefixes map[string]*ResourceCache[forgesdk.VpcPrefix, *forgesdk.VpcPrefix]

	// siteVpcPrefixesMu is a mutex to protect access to the siteVpcPrefixes map
	siteVpcPrefixesMu sync.RWMutex

	// siteVPCs is a map of all site VPC resources where the key is the site id
	siteVPCs map[string]*ResourceCache[forgesdk.VPC, *forgesdk.VPC]

	// siteVPCsMu is a mutex to protect access to the siteVPCs map
	siteVPCsMu sync.RWMutex

	// siteSSHKeyGroups is a map of all site SSH key group resources where the key is the site id
	siteSSHKeyGroups map[string]*ResourceCache[forgesdk.SshKeyGroup, *forgesdk.SshKeyGroup]

	// siteSSHKeyGroupsMu is a mutex to protect access to the siteSSHKeyGroups map
	siteSSHKeyGroupsMu sync.RWMutex

	// siteIPBlocks is a map of all site IP block resources where the key is the site id
	siteIPBlocks map[string]*ResourceCache[forgesdk.IpBlock, *forgesdk.IpBlock]

	// siteIPBlocksMu is a mutex to protect access to the siteIPBlocks map
	siteIPBlocksMu sync.RWMutex

	// siteInfiniBandPartitions is a map of all site InfiniBand partition resources where the key is the site id
	siteInfiniBandPartitions map[string]*ResourceCache[forgesdk.InfiniBandPartition, *forgesdk.InfiniBandPartition]

	// siteInfiniBandPartitionsMu is a mutex to protect access to the siteInfiniBandPartitions map
	siteInfiniBandPartitionsMu sync.RWMutex

	// siteNVLinkLogicalPartitions is a map of all site NVLink logical partition resources where the key is the site id
	siteNVLinkLogicalPartitions map[string]*ResourceCache[forgesdk.NVLinkLogicalPartition, *forgesdk.NVLinkLogicalPartition]

	// siteNVLinkLogicalPartitionsMu is a mutex to protect access to the siteNVLinkLogicalPartitions map
	siteNVLinkLogicalPartitionsMu sync.RWMutex
}

// NewForgeCacheEngine creates a new ForgeCacheEngine instance
func NewForgeCacheEngine(cfg config.Config, db db.DB, logger *slog.Logger) *ForgeCacheEngine {
	return &ForgeCacheEngine{
		cfg:       cfg,
		sdkCfg:    forgesdksupport.NewSDKConfig(&cfg),
		serverIdx: forgesdksupport.ContextServerIndex(&cfg),
		db:        db,
		logger:    logger.With("component", "cache-engine"),
		stopCh:    make(chan struct{}),
		ops:       make(map[string]Operation),

		// note: bearer tokens actually last 10 minutes but we're being extra safe here
		bearerTokens:             make(map[string]*api.BearerToken),
		tenants:                  NewResourceCache[forgesdk.Tenant](time.Hour * 24 * 365),
		tenantSyncAuth:           make(map[string]bool),
		tenantSites:              make(map[string]*ResourceCache[forgesdk.Site, *forgesdk.Site]),
		siteInstances:            make(map[string]*ResourceCache[forgesdk.Instance, *forgesdk.Instance]),
		siteInstanceTypes:        make(map[string]*ResourceCache[forgesdk.InstanceType, *forgesdk.InstanceType]),
		siteSubnets:              make(map[string]*ResourceCache[forgesdk.Subnet, *forgesdk.Subnet]),
		siteVpcPrefixes:          make(map[string]*ResourceCache[forgesdk.VpcPrefix, *forgesdk.VpcPrefix]),
		siteVPCs:                 make(map[string]*ResourceCache[forgesdk.VPC, *forgesdk.VPC]),
		siteSSHKeyGroups:         make(map[string]*ResourceCache[forgesdk.SshKeyGroup, *forgesdk.SshKeyGroup]),
		siteIPBlocks:             make(map[string]*ResourceCache[forgesdk.IpBlock, *forgesdk.IpBlock]),
		siteInfiniBandPartitions: make(map[string]*ResourceCache[forgesdk.InfiniBandPartition, *forgesdk.InfiniBandPartition]),
		siteNVLinkLogicalPartitions: make(
			map[string]*ResourceCache[forgesdk.NVLinkLogicalPartition, *forgesdk.NVLinkLogicalPartition],
		),
	}
}

// AddOperation adds a new operation to the cache engine
func (ce *ForgeCacheEngine) AddOperation(id string, op Operation) {
	ce.opsMu.Lock()
	defer ce.opsMu.Unlock()

	ce.logger.Debug("Adding operation", "id", id)
	ce.ops[id] = op
}

// Start performs some pre-warming for the cache and initializes the base case engine
func (ce *ForgeCacheEngine) Start(ctx context.Context) {
	ce.logger.Debug("Starting up")
	ce.GetTenantsCache().Hydrate(api.GetTenants(ce.cfg.ForgeEnvironment == config.ForgeEnvironmentTest))

	for _, tenant := range ce.GetTenantsCache().Items() {
		ce.AddOperation(opPrefixSyncTenantSites+tenant.GetId(), NewOpSyncTenantSites(ce, tenant))
	}

	go ce.processLoop(ctx)
}

// Stop halts the cache engine's processing loop
func (ce *ForgeCacheEngine) Stop() {
	ce.logger.Info("Stopping")

	if ce.stopCh != nil {
		close(ce.stopCh)
	}
}

// processLoop is the main loop for the cache engine
func (ce *ForgeCacheEngine) processLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)

	for {
		select {
		case <-ctx.Done():
			ce.logger.Info("Context canceled; stopping")
			return
		case <-ce.stopCh:
			ce.logger.Info("Stop signal received; stopping")
			return
		case <-ticker.C:
			ce.executeNextOperations(ctx)
		}
	}
}

// executeNextOperations executes the next set of operations in the cache engine
func (ce *ForgeCacheEngine) executeNextOperations(ctx context.Context) {
	var wg sync.WaitGroup
	var hadToWait bool

	ce.logger.Debug("Checking for ready operations")

	ce.opsMu.RLock()
	for _, op := range ce.ops {
		if op.GetNextExecutionTime().Before(time.Now()) {
			wg.Add(1)
			hadToWait = true

			go func(op Operation) {
				defer wg.Done()
				op.Execute(ctx)
			}(op)
		}
	}
	ce.opsMu.RUnlock()

	ce.logger.Debug("Waiting for operations to finish")
	wg.Wait()

	if hadToWait {
		ce.logger.Debug("All operations finished")
	} else {
		ce.logger.Debug("No operations were ready to execute")
	}
}

// AutoSetContextBearerToken sets the bearer token in the given context for the specified org ID; it
// will use the cached token if it is still valid, or fetch a new one if necessary.
func (ce *ForgeCacheEngine) AutoSetContextBearerToken(ctx context.Context, orgID string) (context.Context, error) {
	ce.bearerTokenMu.RLock()
	cachedToken, exists := ce.bearerTokens[orgID]
	ce.bearerTokenMu.RUnlock()

	if exists && !cachedToken.HasExpired() {
		ce.logger.Debug("Automatically setting context bearer token to cached value", "orgId", orgID, "expiresAt", cachedToken.Expiration)
		return context.WithValue(ctx, forgesdk.ContextAccessToken, cachedToken.Token), nil
	}

	ce.logger.Debug("No valid bearer token found in cache; attaining new one", "orgId", orgID)

	bearerToken, err := api.NewBearerToken(&ce.cfg, orgID)
	if err != nil {
		return ctx, err
	}

	ce.bearerTokenMu.Lock()
	ce.bearerTokens[orgID] = bearerToken
	ce.bearerTokenMu.Unlock()

	return context.WithValue(ctx, forgesdk.ContextAccessToken, bearerToken.Token), nil
}

// ClientAndContext returns a configured Forge API client and a context with the bearer token
// and server index set for the given org. The client is constructed from the SDK config stored
// at construction time so server selection happens once, not per-call.
func (ce *ForgeCacheEngine) ClientAndContext(ctx context.Context, orgID string) (*forgesdk.APIClient, context.Context, error) {
	ctx, err := ce.AutoSetContextBearerToken(ctx, orgID)
	if err != nil {
		return nil, ctx, err
	}
	ctx = context.WithValue(ctx, forgesdk.ContextServerIndex, ce.serverIdx)
	return forgesdk.NewAPIClient(ce.sdkCfg), ctx, nil
}

// GetTenantsCache returns the cache for tenant resources
func (ce *ForgeCacheEngine) GetTenantsCache() *ResourceCache[forgesdk.Tenant, *forgesdk.Tenant] {
	return ce.tenants
}

// RecordTenantSyncAuth records the authorization outcome of a tenant-sites sync. A 403 for an
// individual tenant is expected when a single API key only covers a subset of tenants, but if
// every known tenant is unauthorized it indicates a genuine credential/config problem, which is
// surfaced once at Warn level (see NKX-13835).
func (ce *ForgeCacheEngine) RecordTenantSyncAuth(tenantID string, authorized bool) {
	ce.tenantSyncAuthMu.Lock()
	defer ce.tenantSyncAuthMu.Unlock()

	ce.tenantSyncAuth[tenantID] = authorized

	// Scope the evaluation to the tenants currently in the cache. Entries for tenants that were
	// removed on rehydration are pruned so a stale `true` can't keep anyAuthorized true and
	// suppress the warning for the current tenant set.
	currentTenants := ce.GetTenantsCache().Items()
	current := make(map[string]struct{}, len(currentTenants))
	for _, t := range currentTenants {
		current[t.GetId()] = struct{}{}
	}

	anyAuthorized := false
	recorded := 0

	for id, ok := range ce.tenantSyncAuth {
		if _, present := current[id]; !present {
			delete(ce.tenantSyncAuth, id)

			continue
		}

		recorded++

		if ok {
			anyAuthorized = true
		}
	}

	if anyAuthorized {
		ce.tenantSyncAuthWarned = false

		return
	}

	// Only warn once we have an outcome for every current tenant and none were accessible, so we
	// don't warn prematurely while the first sync cycle is still in progress.
	total := len(current)
	if total > 0 && recorded >= total && !ce.tenantSyncAuthWarned {
		ce.logger.Warn(
			"Forge API key is not authorized for any known tenant; site sync cannot proceed",
			"tenantCount", total,
		)

		ce.tenantSyncAuthWarned = true
	}
}

// GetTenantSitesCache returns the cache for site resources of a specific tenant
func (ce *ForgeCacheEngine) GetTenantSitesCache(tenantID string) *ResourceCache[forgesdk.Site, *forgesdk.Site] {
	ce.tenantSitesMu.Lock()
	defer ce.tenantSitesMu.Unlock()

	if _, exists := ce.tenantSites[tenantID]; !exists {
		ce.tenantSites[tenantID] = NewResourceCache[forgesdk.Site](ce.cfg.TenantSitesCacheTTL)
	}

	return ce.tenantSites[tenantID]
}

// GetSiteInstancesCache returns the cache for instance resources of a specific site
func (ce *ForgeCacheEngine) GetSiteInstancesCache(orgID, siteID string) *ResourceCache[forgesdk.Instance, *forgesdk.Instance] {
	cacheKey := ce.getSiteResourceKey(orgID, siteID)

	ce.siteInstancesMu.Lock()
	defer ce.siteInstancesMu.Unlock()

	if _, exists := ce.siteInstances[cacheKey]; !exists {
		ce.siteInstances[cacheKey] = NewResourceCache[forgesdk.Instance](ce.cfg.SiteInstancesCacheTTL)
	}

	return ce.siteInstances[cacheKey]
}

// GetSiteInstanceTypesCache returns the cache for instance type resources of a specific site
func (ce *ForgeCacheEngine) GetSiteInstanceTypesCache(orgID, siteID string) *ResourceCache[forgesdk.InstanceType, *forgesdk.InstanceType] {
	cacheKey := ce.getSiteResourceKey(orgID, siteID)

	ce.siteInstanceTypesMu.Lock()
	defer ce.siteInstanceTypesMu.Unlock()

	if _, exists := ce.siteInstanceTypes[cacheKey]; !exists {
		ce.siteInstanceTypes[cacheKey] = NewResourceCache[forgesdk.InstanceType](ce.cfg.SiteInstanceTypesCacheTTL)
	}

	return ce.siteInstanceTypes[cacheKey]
}

// GetSiteSubnetsCache returns the cache for subnet resources of a specific site
func (ce *ForgeCacheEngine) GetSiteSubnetsCache(orgID, siteID string) *ResourceCache[forgesdk.Subnet, *forgesdk.Subnet] {
	cacheKey := ce.getSiteResourceKey(orgID, siteID)

	ce.siteSubnetsMu.Lock()
	defer ce.siteSubnetsMu.Unlock()

	if _, exists := ce.siteSubnets[cacheKey]; !exists {
		ce.siteSubnets[cacheKey] = NewResourceCache[forgesdk.Subnet](ce.cfg.SiteSubnetsCacheTTL)
	}

	return ce.siteSubnets[cacheKey]
}

// GetSiteVpcPrefixesCache returns the cache for VPC prefix resources of a specific site
func (ce *ForgeCacheEngine) GetSiteVpcPrefixesCache(orgID, siteID string) *ResourceCache[forgesdk.VpcPrefix, *forgesdk.VpcPrefix] {
	cacheKey := ce.getSiteResourceKey(orgID, siteID)

	ce.siteVpcPrefixesMu.Lock()
	defer ce.siteVpcPrefixesMu.Unlock()

	if _, exists := ce.siteVpcPrefixes[cacheKey]; !exists {
		ce.siteVpcPrefixes[cacheKey] = NewResourceCache[forgesdk.VpcPrefix](ce.cfg.SiteVpcPrefixesCacheTTL)
	}

	return ce.siteVpcPrefixes[cacheKey]
}

// GetSiteVPCsCache returns the cache for VPC resources of a specific site
func (ce *ForgeCacheEngine) GetSiteVPCsCache(orgID, siteID string) *ResourceCache[forgesdk.VPC, *forgesdk.VPC] {
	cacheKey := ce.getSiteResourceKey(orgID, siteID)

	ce.siteVPCsMu.Lock()
	defer ce.siteVPCsMu.Unlock()

	if _, exists := ce.siteVPCs[cacheKey]; !exists {
		ce.siteVPCs[cacheKey] = NewResourceCache[forgesdk.VPC](ce.cfg.SiteVPCsCacheTTL)
	}

	return ce.siteVPCs[cacheKey]
}

// GetSiteSSHKeyGroups returns the cache for SSH key group resources of a specific site
func (ce *ForgeCacheEngine) GetSiteSSHKeyGroupsCache(orgID, siteID string) *ResourceCache[forgesdk.SshKeyGroup, *forgesdk.SshKeyGroup] {
	cacheKey := ce.getSiteResourceKey(orgID, siteID)

	ce.siteSSHKeyGroupsMu.Lock()
	defer ce.siteSSHKeyGroupsMu.Unlock()

	if _, exists := ce.siteSSHKeyGroups[cacheKey]; !exists {
		ce.siteSSHKeyGroups[cacheKey] = NewResourceCache[forgesdk.SshKeyGroup](ce.cfg.SiteSSHKeyGroupsCacheTTL)
	}

	return ce.siteSSHKeyGroups[cacheKey]
}

// GetSiteIPBlocksCache returns the cache for IP block resources of a specific site
func (ce *ForgeCacheEngine) GetSiteIPBlocksCache(orgID, siteID string) *ResourceCache[forgesdk.IpBlock, *forgesdk.IpBlock] {
	cacheKey := ce.getSiteResourceKey(orgID, siteID)

	ce.siteIPBlocksMu.Lock()
	defer ce.siteIPBlocksMu.Unlock()

	if _, exists := ce.siteIPBlocks[cacheKey]; !exists {
		ce.siteIPBlocks[cacheKey] = NewResourceCache[forgesdk.IpBlock](ce.cfg.SiteIPBlocksCacheTTL)
	}

	return ce.siteIPBlocks[cacheKey]
}

// GetSiteInfiniBandPartitionsCache returns the cache for InfiniBand partition resources of a specific site
func (ce *ForgeCacheEngine) GetSiteInfiniBandPartitionsCache(orgID, siteID string) *ResourceCache[forgesdk.InfiniBandPartition, *forgesdk.InfiniBandPartition] {
	cacheKey := ce.getSiteResourceKey(orgID, siteID)

	ce.siteInfiniBandPartitionsMu.Lock()
	defer ce.siteInfiniBandPartitionsMu.Unlock()

	if _, exists := ce.siteInfiniBandPartitions[cacheKey]; !exists {
		ce.siteInfiniBandPartitions[cacheKey] = NewResourceCache[forgesdk.InfiniBandPartition](ce.cfg.SiteInfiniBandPartitionsCacheTTL)
	}

	return ce.siteInfiniBandPartitions[cacheKey]
}

// GetSiteNVLinkLogicalPartitionsCache returns the cache for NVLink logical partition resources of a specific site
func (ce *ForgeCacheEngine) GetSiteNVLinkLogicalPartitionsCache(
	orgID,
	siteID string,
) *ResourceCache[forgesdk.NVLinkLogicalPartition, *forgesdk.NVLinkLogicalPartition] {
	cacheKey := ce.getSiteResourceKey(orgID, siteID)

	ce.siteNVLinkLogicalPartitionsMu.Lock()
	defer ce.siteNVLinkLogicalPartitionsMu.Unlock()

	if _, exists := ce.siteNVLinkLogicalPartitions[cacheKey]; !exists {
		ce.siteNVLinkLogicalPartitions[cacheKey] = NewResourceCache[forgesdk.NVLinkLogicalPartition](
			ce.cfg.SiteNVLinkLogicalPartitionsCacheTTL,
		)
	}

	return ce.siteNVLinkLogicalPartitions[cacheKey]
}

// RefreshSite discovers a registered site and hydrates its resource caches.
func (ce *ForgeCacheEngine) RefreshSite(ctx context.Context, siteID string) {
	ce.logger.Info("Refreshing site", "siteId", siteID)

	ce.opsMu.RLock()
	var syncOps []Operation
	for opID, op := range ce.ops {
		if strings.HasPrefix(opID, opPrefixSyncTenantSites) {
			syncOps = append(syncOps, op)
		}
	}
	ce.opsMu.RUnlock()

	for _, op := range syncOps {
		op.Execute(ctx)
	}

	ce.opsMu.RLock()
	var siteOps []Operation
	for opID, op := range ce.ops {
		if strings.HasSuffix(opID, "-"+siteID) {
			siteOps = append(siteOps, op)
		}
	}
	ce.opsMu.RUnlock()

	for _, op := range siteOps {
		op.Execute(ctx)
	}

	ce.logger.Info("Completed site refresh", "siteId", siteID)
}

// getSiteResourceKey generates a unique key for site-scoped resources based on the site ID and org ID
// to avoid potential collisions between sites with the same ID but with different organizations.
// This function exists to keep the key generation logic in one place (as opposed to each function
// that needs to generate a key doing it themselves).
func (ce *ForgeCacheEngine) getSiteResourceKey(orgID, siteID string) string {
	return fmt.Sprintf("%s:%s", orgID, siteID)
}
