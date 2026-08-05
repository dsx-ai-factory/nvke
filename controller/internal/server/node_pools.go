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

	"github.com/google/uuid"

	forgesdk "gitlab-master.nvidia.com/nvmetal/cloud-api-sdk-go"

	"github.com/NVIDIA/nke/controller/api"
	"github.com/NVIDIA/nke/controller/internal/capi"
	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/naming"
)

var (
	errClusterNotFound  = errors.New("cluster not found")
	errClusterDeleting  = errors.New("cluster is deleting")
	errNodePoolDeleting = errors.New("node pool is deleting")
)

type badRequestError struct {
	message string
}

func (e *badRequestError) Error() string {
	return e.message
}

func newBadRequestErrorf(format string, args ...any) error {
	return &badRequestError{message: fmt.Sprintf(format, args...)}
}

type forgeSiteGetter interface {
	GetSiteByID(ctx context.Context, orgID, siteID string) (*forgesdk.Site, error)
}

type nodePoolForgeProvider interface {
	forgeSiteGetter
	forgeClusterNetworkProvider
}

type nodePoolService struct {
	logger        *slog.Logger
	db            db.DB
	forgeProvider nodePoolForgeProvider
	teleportCfg   config.TeleportConfig
}

func NewNodePoolService(logger *slog.Logger, database db.DB, forgeProvider nodePoolForgeProvider, teleportCfg config.TeleportConfig) *nodePoolService {
	return &nodePoolService{
		logger:        logger,
		db:            database,
		forgeProvider: forgeProvider,
		teleportCfg:   teleportCfg,
	}
}

// applyNodePoolDefaults fills in server-side defaults for omitted node pool fields
// based on the pool's role (control plane vs worker) and infrastructure type.
func (s *nodePoolService) applyNodePoolDefaults(req *api.CreateNodePoolRequest, cluster db.Cluster) {
	if req.TeleportEnabled == nil {
		v := s.teleportCfg.NodePoolTeleportDefault(req.ControlPlane)
		req.TeleportEnabled = &v
	}
	if !clusterIsCAPIBacked(cluster) && req.DiskSelectionStrategy == "" {
		req.DiskSelectionStrategy = api.DiskSelectionStrategyCreateSmallest
	}
}

func (s *nodePoolService) ListNodePools(ctx context.Context, req api.ListNodePoolsRequestObject) (api.ListNodePoolsResponseObject, error) {
	filters := db.NodePoolFilters{
		ClusterID:   req.Params.ClusterID,
		Name:        req.Params.Name,
		ClusterName: req.Params.ClusterName,
	}

	dbNodePools, err := s.db.ListNodePools(ctx, filters)
	if err != nil {
		return nil, fmt.Errorf("failed to list node pools: %w", err)
	}
	apiNodePools := make([]api.NodePool, len(dbNodePools))
	for i := range dbNodePools {
		apiNodePools[i] = apiNodePoolFromDB(&dbNodePools[i])
	}
	return api.ListNodePools200JSONResponse{NodePools: apiNodePools}, nil
}

