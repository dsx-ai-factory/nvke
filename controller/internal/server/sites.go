// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"

	"github.com/google/uuid"

	"github.com/NVIDIA/nke/controller/api"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/providers/forge"
)

var (
	errVIPOutOfRange = errors.New("VIP is not within site VIP range")
	errSiteNotFound  = errors.New("site not found")
)

const (
	vipAllocationTypeCluster        = api.CreateVIPAllocationRequestAllocationType("cluster")
	vipAllocationTypeSiteController = api.CreateVIPAllocationRequestAllocationType("siteController")
)

type siteService struct {
	logger        *slog.Logger
	db            db.DB
	forgeProvider *forge.ForgeProvider
}

func NewSiteService(logger *slog.Logger, database db.DB, forgeProvider *forge.ForgeProvider) *siteService {
	return &siteService{
		logger:        logger,
		db:            database,
		forgeProvider: forgeProvider,
	}
}

func (s *siteService) ListSites(ctx context.Context, req api.ListSitesRequestObject) (api.ListSitesResponseObject, error) {
	sites, err := s.db.ListSites(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list sites: %w", err)
	}

	apiSites := make([]api.Site, len(sites))
	for i, site := range sites {
		vipAllocations, err := s.db.ListVIPAllocations(ctx, &site.ID)
		if err != nil {
			return nil, fmt.Errorf("failed to list VIP allocations: %w", err)
		}
		apiSites[i] = apiSiteFromDB(site, vipAllocations)
	}

	return api.ListSites200JSONResponse{Sites: apiSites}, nil
}

func (s *siteService) RegisterSite(ctx context.Context, req api.RegisterSiteRequestObject) (api.RegisterSiteResponseObject, error) {
	if req.Body == nil {
		return api.RegisterSite400JSONResponse{Message: "Request body is required"}, nil
	}

	vipRanges := normalizedVIPRanges(req.Body.VIPRanges)

	// Validate all VIP ranges
	for i, vipRange := range vipRanges {
		if _, err := netip.ParsePrefix(vipRange); err != nil {
			return api.RegisterSite400JSONResponse{Message: fmt.Sprintf("Invalid VIP range at index %d: %s", i, vipRange)}, nil
		}
	}

	dbSite := db.Site{
		ID:        req.Body.ID,
		VIPRanges: vipRanges,
	}

	site, err := s.db.RegisterSite(ctx, &dbSite)
	if err != nil {
		if errors.Is(err, db.ErrSiteIDAlreadyExists) {
			return api.RegisterSite400JSONResponse{Message: "Site ID already exists"}, nil
		}
		return nil, fmt.Errorf("failed to register site: %w", err)
	}

	// Populate the newly registered site's Forge resource caches before responding.
	if req.Body.ID != uuid.Nil {
		s.forgeProvider.GetCacheEngine().RefreshSite(ctx, req.Body.ID.String())
	}

	return api.RegisterSite201JSONResponse(apiSiteFromDB(site, nil)), nil
}

func (s *siteService) GetSite(ctx context.Context, req api.GetSiteRequestObject) (api.GetSiteResponseObject, error) {
	site, err := s.db.GetSite(ctx, req.SiteID, false)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return api.GetSite404Response{}, nil
		}
		return nil, fmt.Errorf("failed to get site: %w", err)
	}
	vipAllocations, err := s.db.ListVIPAllocations(ctx, &site.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to list VIP allocations: %w", err)
	}

	return api.GetSite200JSONResponse(apiSiteFromDB(site, vipAllocations)), nil
}

func (s *siteService) ListVIPAllocations(
	ctx context.Context,
	req api.ListVIPAllocationsRequestObject,
) (api.ListVIPAllocationsResponseObject, error) {
	vipAllocations, err := s.db.ListVIPAllocations(ctx, req.Params.SiteID)
	if err != nil {
		return nil, fmt.Errorf("failed to list VIP allocations: %w", err)
	}

	return api.ListVIPAllocations200JSONResponse{VipAllocations: apiVIPAllocationsFromDB(vipAllocations)}, nil
}

func (s *siteService) GetVIPAllocation(
	ctx context.Context,
	req api.GetVIPAllocationRequestObject,
) (api.GetVIPAllocationResponseObject, error) {
	vipAllocation, err := s.db.GetVIPAllocation(ctx, req.VipAllocationID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return api.GetVIPAllocation404Response{}, nil
		}
		return nil, fmt.Errorf("failed to get VIP allocation: %w", err)
	}

	return api.GetVIPAllocation200JSONResponse(apiVIPAllocationFromDB(vipAllocation)), nil
}

