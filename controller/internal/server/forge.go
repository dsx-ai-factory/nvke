// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

	"github.com/NVIDIA/nke/controller/api"
	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/providers/forge"
	forgeapi "github.com/NVIDIA/nke/controller/internal/providers/forge/api"
)

const (
	defaultSiteName = "Default Site"
)

func getTenants(dockerTest bool) []api.ForgeTenant {
	tenants := forgeapi.GetTenants(dockerTest)
	response := make([]api.ForgeTenant, 0, len(tenants))

	for _, tenant := range tenants {
		response = append(response, api.ForgeTenant{
			ID:    uuid.MustParse(tenant.GetId()),
			OrgID: tenant.GetOrg(),
			Name:  tenant.GetOrgDisplayName(),
		})
	}

	return response
}

type forgeService struct {
	logger        *slog.Logger
	cfg           config.Config
	forgeProvider *forge.ForgeProvider
}

func NewForgeService(cfg config.Config, logger *slog.Logger, forgeProvider *forge.ForgeProvider) *forgeService {
	return &forgeService{
		logger:        logger,
		cfg:           cfg,
		forgeProvider: forgeProvider,
	}
}

func (s *forgeService) ListForgeTenants(
	ctx context.Context,
	req api.ListForgeTenantsRequestObject,
) (api.ListForgeTenantsResponseObject, error) {
	return api.ListForgeTenants200JSONResponse{Tenants: getTenants(s.cfg.ForgeEnvironment == config.ForgeEnvironmentTest)}, nil
}

func (s *forgeService) ListCNIPlugins(
	ctx context.Context,
	req api.ListCNIPluginsRequestObject,
) (api.ListCNIPluginsResponseObject, error) {
	dbCNIPlugins := db.ListCNIPlugins()

	cniPlugins := make([]string, len(dbCNIPlugins))
	for i, plugin := range dbCNIPlugins {
		cniPlugins[i] = string(plugin)
	}

	return api.ListCNIPlugins200JSONResponse{CniPlugins: cniPlugins}, nil
}

func (s *forgeService) ListForgeInstances(
	ctx context.Context,
	req api.ListForgeInstancesRequestObject,
) (api.ListForgeInstancesResponseObject, error) {
	s.logger.Info("ListForgeInstances request", "orgID", req.Params.OrgID)
	instances, err := s.forgeProvider.GetInstances(ctx, req.Params.OrgID, req.Params.SiteID.String())
	if err != nil {
		return nil, fmt.Errorf("failed to list Forge instances: %w", err)
	}

	// filter the instances by VPC ID if provided
	response := []api.ForgeInstance{}
	for _, instance := range instances {
		var filteredInstance *forgesdk.Instance

		noVPCSpecified := req.Params.VpcID == nil || req.Params.VpcID.String() == ""
		vpcsMatch := !noVPCSpecified && instance.GetVpcId() == req.Params.VpcID.String()

		if noVPCSpecified || vpcsMatch {
			filteredInstance = instance
		}

		if filteredInstance == nil {
			continue
		}

		// type conversion
		var subnetUUID *uuid.UUID
		var vpcPrefixUUID *uuid.UUID
		interfaces := filteredInstance.GetInterfaces()
		if len(interfaces) > 0 {
			if subnetID, err := uuid.Parse(interfaces[0].GetSubnetId()); err == nil {
				subnetUUID = &subnetID
			}
			if vpcPrefixID, err := uuid.Parse(interfaces[0].GetVpcPrefixId()); err == nil {
				vpcPrefixUUID = &vpcPrefixID
			}
		}

		response = append(response, api.ForgeInstance{
			ID:          uuid.MustParse(filteredInstance.GetId()),
			Name:        filteredInstance.GetName(),
			SubnetID:    subnetUUID,
			VpcPrefixID: vpcPrefixUUID,
		})
	}

	return api.ListForgeInstances200JSONResponse{Instances: response}, nil
}

func (s *forgeService) ListForgeSubnets(
	ctx context.Context,
	req api.ListForgeSubnetsRequestObject,
) (api.ListForgeSubnetsResponseObject, error) {
	s.logger.Info("ListForgeSubnets request", "orgID", req.Params.OrgID)
	subnets, err := s.forgeProvider.GetSubnets(ctx, req.Params.OrgID, req.Params.SiteID.String())
	if err != nil {
		return nil, fmt.Errorf("failed to list Forge subnets: %w", err)
	}

	// filter the subnets by VPC ID if provided
	response := []api.ForgeSubnet{}
	for _, subnet := range subnets {
		var filteredSubnet *forgesdk.Subnet

		noVPCSpecified := req.Params.VpcID == nil || req.Params.VpcID.String() == ""
		vpcsMatch := !noVPCSpecified && subnet.GetVpcId() == req.Params.VpcID.String()

		if noVPCSpecified || vpcsMatch {
			filteredSubnet = subnet
		}

		if filteredSubnet == nil {
			continue
		}

		// type conversion
		prefixLength := int(filteredSubnet.GetPrefixLength())
		response = append(response, api.ForgeSubnet{
			ID:           uuid.MustParse(filteredSubnet.GetId()),
			Name:         filteredSubnet.GetName(),
			PrefixLength: &prefixLength,
		})
	}

	return api.ListForgeSubnets200JSONResponse{Subnets: response}, nil
}