func (s *nodePoolService) CreateNodePool(
	ctx context.Context,
	req api.CreateNodePoolRequestObject,
) (api.CreateNodePoolResponseObject, error) {
	cluster, err := s.db.GetCluster(ctx, req.Body.ClusterID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return api.CreateNodePool400JSONResponse{Message: "Cluster not found"}, nil
		}
		return nil, fmt.Errorf("failed to get cluster: %w", err)
	}

	if err := s.validateCreateNodePoolRequest(ctx, cluster, req.Body); err != nil {
		return api.CreateNodePool400JSONResponse{Message: err.Error()}, nil
	}
	if !clusterIsCAPIBacked(cluster) {
		network, err := resolveStoredClusterNetwork(cluster)
		if err != nil {
			return nil, fmt.Errorf("invalid stored cluster network: %w", err)
		}
		if err := validateForgeNetworkIDs(
			ctx,
			s.forgeProvider,
			cluster.OrgID,
			cluster.SiteID,
			req.Body.SubnetIDs,
			req.Body.VpcPrefixIDs,
			network,
		); err != nil {
			var badRequestErr *badRequestError
			if errors.As(err, &badRequestErr) {
				return api.CreateNodePool400JSONResponse{Message: badRequestErr.Error()}, nil
			}
			return nil, err
		}
	}

	var createdNodePool db.NodePool
	err = s.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		cluster, err := tx.GetClusterForUpdate(ctx, req.Body.ClusterID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return errClusterNotFound
			}
			return fmt.Errorf("failed to get cluster: %w", err)
		}

		if cluster.Conditions.Deleting.Status == db.ConditionStatusTrue {
			return errClusterDeleting
		}

		s.applyNodePoolDefaults(req.Body, cluster)
		dbNodePool, err := dbNodePoolFromAPI(req.Body, cluster.KubernetesVersion)
		if err != nil {
			return fmt.Errorf("failed to convert node pool request: %w", err)
		}
		clusterID := cluster.ID
		nodePools, err := tx.ListNodePools(ctx, db.NodePoolFilters{ClusterID: &clusterID})
		if err != nil {
			return fmt.Errorf("failed to list node pools for pod CIDR capacity validation: %w", err)
		}
		nodePools = append(nodePools, dbNodePool)
		if err := validateStoredPodCIDRCapacity(cluster, nodePools); err != nil {
			return newBadRequestErrorf("%s", err)
		}

		createdNodePool, err = tx.CreateNodePool(ctx, &dbNodePool)
		if err != nil {
			return fmt.Errorf("failed to create node pool: %w", err)
		}

		workflowID := constants.ProvisionNodePoolWorkflowPrefix + createdNodePool.ID.String()
		_, err = tx.CreateWorkflowOutboxEntry(ctx, "ProvisionNodePool", workflowID, map[string]interface{}{
			"nodePoolID": createdNodePool.ID,
		})
		if err != nil {
			return fmt.Errorf("failed to create workflow outbox entry: %w", err)
		}

		return nil
	})
	if err != nil {
		var badRequestErr *badRequestError
		if errors.As(err, &badRequestErr) {
			return api.CreateNodePool400JSONResponse{Message: badRequestErr.Error()}, nil
		}
		if errors.Is(err, errClusterDeleting) {
			return api.CreateNodePool409JSONResponse{Message: "Cannot create node pool when parent cluster is deleting"}, nil
		}
		if errors.Is(err, errClusterNotFound) {
			return api.CreateNodePool400JSONResponse{Message: "Parent cluster not found"}, nil
		}
		if errors.Is(err, db.ErrNodePoolNameNotUniqueInCluster) {
			return api.CreateNodePool400JSONResponse{
				Message: fmt.Sprintf("A node pool already exists with the name %q in cluster ID %q", req.Body.Name, req.Body.ClusterID),
			}, nil
		}
		return nil, err
	}

	return api.CreateNodePool201JSONResponse(apiNodePoolFromDB(&createdNodePool)), nil
}

func isValidDiskSelectionStrategy(s api.DiskSelectionStrategyCreate) bool {
	switch s {
	case api.DiskSelectionStrategyCreateSmallest, api.DiskSelectionStrategyCreateLargest:
		return true
	default:
		return false
	}
}

func validateForgeDiskSelectionStrategy(s api.DiskSelectionStrategyCreate) error {
	if s != "" && !isValidDiskSelectionStrategy(s) {
		return fmt.Errorf("invalid diskSelectionStrategy: %q", s)
	}

	return nil
}

func validateCAPISpireAttestationMethod(method string) error {
	switch method {
	case "":
		return fmt.Errorf("capiSpireAttestationMethod is required for CAPI clusters")
	case string(db.SpireAttestationMethodTPM), string(db.SpireAttestationMethodJoinToken):
		return nil
	default:
		return fmt.Errorf("invalid capiSpireAttestationMethod: %q", method)
	}
}

func clusterIsCAPIBacked(cluster db.Cluster) bool {
	return cluster.SiteID == uuid.Nil
}

func unsupportedNodePoolFieldError(fieldName, clusterType string) error {
	return fmt.Errorf("%s is not supported for %s", fieldName, clusterType)
}

func validateCreateCAPIRestrictedFields(req *api.CreateNodePoolRequest) error {
	switch {
	case req.InstanceTypeID != uuid.Nil:
		return unsupportedNodePoolFieldError("instanceTypeID", "CAPI clusters")
	case req.InfinibandPartitionID != nil:
		return unsupportedNodePoolFieldError("infinibandPartitionID", "CAPI clusters")
	case req.NvLinkLogicalPartitionID != nil:
		return unsupportedNodePoolFieldError("nvLinkLogicalPartitionID", "CAPI clusters")
	case req.SSHKeyGroupIDs != nil:
		return unsupportedNodePoolFieldError("sshKeyGroupIDs", "CAPI clusters")
	case req.SubnetIDs != nil:
		return unsupportedNodePoolFieldError("subnetIDs", "CAPI clusters")
	case req.VpcPrefixIDs != nil:
		return unsupportedNodePoolFieldError("vpcPrefixIDs", "CAPI clusters")
	case req.OSProfile != "":
		return unsupportedNodePoolFieldError("osProfile", "CAPI clusters")
	case req.DiskSelectionStrategy != "":
		return unsupportedNodePoolFieldError("diskSelectionStrategy", "CAPI clusters")
	default:
		return nil
	}
}