func (s *siteService) CreateVIPAllocation(
	ctx context.Context,
	req api.CreateVIPAllocationRequestObject,
) (api.CreateVIPAllocationResponseObject, error) {
	if req.Body == nil {
		return api.CreateVIPAllocation400JSONResponse{Message: "Request body is required"}, nil
	}

	// Validate VIP format
	if _, err := netip.ParseAddr(req.Body.Vip); err != nil {
		return api.CreateVIPAllocation400JSONResponse{Message: "Invalid VIP address format"}, nil
	}

	// Validate that clusterID is provided if allocationType is cluster
	if req.Body.AllocationType == vipAllocationTypeCluster && req.Body.ClusterID == nil {
		return api.CreateVIPAllocation400JSONResponse{Message: "ClusterID is required when allocationType is cluster"}, nil
	}

	// Validate that clusterID is not provided if allocationType is siteController
	if req.Body.AllocationType == vipAllocationTypeSiteController && req.Body.ClusterID != nil {
		return api.CreateVIPAllocation400JSONResponse{Message: "ClusterID should not be provided when allocationType is siteController"}, nil
	}

	var createdAllocation db.VIPAllocation
	err := s.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		// Verify site exists and get VIP range for validation
		site, err := tx.GetSite(ctx, req.Body.SiteID, false)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return errSiteNotFound
			}
			return fmt.Errorf("failed to get site: %w", err)
		}

		// Validate VIP is within site ranges
		if err := validateVIPInRanges(req.Body.Vip, site.VIPRanges); err != nil {
			return err
		}

		// Validate cluster exists if clusterID is provided
		if req.Body.ClusterID != nil {
			_, err := tx.GetCluster(ctx, *req.Body.ClusterID)
			if err != nil {
				if errors.Is(err, db.ErrNotFound) {
					return errClusterNotFound
				}
				return fmt.Errorf("failed to validate cluster: %w", err)
			}
		}

		// Create the allocation
		allocation := db.VIPAllocation{
			SiteID:         req.Body.SiteID,
			VIP:            req.Body.Vip,
			AllocationType: string(req.Body.AllocationType),
			ClusterID:      req.Body.ClusterID,
		}

		createdAllocation, err = tx.CreateVIPAllocation(ctx, &allocation)
		if err != nil {
			if errors.Is(err, db.ErrVIPAlreadyAllocated) {
				return err
			}
			if errors.Is(err, db.ErrClusterAlreadyHasVIP) {
				return err
			}
			return fmt.Errorf("failed to create VIP allocation: %w", err)
		}

		return nil
	})
	//nolint:nestif // consistency with other error handlers
	if err != nil {
		if errors.Is(err, errSiteNotFound) {
			return api.CreateVIPAllocation400JSONResponse{Message: "Site not found"}, nil
		}
		if errors.Is(err, errClusterNotFound) {
			return api.CreateVIPAllocation400JSONResponse{Message: "Cluster not found"}, nil
		}
		if errors.Is(err, db.ErrVIPAlreadyAllocated) {
			return api.CreateVIPAllocation400JSONResponse{Message: "VIP is already allocated"}, nil
		}
		if errors.Is(err, db.ErrClusterAlreadyHasVIP) {
			return api.CreateVIPAllocation400JSONResponse{Message: "Cluster already has a VIP allocation"}, nil
		}
		if errors.Is(err, errVIPOutOfRange) {
			return api.CreateVIPAllocation400JSONResponse{Message: "VIP is not within site VIP range"}, nil
		}
		return nil, err
	}

	return api.CreateVIPAllocation201JSONResponse(apiVIPAllocationFromDB(createdAllocation)), nil
}

func apiSiteFromDB(site db.Site, vipAllocations []db.VIPAllocation) api.Site {
	return api.Site{
		ID:             site.ID,
		VIPRanges:      normalizedVIPRanges(site.VIPRanges),
		VIPAllocations: apiVIPAllocationsFromDB(vipAllocations),
	}
}

func apiVIPAllocationsFromDB(vips []db.VIPAllocation) []api.VIPAllocation {
	apiVIPs := make([]api.VIPAllocation, len(vips))
	for i, vip := range vips {
		apiVIPs[i] = apiVIPAllocationFromDB(vip)
	}
	return apiVIPs
}

func apiVIPAllocationFromDB(vip db.VIPAllocation) api.VIPAllocation {
	return api.VIPAllocation{
		ID:             vip.ID,
		SiteID:         vip.SiteID,
		VIP:            vip.VIP,
		AllocationType: api.VIPAllocationAllocationType(vip.AllocationType),
		ClusterID:      vip.ClusterID,
		CreatedAt:      &vip.CreatedAt,
	}
}

func validateVIPInRanges(vip string, vipRanges []string) error {
	addr, err := netip.ParseAddr(vip)
	if err != nil {
		return fmt.Errorf("failed to parse VIP address: %w", err)
	}

	for _, vipRange := range vipRanges {
		prefix, err := netip.ParsePrefix(vipRange)
		if err != nil {
			return fmt.Errorf("failed to parse VIP range %s: %w", vipRange, err)
		}

		if prefix.Contains(addr) {
			return nil // VIP is valid if it's in any of the ranges
		}
	}

	return errVIPOutOfRange
}

func normalizedVIPRanges(vipRanges []string) []string {
	if vipRanges == nil {
		return []string{}
	}

	return vipRanges
}