func (s *forgeService) ListForgeVPCs(
	ctx context.Context,
	req api.ListForgeVPCsRequestObject,
) (api.ListForgeVPCsResponseObject, error) {
	s.logger.Info("ListForgeVPCs request", "orgID", req.Params.OrgID)
	vpcs, err := s.forgeProvider.GetVPCs(ctx, req.Params.OrgID, req.Params.SiteID.String())
	if err != nil {
		return nil, fmt.Errorf("failed to list Forge VPCs: %w", err)
	}

	// Type conversion
	response := make([]api.ForgeVPC, 0)
	for _, vpc := range vpcs {
		response = append(response, api.ForgeVPC{
			ID:   uuid.MustParse(vpc.GetId()),
			Name: vpc.GetName(),
		})
	}

	return api.ListForgeVPCs200JSONResponse{Vpcs: response}, nil
}

func (s *forgeService) ListForgeVpcPrefixes(
	ctx context.Context,
	req api.ListForgeVpcPrefixesRequestObject,
) (api.ListForgeVpcPrefixesResponseObject, error) {
	s.logger.Info("ListForgeVpcPrefixes request", "orgID", req.Params.OrgID)
	vpcPrefixes, err := s.forgeProvider.GetVpcPrefixes(ctx, req.Params.OrgID, req.Params.SiteID.String())
	if err != nil {
		return nil, fmt.Errorf("failed to list Forge VPC prefixes: %w", err)
	}

	// filter the VPC prefixes by VPC ID if provided
	response := []api.ForgeVpcPrefix{}
	for _, vpcPrefix := range vpcPrefixes {
		var filteredVpcPrefix *forgesdk.VpcPrefix

		noVPCSpecified := req.Params.VpcID == nil || req.Params.VpcID.String() == ""
		vpcsMatch := !noVPCSpecified && vpcPrefix.GetVpcId() == req.Params.VpcID.String()

		if noVPCSpecified || vpcsMatch {
			filteredVpcPrefix = vpcPrefix
		}

		if filteredVpcPrefix == nil {
			continue
		}

		// type conversion
		prefixLength := int(filteredVpcPrefix.GetPrefixLength())
		response = append(response, api.ForgeVpcPrefix{
			ID:           uuid.MustParse(filteredVpcPrefix.GetId()),
			Name:         filteredVpcPrefix.GetName(),
			PrefixLength: &prefixLength,
		})
	}

	return api.ListForgeVpcPrefixes200JSONResponse{VpcPrefixes: response}, nil
}

func (s *forgeService) ListForgeSSHKeyGroups(
	ctx context.Context,
	req api.ListForgeSSHKeyGroupsRequestObject,
) (api.ListForgeSSHKeyGroupsResponseObject, error) {
	s.logger.Info("ListForgeSSHKeyGroups request", "orgID", req.Params.OrgID)
	sshKeyGroups, err := s.forgeProvider.GetSSHKeyGroups(ctx, req.Params.OrgID, req.Params.SiteID.String())
	if err != nil {
		return nil, fmt.Errorf("failed to list Forge SSH key groups: %w", err)
	}

	// Type conversion
	response := make([]api.ForgeSSHKeyGroup, 0)
	for _, sshKeyGroup := range sshKeyGroups {
		siteAssociations := make([]string, 0)
		for _, siteAssociation := range sshKeyGroup.GetSiteAssociations() {
			site := siteAssociation.GetSite()
			siteAssociations = append(siteAssociations, site.GetName())
		}

		response = append(response, api.ForgeSSHKeyGroup{
			ID:               uuid.MustParse(sshKeyGroup.GetId()),
			Name:             sshKeyGroup.GetName(),
			SiteAssociations: &siteAssociations,
		})
	}

	return api.ListForgeSSHKeyGroups200JSONResponse{SSHKeyGroups: response}, nil
}

func (s *forgeService) ListForgeInstanceTypes(
	ctx context.Context,
	req api.ListForgeInstanceTypesRequestObject,
) (api.ListForgeInstanceTypesResponseObject, error) {
	s.logger.Info("ListForgeInstanceTypes request", "orgID", req.Params.OrgID, "siteID", req.Params.SiteID)
	instanceTypes, err := s.forgeProvider.GetInstanceTypes(ctx, req.Params.OrgID, req.Params.SiteID.String())
	if err != nil {
		return nil, fmt.Errorf("failed to list Forge instance types: %w", err)
	}

	// Type conversion (unallocated types excluded via Forge excludeUnallocated query param on cache sync).
	response := make([]api.ForgeInstanceType, 0)
	for _, instanceType := range instanceTypes {
		allocationStats := instanceType.GetAllocationStats()
		total := int(allocationStats.GetTotal())
		unused := int(allocationStats.GetUnused())
		unusedUsable := int(allocationStats.GetUnusedUsable())

		response = append(response, api.ForgeInstanceType{
			ID:           uuid.MustParse(instanceType.GetId()),
			Name:         instanceType.GetName(),
			Total:        &total,
			Unused:       &unused,
			UnusedUsable: &unusedUsable,
		})
	}

	return api.ListForgeInstanceTypes200JSONResponse{InstanceTypes: response}, nil
}