func validateCreateForgeRestrictedFields(req *api.CreateNodePoolRequest) error {
	switch {
	case req.CapiInfraMachineTemplate != nil:
		return unsupportedNodePoolFieldError("capiInfraMachineTemplate", "Forge clusters")
	case req.CapiSpireAttestationMethod != "":
		return unsupportedNodePoolFieldError("capiSpireAttestationMethod", "Forge clusters")
	case req.CapiBootstrapFiles != nil:
		return unsupportedNodePoolFieldError("capiBootstrapFiles", "Forge clusters")
	case req.CapiPreKubeadmCommands != nil:
		return unsupportedNodePoolFieldError("capiPreKubeadmCommands", "Forge clusters")
	default:
		return nil
	}
}

func validateUpdateCAPIRestrictedFields(req *api.UpdateNodePoolRequest) error {
	switch {
	case req.OSProfile != nil:
		return unsupportedNodePoolFieldError("osProfile", "CAPI clusters")
	case req.SubnetIDs != nil:
		return unsupportedNodePoolFieldError("subnetIDs", "CAPI clusters")
	case req.VpcPrefixIDs != nil:
		return unsupportedNodePoolFieldError("vpcPrefixIDs", "CAPI clusters")
	case req.DiskSelectionStrategy != nil && *req.DiskSelectionStrategy != "":
		return unsupportedNodePoolFieldError("diskSelectionStrategy", "CAPI clusters")
	default:
		return nil
	}
}

func validateUpdateForgeRestrictedFields(req *api.UpdateNodePoolRequest) error {
	switch {
	case req.CapiInfraMachineTemplate != nil:
		return unsupportedNodePoolFieldError("capiInfraMachineTemplate", "Forge clusters")
	case req.CapiBootstrapFiles != nil:
		return unsupportedNodePoolFieldError("capiBootstrapFiles", "Forge clusters")
	case req.CapiPreKubeadmCommands != nil:
		return unsupportedNodePoolFieldError("capiPreKubeadmCommands", "Forge clusters")
	case req.CapiSpireAttestationMethod != nil:
		return unsupportedNodePoolFieldError("capiSpireAttestationMethod", "Forge clusters")
	default:
		return nil
	}
}

func validateCloudInitFiles(files []api.CloudInitFile) error {
	seenPaths := make(map[string]struct{}, len(files))
	for _, file := range files {
		if file.Path == "" {
			return fmt.Errorf("file path is required")
		}
		if file.Content == "" {
			return fmt.Errorf("file content is required for path %q", file.Path)
		}
		switch file.Path {
		case constants.CAPISpireAgentConfigPath, constants.CAPISpireTrustBundlePath:
			return fmt.Errorf("file path %q is managed by CWE and must not be provided", file.Path)
		}
		if _, exists := seenPaths[file.Path]; exists {
			return fmt.Errorf("duplicate file path %q", file.Path)
		}
		seenPaths[file.Path] = struct{}{}
	}
	return nil
}

func (s *nodePoolService) validateCreateNodePoolRequest(ctx context.Context, cluster db.Cluster, req *api.CreateNodePoolRequest) error {
	if err := naming.ValidateNodeNamePrefix(cluster.Name, req.NodeNamePrefix); err != nil {
		return err
	}

	if clusterIsCAPIBacked(cluster) {
		return validateCreateCAPINodePoolRequest(req)
	}

	return s.validateCreateForgeNodePoolRequest(ctx, cluster, req)
}

func validateCreateCAPINodePoolRequest(req *api.CreateNodePoolRequest) error {
	if err := validateCreateCAPIRestrictedFields(req); err != nil {
		return err
	}
	if req.CapiInfraMachineTemplate == nil {
		return fmt.Errorf("capiInfraMachineTemplate is required for CAPI clusters")
	}
	if isEmptyManifestObject(req.CapiInfraMachineTemplate) {
		return fmt.Errorf("capiInfraMachineTemplate cannot be empty for CAPI clusters")
	}
	if err := validateCAPISpireAttestationMethod(string(req.CapiSpireAttestationMethod)); err != nil {
		return err
	}
	if err := validateCAPIInfrastructureManifest(req.CapiInfraMachineTemplate); err != nil {
		return fmt.Errorf("invalid capiInfraMachineTemplate: %w", err)
	}
	if err := validateCloudInitFiles(req.CapiBootstrapFiles); err != nil {
		return err
	}
	return nil
}

func (s *nodePoolService) validateCreateForgeNodePoolRequest(
	ctx context.Context,
	cluster db.Cluster,
	req *api.CreateNodePoolRequest,
) error {
	if err := validateForgeDiskSelectionStrategy(req.DiskSelectionStrategy); err != nil {
		return err
	}
	if err := validateCreateForgeRestrictedFields(req); err != nil {
		return err
	}

	if req.InstanceTypeID == uuid.Nil {
		return fmt.Errorf("instanceTypeID is required when creating a Forge node pool")
	}

	if req.OSProfile == "" {
		return fmt.Errorf("osProfile is required when creating a Forge node pool")
	}
	if req.ConfigProfile == "" {
		return fmt.Errorf("configProfile is required when creating a Forge node pool")
	}

	forgeSite, err := s.forgeProvider.GetSiteByID(ctx, cluster.OrgID, cluster.SiteID.String())
	if err != nil {
		return fmt.Errorf("failed to get forge site: %w", err)
	}

	capabilities := forgeSite.GetCapabilities()
	isNativeNetworkingEnabled := capabilities.GetNativeNetworking()

	hasSubnetIDs := len(req.SubnetIDs) > 0
	hasVpcPrefixIDs := len(req.VpcPrefixIDs) > 0

	if isNativeNetworkingEnabled {
		if !hasVpcPrefixIDs {
			return fmt.Errorf("vpcPrefixIDs is required when the site has native networking enabled")
		}
		if hasSubnetIDs {
			return fmt.Errorf("subnetIDs should not be provided when the site has native networking enabled")
		}
	} else {
		if !hasSubnetIDs {
			return fmt.Errorf("subnetIDs is required when the site does not have native networking enabled")
		}
		if hasVpcPrefixIDs {
			return fmt.Errorf("vpcPrefixIDs should not be provided when the site does not have native networking enabled")
		}
	}

	return nil
}

func (s *nodePoolService) GetNodePool(ctx context.Context, req api.GetNodePoolRequestObject) (api.GetNodePoolResponseObject, error) {
	dbNodePool, err := s.db.GetNodePool(ctx, req.NodePoolID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return api.GetNodePool404Response{}, nil
		}
		return nil, fmt.Errorf("failed to get node pool: %w", err)
	}

	return api.GetNodePool200JSONResponse(apiNodePoolFromDB(&dbNodePool)), nil
}

func (s *nodePoolService) DeleteNodePool(
	ctx context.Context,
	req api.DeleteNodePoolRequestObject,
) (api.DeleteNodePoolResponseObject, error) {
	err := s.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		dbNodePool, err := tx.GetNodePool(ctx, req.NodePoolID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return err
			}
			return fmt.Errorf("failed to get node pool: %w", err)
		}

		dbNodePool.Conditions.Deleting = db.NewCondition(true, "Node pool deletion in progress")

		if _, err := tx.UpdateNodePool(ctx, &dbNodePool); err != nil {
			return fmt.Errorf("failed to update node pool: %w", err)
		}

		workflowID := constants.TeardownNodePoolWorkflowPrefix + dbNodePool.ID.String()
		_, err = tx.CreateWorkflowOutboxEntry(ctx, "TeardownNodePool", workflowID, map[string]interface{}{
			"nodePoolID": dbNodePool.ID,
		})
		if err != nil {
			return fmt.Errorf("failed to create workflow outbox entry: %w", err)
		}

		return nil
	})
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return api.DeleteNodePool404Response{}, nil
		}
		return nil, err
	}

	return api.DeleteNodePool202Response{}, nil
}