func (s *forgeService) ListForgeInfiniBandPartitions(
	ctx context.Context,
	req api.ListForgeInfiniBandPartitionsRequestObject,
) (api.ListForgeInfiniBandPartitionsResponseObject, error) {
	s.logger.Info("ListForgeInfiniBandPartitions request", "orgID", req.Params.OrgID, "siteID", req.Params.SiteID)
	infiniBandPartitions, err := s.forgeProvider.GetInfiniBandPartitions(ctx, req.Params.OrgID, req.Params.SiteID.String())
	if err != nil {
		return nil, fmt.Errorf("failed to list Forge infiniband partitions: %w", err)
	}

	// Type conversion
	response := make([]api.ForgeInfiniBandPartition, 0)
	for _, infiniBandPartition := range infiniBandPartitions {
		response = append(response, api.ForgeInfiniBandPartition{
			ID:   uuid.MustParse(infiniBandPartition.GetId()),
			Name: infiniBandPartition.GetName(),
		})
	}

	return api.ListForgeInfiniBandPartitions200JSONResponse{InfiniBandPartitions: response}, nil
}

func (s *forgeService) ListForgeNVLinkLogicalPartitions(
	ctx context.Context,
	req api.ListForgeNVLinkLogicalPartitionsRequestObject,
) (api.ListForgeNVLinkLogicalPartitionsResponseObject, error) {
	s.logger.Info("ListForgeNVLinkLogicalPartitions request", "orgID", req.Params.OrgID, "siteID", req.Params.SiteID)

	nvlinkLogicalPartitions, err := s.forgeProvider.GetNVLinkLogicalPartitions(
		ctx,
		req.Params.OrgID,
		req.Params.SiteID.String(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list Forge NVLink logical partitions: %w", err)
	}

	response := make([]api.ForgeNVLinkLogicalPartition, 0, len(nvlinkLogicalPartitions))
	for _, nvlinkLogicalPartition := range nvlinkLogicalPartitions {
		response = append(response, api.ForgeNVLinkLogicalPartition{
			ID:   uuid.MustParse(nvlinkLogicalPartition.GetId()),
			Name: nvlinkLogicalPartition.GetName(),
		})
	}

	return api.ListForgeNVLinkLogicalPartitions200JSONResponse{NvLinkLogicalPartitions: response}, nil
}

func (s *forgeService) ListForgeSites(
	ctx context.Context,
	req api.ListForgeSitesRequestObject,
) (api.ListForgeSitesResponseObject, error) {
	s.logger.Info("ListForgeSites request", "orgID", req.Params.OrgID)

	sites, err := s.forgeProvider.GetSites(ctx, req.Params.OrgID)
	if err != nil {
		return nil, fmt.Errorf("failed to list Forge sites: %w", err)
	}

	// Type conversion
	response := make([]api.ForgeSite, 0)
	for _, site := range sites {
		siteID := uuid.MustParse(site.GetId())
		siteName := site.GetName()
		if siteID == uuid.Nil {
			siteName = defaultSiteName
		}

		capabilities := site.GetCapabilities()
		nativeNetworking := capabilities.GetNativeNetworking()

		response = append(response, api.ForgeSite{
			ID:   siteID,
			Name: siteName,
			Capabilities: &api.ForgeSiteCapabilities{
				NativeNetworking: nativeNetworking,
			},
		})
	}

	return api.ListForgeSites200JSONResponse{Sites: response}, nil
}

func (s *forgeService) ListForgeIPBlocks(
	ctx context.Context,
	req api.ListForgeIPBlocksRequestObject,
) (api.ListForgeIPBlocksResponseObject, error) {
	s.logger.Info("ListForgeIPBlocks request", "orgID", req.Params.OrgID)
	ipBlocks, err := s.forgeProvider.GetIPBlocks(ctx, req.Params.OrgID, req.Params.SiteID.String())
	if err != nil {
		return nil, fmt.Errorf("failed to list Forge IP blocks: %w", err)
	}

	// Type conversion
	response := make([]api.ForgeIPBlock, 0)
	for _, ipBlock := range ipBlocks {
		response = append(response, api.ForgeIPBlock{
			ID:   uuid.MustParse(ipBlock.GetId()),
			Name: ipBlock.Name,
		})
	}

	return api.ListForgeIPBlocks200JSONResponse{IPBlocks: response}, nil
}