func (s *nodePoolService) UpdateNodePool(
	ctx context.Context,
	req api.UpdateNodePoolRequestObject,
) (api.UpdateNodePoolResponseObject, error) {
	var updatedNodePool db.NodePool
	err := s.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		dbNodePool, err := tx.GetNodePoolForUpdate(ctx, req.NodePoolID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return err
			}
			return fmt.Errorf("failed to get node pool: %w", err)
		}

		if dbNodePool.Conditions.Deleting.Status == db.ConditionStatusTrue {
			return errNodePoolDeleting
		}

		cluster, err := tx.GetClusterForUpdate(ctx, dbNodePool.ClusterID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return errClusterNotFound
			}
			return fmt.Errorf("failed to get cluster: %w", err)
		}
		capacityCanIncrease := req.Body != nil &&
			(req.Body.DesiredNodeCount != nil || req.Body.MaxSurge != nil)
		var nodePools []db.NodePool
		previousRequiredNodeBlocks := 0
		if capacityCanIncrease {
			clusterID := cluster.ID
			nodePools, err = tx.ListNodePools(ctx, db.NodePoolFilters{ClusterID: &clusterID})
			if err != nil {
				return fmt.Errorf("failed to list node pools for pod CIDR capacity validation: %w", err)
			}
			previousRequiredNodeBlocks = requiredPodCIDRNodeBlocks(nodePools, cluster.ControlPlaneConfig.MaxSurge)
		}

		if err := s.validateUpdateNodePoolRequest(ctx, cluster, req.Body); err != nil {
			return &badRequestError{message: err.Error()}
		}

		// Snapshot the pre-update config so we can emit an audit record below. Safe as a shallow
		// copy: applyUpdateNodePoolRequest reassigns slice fields rather than mutating in place.
		oldNodePool := dbNodePool

		if err := applyUpdateNodePoolRequest(&dbNodePool, req.Body); err != nil {
			return err
		}
		if err := validateUpdatedNodePool(cluster, &dbNodePool); err != nil {
			return err
		}
		if capacityCanIncrease {
			found := false
			for i := range nodePools {
				if nodePools[i].ID == dbNodePool.ID {
					nodePools[i] = dbNodePool
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("node pool %s is missing from cluster %s", dbNodePool.ID, cluster.ID)
			}
			if requiredPodCIDRNodeBlocks(nodePools, cluster.ControlPlaneConfig.MaxSurge) > previousRequiredNodeBlocks {
				if err := validateStoredPodCIDRCapacity(cluster, nodePools); err != nil {
					return newBadRequestErrorf("%s", err)
				}
			}
		}
		if !clusterIsCAPIBacked(cluster) &&
			req.Body != nil &&
			(req.Body.SubnetIDs != nil || req.Body.VpcPrefixIDs != nil) {
			network, err := resolveStoredClusterNetwork(cluster)
			if err != nil {
				return fmt.Errorf("invalid stored cluster network: %w", err)
			}
			if err := validateForgeNetworkIDs(
				ctx,
				s.forgeProvider,
				cluster.OrgID,
				cluster.SiteID,
				dbNodePool.SubnetIDs,
				dbNodePool.VpcPrefixIDs,
				network,
			); err != nil {
				return err
			}
		}

		updatedNodePool, err = tx.UpdateNodePool(ctx, &dbNodePool)
		if err != nil {
			return fmt.Errorf("failed to update node pool: %w", err)
		}

		// Auto-provision-disabled pools (e.g. L0 platform-managed control planes) never have their
		// config changes turned into a new NodeSet by the NodePoolController, so the reconciler's
		// audit path is skipped. Emit the audit record directly here from the before/after config.
		// Auto-provisioned pools are intentionally excluded: they get their audit record when the
		// reconciler creates the resulting NodeSet, so emitting here too would double-record.
		if updatedNodePool.AutoProvisionDisabled {
			if input, changed := nodePoolConfigChangeInput(oldNodePool, updatedNodePool); changed {
				auditID := constants.RecordNodePoolConfigChangeWorkflowPrefix +
					updatedNodePool.ID.String() + "-" + input.NewNodeSetID.String()
				if _, err := tx.CreateWorkflowOutboxEntry(
					ctx, constants.RecordNodePoolConfigChangeWorkflowType, auditID, input,
				); err != nil {
					return fmt.Errorf("failed to create config change audit entry: %w", err)
				}
			}
		}

		return nil
	})
	if err != nil {
		var badRequestErr *badRequestError
		if errors.As(err, &badRequestErr) {
			return api.UpdateNodePool400JSONResponse{Message: badRequestErr.Error()}, nil
		}
		if errors.Is(err, db.ErrNotFound) || errors.Is(err, errClusterNotFound) {
			return api.UpdateNodePool404Response{}, nil
		}
		return nil, err
	}

	return api.UpdateNodePool200JSONResponse(apiNodePoolFromDB(&updatedNodePool)), nil
}

func applyUpdateNodePoolRequest(dbNodePool *db.NodePool, req *api.UpdateNodePoolRequest) error {
	if req == nil {
		return nil
	}

	if req.SubnetIDs != nil {
		dbNodePool.SubnetIDs = uuidSliceDedupe(req.SubnetIDs)
	}
	if req.VpcPrefixIDs != nil {
		dbNodePool.VpcPrefixIDs = uuidSliceDedupe(req.VpcPrefixIDs)
	}

	if req.AutoProvisionDisabled != nil {
		dbNodePool.AutoProvisionDisabled = *req.AutoProvisionDisabled
	}
	if req.AutoModeDisabled != nil {
		dbNodePool.AutoModeDisabled = *req.AutoModeDisabled
	}
	if req.ProvisioningMonitorDisabled != nil {
		dbNodePool.ProvisioningMonitorDisabled = *req.ProvisioningMonitorDisabled
	}

	if req.ConfigProfile != nil {
		dbNodePool.ConfigProfile = *req.ConfigProfile
	}
	if req.CapiInfraMachineTemplate != nil {
		capiInfraMachineTemplate, err := rawManifestFromObject(*req.CapiInfraMachineTemplate)
		if err != nil {
			return newBadRequestErrorf("failed to convert capiInfraMachineTemplate: %v", err)
		}
		dbNodePool.CapiInfraMachineTemplate = capiInfraMachineTemplate
	}

	if req.Description != nil {
		dbNodePool.Description = req.Description
	}

	if req.DesiredNodeCount != nil {
		dbNodePool.DesiredCount = *req.DesiredNodeCount
	}

	if req.OSProfile != nil {
		dbNodePool.OSProfile = *req.OSProfile
	}
	if req.CapiBootstrapFiles != nil {
		dbNodePool.CapiBootstrapFiles = dbCloudInitFilesFromAPI(req.CapiBootstrapFiles)
	}
	if req.CapiPreKubeadmCommands != nil {
		dbNodePool.CapiPreKubeadmCommands = append([]string(nil), req.CapiPreKubeadmCommands...)
	}

	if req.SystemReservedMemory != nil {
		dbNodePool.SystemReservedMemory = *req.SystemReservedMemory
	}
	if req.RecycleBudget != nil {
		dbNodePool.RecycleBudget = *req.RecycleBudget
	}
	if req.DiskSelectionStrategy != nil {
		dbNodePool.DiskSelectionStrategy = db.DiskSelectionStrategy(*req.DiskSelectionStrategy)
	}
	if req.MaxSurge != nil {
		dbNodePool.MaxSurge = *req.MaxSurge
	}
	if req.TeleportEnabled != nil {
		dbNodePool.TeleportEnabled = *req.TeleportEnabled
	}
	if req.CapiSpireAttestationMethod != nil {
		dbNodePool.CapiSpireAttestationMethod = db.SpireAttestationMethod(*req.CapiSpireAttestationMethod)
	}
	if req.KubernetesVersion != nil {
		dbNodePool.KubernetesVersion = *req.KubernetesVersion
	}

	return nil
}

func validateUpdatedNodePool(cluster db.Cluster, dbNodePool *db.NodePool) error {
	if dbNodePool.ControlPlane && dbNodePool.MaxSurge > 1 {
		return newBadRequestErrorf("maxSurge should be max 1 for control plane nodepools")
	}

	if clusterIsCAPIBacked(cluster) {
		return validateUpdatedCAPINodePool(dbNodePool)
	}

	return validateUpdatedForgeNodePool(dbNodePool)
}

func validateUpdatedCAPINodePool(dbNodePool *db.NodePool) error {
	if dbNodePool.DiskSelectionStrategy != "" {
		return newBadRequestErrorf("%s", unsupportedNodePoolFieldError("diskSelectionStrategy", "CAPI clusters"))
	}
	if capi.IsEmptyManifest(dbNodePool.CapiInfraMachineTemplate) {
		return newBadRequestErrorf("capiInfraMachineTemplate is required for CAPI clusters")
	}
	capiInfraMachineTemplate, err := objectFromRawManifest(dbNodePool.CapiInfraMachineTemplate)
	if err != nil {
		return newBadRequestErrorf("failed to parse capiInfraMachineTemplate: %v", err)
	}
	if err := validateCAPIInfrastructureManifest(capiInfraMachineTemplate); err != nil {
		return newBadRequestErrorf("invalid capiInfraMachineTemplate: %s", err)
	}
	if err := validateCAPISpireAttestationMethod(string(dbNodePool.CapiSpireAttestationMethod)); err != nil {
		return newBadRequestErrorf("%s", err.Error())
	}
	for _, file := range dbNodePool.CapiBootstrapFiles {
		switch file.Path {
		case constants.CAPISpireAgentConfigPath, constants.CAPISpireTrustBundlePath:
			return newBadRequestErrorf("file path %q is managed by CWE and must not be provided", file.Path)
		}
	}

	return nil
}

func validateUpdatedForgeNodePool(dbNodePool *db.NodePool) error {
	if err := validateForgeDiskSelectionStrategy(api.DiskSelectionStrategyCreate(dbNodePool.DiskSelectionStrategy)); err != nil {
		return newBadRequestErrorf("%s", err.Error())
	}
	if dbNodePool.CapiSpireAttestationMethod != "" {
		return newBadRequestErrorf("capiSpireAttestationMethod is not supported for Forge clusters")
	}
	return nil
}

// validateUpdateNodePoolRequest validates cluster-type-specific update fields before the request is applied.
func (s *nodePoolService) validateUpdateNodePoolRequest(ctx context.Context, cluster db.Cluster, req *api.UpdateNodePoolRequest) error {
	if clusterIsCAPIBacked(cluster) {
		return validateUpdateCAPINodePoolRequest(req)
	}

	return s.validateUpdateForgeNodePoolRequest(ctx, cluster, req)
}

func validateUpdateCAPINodePoolRequest(req *api.UpdateNodePoolRequest) error {
	if err := validateUpdateCAPIRestrictedFields(req); err != nil {
		return err
	}
	if req.CapiInfraMachineTemplate != nil {
		if isEmptyManifestObject(*req.CapiInfraMachineTemplate) {
			return fmt.Errorf("capiInfraMachineTemplate cannot be empty for CAPI clusters")
		}
		if err := validateCAPIInfrastructureManifest(*req.CapiInfraMachineTemplate); err != nil {
			return fmt.Errorf("invalid capiInfraMachineTemplate: %w", err)
		}
	}
	if req.CapiBootstrapFiles != nil {
		if err := validateCloudInitFiles(req.CapiBootstrapFiles); err != nil {
			return err
		}
	}
	return nil
}

func (s *nodePoolService) validateUpdateForgeNodePoolRequest(
	ctx context.Context,
	cluster db.Cluster,
	req *api.UpdateNodePoolRequest,
) error {
	if req.DiskSelectionStrategy != nil {
		if err := validateForgeDiskSelectionStrategy(api.DiskSelectionStrategyCreate(*req.DiskSelectionStrategy)); err != nil {
			return err
		}
	}
	if err := validateUpdateForgeRestrictedFields(req); err != nil {
		return err
	}

	if !nonEmptySubnetIDs(req) && !nonEmptyVpcPrefixIDs(req) {
		return nil
	}
	if nonEmptySubnetIDs(req) && nonEmptyVpcPrefixIDs(req) {
		return fmt.Errorf("cannot provide both subnetIDs and vpcPrefixIDs")
	}

	forgeSite, err := s.forgeProvider.GetSiteByID(ctx, cluster.OrgID, cluster.SiteID.String())
	if err != nil {
		return fmt.Errorf("failed to get forge site: %w", err)
	}

	capabilities := forgeSite.GetCapabilities()
	isNativeNetworkingEnabled := capabilities.GetNativeNetworking()

	if nonEmptySubnetIDs(req) && isNativeNetworkingEnabled {
		return fmt.Errorf("subnetIDs should not be provided when the site has native networking enabled")
	}
	if nonEmptyVpcPrefixIDs(req) && !isNativeNetworkingEnabled {
		return fmt.Errorf("vpcPrefixIDs should not be provided when the site does not have native networking enabled")
	}

	return nil
}

func nonEmptySubnetIDs(req *api.UpdateNodePoolRequest) bool {
	return req != nil && req.SubnetIDs != nil && len(req.SubnetIDs) > 0
}

func nonEmptyVpcPrefixIDs(req *api.UpdateNodePoolRequest) bool {
	return req != nil && req.VpcPrefixIDs != nil && len(req.VpcPrefixIDs) > 0
}

// uuidSliceDedupe returns a copy of s with duplicate UUIDs removed (order preserved by first occurrence).
func uuidSliceDedupe(s []uuid.UUID) []uuid.UUID {
	seen := make(map[uuid.UUID]struct{})
	out := make([]uuid.UUID, 0, len(s))
	for _, id := range s {
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	return out
}

func apiNodePoolFromDB(dbNodePool *db.NodePool) api.NodePool {
	var maxSurge *int
	if dbNodePool.MaxSurge > 0 {
		maxSurge = &dbNodePool.MaxSurge
	}
	capiInfraMachineTemplate, err := objectFromRawManifest(dbNodePool.CapiInfraMachineTemplate)
	if err != nil {
		capiInfraMachineTemplate = nil
	}
	var capiInfraMachineTemplatePtr *map[string]interface{}
	if len(capiInfraMachineTemplate) > 0 {
		capiInfraMachineTemplatePtr = &capiInfraMachineTemplate
	}

	return api.NodePool{
		ID:                          dbNodePool.ID,
		ClusterID:                   dbNodePool.ClusterID,
		Name:                        dbNodePool.Name,
		NodeNamePrefix:              dbNodePool.NodeNamePrefix,
		Description:                 dbNodePool.Description,
		SubnetIDs:                   dbNodePool.SubnetIDs,
		VpcPrefixIDs:                dbNodePool.VpcPrefixIDs,
		InstanceTypeID:              dbNodePool.InstanceTypeID,
		InfinibandPartitionID:       dbNodePool.InfinibandPartitionID,
		NvLinkLogicalPartitionID:    dbNodePool.NvLinkLogicalPartitionID,
		OSProfile:                   dbNodePool.OSProfile,
		ConfigProfile:               dbNodePool.ConfigProfile,
		KubernetesVersion:           dbNodePool.KubernetesVersion,
		SystemReservedMemory:        &dbNodePool.SystemReservedMemory,
		ControlPlane:                dbNodePool.ControlPlane,
		NodeLabels:                  dbNodePool.NodeLabels,
		SSHKeyGroupIDs:              dbNodePool.SSHKeyGroupIDs,
		DesiredNodeCount:            dbNodePool.DesiredCount,
		AutoProvisionDisabled:       dbNodePool.AutoProvisionDisabled,
		AutoModeDisabled:            dbNodePool.AutoModeDisabled,
		ProvisioningMonitorDisabled: dbNodePool.ProvisioningMonitorDisabled,
		RecycleBudget:               dbNodePool.RecycleBudget,
		MaxSurge:                    maxSurge,
		CurrentNodeSetID:            dbNodePool.CurrentNodeSetID,
		TeleportEnabled:             &dbNodePool.TeleportEnabled,
		DiskSelectionStrategy:       api.DiskSelectionStrategy(dbNodePool.DiskSelectionStrategy),

		CapiInfraMachineTemplate:   capiInfraMachineTemplatePtr,
		CapiSpireAttestationMethod: apiSpireAttestationMethodFromDB(dbNodePool.CapiSpireAttestationMethod),
		CapiBootstrapFiles:         apiCloudInitFilesFromDB(dbNodePool.CapiBootstrapFiles),
		CapiPreKubeadmCommands:     stringSlicePointer(dbNodePool.CapiPreKubeadmCommands),

		Conditions: api.NodePoolConditions{
			Provisioning: apiConditionFromDB(dbNodePool.Conditions.Provisioning),
			Provisioned:  apiConditionFromDB(dbNodePool.Conditions.Provisioned),
			Deleting:     apiConditionFromDB(dbNodePool.Conditions.Deleting),
		},
	}
}

func dbNodePoolFromAPI(req *api.CreateNodePoolRequest, kubernetesVersion string) (db.NodePool, error) {
	nodeLabels := make([]string, 0)
	if req.NodeLabels != nil {
		nodeLabels = *req.NodeLabels
	}
	sshKeyGroupIDs := make([]uuid.UUID, 0)
	if req.SSHKeyGroupIDs != nil {
		sshKeyGroupIDs = *req.SSHKeyGroupIDs
	}
	subnetIDs := make([]uuid.UUID, 0)
	if req.SubnetIDs != nil {
		subnetIDs = req.SubnetIDs
	}
	vpcPrefixIDs := make([]uuid.UUID, 0)
	if req.VpcPrefixIDs != nil {
		vpcPrefixIDs = req.VpcPrefixIDs
	}

	nodePoolID := uuid.New()

	autoProvisionDisabled := req.AutoProvisionDisabled != nil && *req.AutoProvisionDisabled
	autoModeDisabled := req.AutoModeDisabled != nil && *req.AutoModeDisabled
	provisioningMonitorDisabled := req.ProvisioningMonitorDisabled != nil && *req.ProvisioningMonitorDisabled
	recycleBudget := 0
	if req.RecycleBudget != nil {
		recycleBudget = *req.RecycleBudget
	}

	capiInfraMachineTemplate, err := rawManifestFromObject(req.CapiInfraMachineTemplate)
	if err != nil {
		return db.NodePool{}, err
	}

	var teleportEnabled bool
	if req.TeleportEnabled != nil {
		teleportEnabled = *req.TeleportEnabled
	}

	capiSpireAttestationMethod := db.SpireAttestationMethod("")
	if req.CapiSpireAttestationMethod != "" {
		capiSpireAttestationMethod = db.SpireAttestationMethod(req.CapiSpireAttestationMethod)
	}

	return db.NodePool{
		ID:                       nodePoolID,
		ClusterID:                req.ClusterID,
		Name:                     req.Name,
		NodeNamePrefix:           req.NodeNamePrefix,
		SubnetIDs:                subnetIDs,
		VpcPrefixIDs:             vpcPrefixIDs,
		InstanceTypeID:           req.InstanceTypeID,
		Description:              req.Description,
		InfinibandPartitionID:    req.InfinibandPartitionID,
		NvLinkLogicalPartitionID: req.NvLinkLogicalPartitionID,
		SSHKeyGroupIDs:           sshKeyGroupIDs,
		OSProfile:                req.OSProfile,
		ConfigProfile:            req.ConfigProfile,
		KubernetesVersion:        kubernetesVersion,
		SystemReservedMemory:     req.SystemReservedMemory,
		ControlPlane:             req.ControlPlane,
		NodeLabels:               nodeLabels,
		Conditions: db.NodePoolConditions{
			Provisioning: db.NewCondition(true, "Starting provisioning"),
			Provisioned:  db.NewCondition(false, "Not yet provisioned"),
			Deleting:     db.NewCondition(false, "Node pool is not being deleted"),
		},
		DesiredCount:                req.DesiredNodeCount,
		AutoProvisionDisabled:       autoProvisionDisabled,
		AutoModeDisabled:            autoModeDisabled,
		ProvisioningMonitorDisabled: provisioningMonitorDisabled,
		RecycleBudget:               recycleBudget,
		TeleportEnabled:             teleportEnabled,
		DiskSelectionStrategy:       db.DiskSelectionStrategy(req.DiskSelectionStrategy),

		CapiInfraMachineTemplate:   capiInfraMachineTemplate,
		CapiSpireAttestationMethod: capiSpireAttestationMethod,
		CapiBootstrapFiles:         dbCloudInitFilesFromAPI(req.CapiBootstrapFiles),
		CapiPreKubeadmCommands:     append([]string(nil), req.CapiPreKubeadmCommands...),
	}, nil
}

func apiSpireAttestationMethodFromDB(method db.SpireAttestationMethod) *api.SpireAttestationMethod {
	if method == "" {
		return nil
	}
	apiMethod := api.SpireAttestationMethod(method)
	return &apiMethod
}
