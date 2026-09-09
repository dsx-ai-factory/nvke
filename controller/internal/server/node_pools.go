// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/NVIDIA/nke/controller/api"
	"github.com/NVIDIA/nke/controller/internal/capi"
	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/naming"
	"github.com/NVIDIA/nke/controller/internal/nodes"
)

var (
	errClusterNotFound                = nodes.ErrClusterNotFound
	errClusterDeleting                = nodes.ErrClusterDeleting
	errNodePoolDeleting               = nodes.ErrNodePoolDeleting
	errNodeDeleting                   = nodes.ErrNodeDeleting
	errControlPlaneNodePoolNotAllowed = errors.New("control-plane node pools are not allowed when control-plane controller provisioning is enabled; configure the control plane via the cluster-level controlPlaneConfig")
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

type nodePoolService struct {
	logger      *slog.Logger
	db          db.DB
	teleportCfg config.TeleportConfig
	// useControlPlaneController mirrors the CWE feature flag
	// PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER. When true,
	// control-plane node pools are rejected on create/update because the control
	// plane is provisioned from the cluster-level controlPlaneConfig instead.
	useControlPlaneController bool
}

func NewNodePoolService(logger *slog.Logger, database db.DB, teleportCfg config.TeleportConfig, useControlPlaneController bool) *nodePoolService {
	return &nodePoolService{
		logger:                    logger,
		db:                        database,
		teleportCfg:               teleportCfg,
		useControlPlaneController: useControlPlaneController,
	}
}

// applyNodePoolDefaults fills in server-side defaults for omitted node pool fields
// based on the pool's role (control plane vs worker) and infrastructure type.
func (s *nodePoolService) applyNodePoolDefaults(req *api.CreateNodePoolRequest, cluster db.Cluster) {
	if req.TeleportEnabled == nil {
		v := s.teleportCfg.NodePoolTeleportDefault(req.ControlPlane)
		req.TeleportEnabled = &v
	}
	// The stored pool is built from the nested payload, so the default has to land
	// there; validation has already required nico for a Forge pool.
	if !clusterIsCAPIBacked(cluster) && req.Nico != nil && req.Nico.DiskSelectionStrategy == "" {
		req.Nico.DiskSelectionStrategy = api.DiskSelectionStrategyCreateSmallest
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

	if s.useControlPlaneController && req.Body.ControlPlane {
		return api.CreateNodePool400JSONResponse{Message: errControlPlaneNodePoolNotAllowed.Error()}, nil
	}

	if err := s.validateCreateNodePoolRequest(cluster, req.Body); err != nil {
		return api.CreateNodePool400JSONResponse{Message: err.Error()}, nil
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
		if err := validateStoredPodCIDRCapacity(cluster, podCIDRNodePoolsWithClusterCP(s.useControlPlaneController, cluster, nodePools)); err != nil {
			return newBadRequestErrorf("%s", err)
		}

		createdNodePool, err = tx.CreateNodePool(ctx, &dbNodePool)
		if err != nil {
			return fmt.Errorf("failed to create node pool: %w", err)
		}

		workflowID := constants.ProvisionNodePoolWorkflowPrefix + createdNodePool.ID.String()
		_, err = tx.CreateWorkflowOutboxEntry(ctx, "ProvisionNodePool", workflowID, map[string]interface{}{
			nodePoolIDConst: createdNodePool.ID,
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
		return fmt.Errorf("invalid nico.diskSelectionStrategy: %q", s)
	}

	return nil
}

func validateNicoNetworkSelection(subnetIDs, vpcPrefixIDs []uuid.UUID, fieldPrefix string) error {
	hasSubnetIDs := len(subnetIDs) > 0
	hasVpcPrefixIDs := len(vpcPrefixIDs) > 0
	if hasSubnetIDs == hasVpcPrefixIDs {
		return fmt.Errorf("exactly one of %ssubnetIDs or %svpcPrefixIDs must be provided", fieldPrefix, fieldPrefix)
	}
	return nil
}

func validateCAPISpireAttestationMethod(method string) error {
	switch method {
	case "":
		return fmt.Errorf("infra.spireAttestationMethod is required")
	case string(db.SpireAttestationMethodNone), string(db.SpireAttestationMethodTPM), string(db.SpireAttestationMethodJoinToken):
		return nil
	default:
		return fmt.Errorf("invalid infra.spireAttestationMethod: %q", method)
	}
}

func clusterIsCAPIBacked(cluster db.Cluster) bool {
	return cluster.ProviderType == db.ProviderTypeCAPI
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

func (s *nodePoolService) validateCreateNodePoolRequest(cluster db.Cluster, req *api.CreateNodePoolRequest) error {
	if err := naming.ValidateNodeNamePrefix(cluster.Name, req.NodeNamePrefix); err != nil {
		return err
	}

	switch cluster.ProviderType {
	case db.ProviderTypeCAPI:
		return validateCreateCAPINodePoolRequest(req)
	case db.ProviderTypeNico:
		return validateCreateNicoNodePoolRequest(req)
	default:
		return fmt.Errorf("unsupported cluster provider")
	}
}

func validateCreateCAPINodePoolRequest(req *api.CreateNodePoolRequest) error {
	if req.Nico != nil {
		return fmt.Errorf("nico is not supported, please provide infra")
	}
	if req.Infra == nil || req.Infra.MachineTemplate == nil {
		return fmt.Errorf("infra.machineTemplate is required")
	}
	if isEmptyManifestObject(req.Infra.MachineTemplate) {
		return fmt.Errorf("infra.machineTemplate cannot be empty")
	}
	if err := validateCAPISpireAttestationMethod(string(req.Infra.SpireAttestationMethod)); err != nil {
		return err
	}
	if err := validateCAPIInfrastructureManifest(req.Infra.MachineTemplate); err != nil {
		return fmt.Errorf("invalid infra.machineTemplate: %w", err)
	}
	if err := validateCloudInitFiles(req.Infra.BootstrapFiles); err != nil {
		return err
	}
	return nil
}

func validateCreateNicoNodePoolRequest(req *api.CreateNodePoolRequest) error {
	if req.Infra != nil {
		return fmt.Errorf("infra is not supported, please provide nico")
	}
	if req.Nico == nil {
		return fmt.Errorf("nico is required when creating a node pool on a nico cluster")
	}
	if err := validateForgeDiskSelectionStrategy(req.Nico.DiskSelectionStrategy); err != nil {
		return err
	}

	if req.Nico.InstanceTypeID == uuid.Nil {
		return fmt.Errorf("nico.instanceTypeID is required when creating a nico node pool")
	}

	if req.Nico.OSProfile == "" {
		return fmt.Errorf("nico.osProfile is required when creating a nico node pool")
	}
	if req.Nico.ConfigProfile == "" {
		return fmt.Errorf("nico.configProfile is required when creating a nico node pool")
	}

	if err := validateNicoNetworkSelection(req.Nico.SubnetIDs, req.Nico.VpcPrefixIDs, "nico."); err != nil {
		return err
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
		return nodes.DeleteNodePoolWithTx(ctx, tx, req.NodePoolID)
	})
	if err != nil {
		if errors.Is(err, errClusterDeleting) || errors.Is(err, errNodePoolDeleting) {
			// Teardown already in progress via cluster delete or a prior pool delete.
			return api.DeleteNodePool202Response{}, nil
		}
		if errors.Is(err, nodes.ErrLastJoinedControlPlaneNodePool) {
			return api.DeleteNodePool400JSONResponse{Message: err.Error()}, nil
		}
		if errors.Is(err, db.ErrNotFound) || errors.Is(err, errClusterNotFound) {
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
		// Re-read the pool without a lock first, solely to learn its cluster, then
		// lock the cluster before the node-pool row. This matches the global
		// cluster-first lock order (see internal/nodes/control_plane_delete.go)
		// so this update cannot deadlock with the live-delete guards or the
		// control-plane reconcile transaction.
		unlockedNodePool, err := tx.GetNodePool(ctx, req.NodePoolID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return err
			}
			return fmt.Errorf("failed to get node pool: %w", err)
		}

		cluster, err := tx.GetClusterForUpdate(ctx, unlockedNodePool.ClusterID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return errClusterNotFound
			}
			return fmt.Errorf("failed to get cluster: %w", err)
		}

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

		if s.useControlPlaneController && dbNodePool.ControlPlane {
			return &badRequestError{message: errControlPlaneNodePoolNotAllowed.Error()}
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
			previousRequiredNodeBlocks = requiredPodCIDRNodeBlocks(podCIDRNodePoolsWithClusterCP(s.useControlPlaneController, cluster, nodePools), cluster.ControlPlaneConfig.MaxSurge)
		}

		if err := s.validateUpdateNodePoolRequest(cluster, req.Body); err != nil {
			return &badRequestError{message: err.Error()}
		}

		// Snapshot the pre-update config so we can emit an audit record below. Safe as a shallow
		// copy: applyUpdateNodePoolRequest reassigns slice fields rather than mutating in place.
		oldNodePool := dbNodePool

		if err := applyUpdateNodePoolRequest(cluster, &dbNodePool, req.Body); err != nil {
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
			augmentedNodePools := podCIDRNodePoolsWithClusterCP(s.useControlPlaneController, cluster, nodePools)
			if requiredPodCIDRNodeBlocks(augmentedNodePools, cluster.ControlPlaneConfig.MaxSurge) > previousRequiredNodeBlocks {
				if err := validateStoredPodCIDRCapacity(cluster, augmentedNodePools); err != nil {
					return newBadRequestErrorf("%s", err)
				}
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

func applyUpdateNodePoolRequest(cluster db.Cluster, dbNodePool *db.NodePool, req *api.UpdateNodePoolRequest) error {
	if req == nil {
		return nil
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

	if req.Description != nil {
		dbNodePool.Description = req.Description
	}

	if req.DesiredNodeCount != nil {
		dbNodePool.DesiredCount = *req.DesiredNodeCount
	}

	if req.SystemReservedMemory != nil {
		dbNodePool.SystemReservedMemory = *req.SystemReservedMemory
	}
	if req.KubeReservedMemory != nil {
		dbNodePool.KubeReservedMemory = *req.KubeReservedMemory
	}
	if req.RecycleBudget != nil {
		dbNodePool.RecycleBudget = *req.RecycleBudget
	}
	if req.MaxSurge != nil {
		dbNodePool.MaxSurge = *req.MaxSurge
	}
	if req.TeleportEnabled != nil {
		dbNodePool.TeleportEnabled = *req.TeleportEnabled
	}
	if req.KubernetesVersion != nil {
		dbNodePool.KubernetesVersion = *req.KubernetesVersion
	}

	// The cluster's provider decides which payload this pool carries; the request
	// only supplies the patch. validateUpdateNodePoolRequest has already rejected
	// a provider shape that does not match the cluster.
	switch cluster.ProviderType {
	case db.ProviderTypeNico:
		if req.Nico == nil {
			return nil
		}
		nico, err := dbNodePool.NicoProvider()
		if err != nil {
			return err
		}
		if req.Nico.SubnetIDs != nil {
			nico.SubnetIDs = uuidSliceDedupe(req.Nico.SubnetIDs)
		}
		if req.Nico.VpcPrefixIDs != nil {
			nico.VpcPrefixIDs = uuidSliceDedupe(req.Nico.VpcPrefixIDs)
		}
		if req.Nico.OSProfile != nil {
			nico.OSProfile = *req.Nico.OSProfile
		}
		if req.Nico.ConfigProfile != nil {
			nico.ConfigProfile = *req.Nico.ConfigProfile
		}
		if req.Nico.DiskSelectionStrategy != nil {
			nico.DiskSelectionStrategy = db.DiskSelectionStrategy(*req.Nico.DiskSelectionStrategy)
		}
		dbNodePool.SetNico(nico)

	case db.ProviderTypeCAPI:
		if req.Infra == nil {
			return nil
		}
		capiProvider, err := dbNodePool.CAPIProvider()
		if err != nil {
			return err
		}
		if req.Infra.MachineTemplate != nil {
			capiInfraMachineTemplate, err := rawManifestFromObject(req.Infra.MachineTemplate)
			if err != nil {
				return newBadRequestErrorf("failed to convert infra.machineTemplate: %v", err)
			}
			capiProvider.InfraMachineTemplate = capiInfraMachineTemplate
		}
		if req.Infra.BootstrapFiles != nil {
			capiProvider.BootstrapFiles = dbCloudInitFilesFromAPI(req.Infra.BootstrapFiles)
		}
		if req.Infra.PreKubeadmCommands != nil {
			capiProvider.PreKubeadmCommands = append([]string(nil), req.Infra.PreKubeadmCommands...)
		}
		if req.Infra.SpireAttestationMethod != nil {
			capiProvider.SpireAttestationMethod = db.SpireAttestationMethod(*req.Infra.SpireAttestationMethod)
		}
		dbNodePool.SetCapi(capiProvider)
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
		return newBadRequestErrorf("nico.diskSelectionStrategy is not supported")
	}
	if capi.IsEmptyManifest(dbNodePool.CapiInfraMachineTemplate) {
		return newBadRequestErrorf("infra.machineTemplate is required")
	}
	capiInfraMachineTemplate, err := objectFromRawManifest(dbNodePool.CapiInfraMachineTemplate)
	if err != nil {
		return newBadRequestErrorf("failed to parse infra.machineTemplate: %v", err)
	}
	if err := validateCAPIInfrastructureManifest(capiInfraMachineTemplate); err != nil {
		return newBadRequestErrorf("invalid infra.machineTemplate: %s", err)
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
		return newBadRequestErrorf("infra.spireAttestationMethod is not supported on a nico node pool")
	}
	return nil
}

// validateUpdateNodePoolRequest validates cluster-type-specific update fields before the request is applied.
func (s *nodePoolService) validateUpdateNodePoolRequest(cluster db.Cluster, req *api.UpdateNodePoolRequest) error {
	switch cluster.ProviderType {
	case db.ProviderTypeCAPI:
		return validateUpdateCAPINodePoolRequest(req)
	case db.ProviderTypeNico:
		return validateUpdateNicoNodePoolRequest(req)
	default:
		return fmt.Errorf("unsupported cluster provider")
	}
}

func validateUpdateCAPINodePoolRequest(req *api.UpdateNodePoolRequest) error {
	if req.Nico != nil {
		return fmt.Errorf("nico is not supported, please use infra")
	}
	if req.Infra != nil && req.Infra.MachineTemplate != nil {
		if isEmptyManifestObject(req.Infra.MachineTemplate) {
			return fmt.Errorf("infra.machineTemplate cannot be empty")
		}
		if err := validateCAPIInfrastructureManifest(req.Infra.MachineTemplate); err != nil {
			return fmt.Errorf("invalid infra.machineTemplate: %w", err)
		}
	}
	if req.Infra != nil && req.Infra.BootstrapFiles != nil {
		if err := validateCloudInitFiles(req.Infra.BootstrapFiles); err != nil {
			return err
		}
	}
	return nil
}

func validateUpdateNicoNodePoolRequest(req *api.UpdateNodePoolRequest) error {
	if req.Infra != nil {
		return fmt.Errorf("infra is not supported, please use nico")
	}
	if req.Nico != nil && req.Nico.DiskSelectionStrategy != nil {
		if err := validateForgeDiskSelectionStrategy(api.DiskSelectionStrategyCreate(*req.Nico.DiskSelectionStrategy)); err != nil {
			return err
		}
	}
	// A profile can be replaced but not cleared. Rejecting the empty value here
	// rather than after the merge leaves a pool whose stored profile is already
	// empty patchable in every other respect.
	if req.Nico != nil {
		if req.Nico.OSProfile != nil && *req.Nico.OSProfile == "" {
			return fmt.Errorf("nico.osProfile cannot be cleared for a nico node pool")
		}
		if req.Nico.ConfigProfile != nil && *req.Nico.ConfigProfile == "" {
			return fmt.Errorf("nico.configProfile cannot be cleared for a nico node pool")
		}
	}

	if !nonEmptySubnetIDs(req) && !nonEmptyVpcPrefixIDs(req) {
		return nil
	}
	if nonEmptySubnetIDs(req) && nonEmptyVpcPrefixIDs(req) {
		return fmt.Errorf("cannot provide both nico.subnetIDs and nico.vpcPrefixIDs")
	}

	return nil
}

func nonEmptySubnetIDs(req *api.UpdateNodePoolRequest) bool {
	return req != nil && req.Nico != nil && req.Nico.SubnetIDs != nil && len(req.Nico.SubnetIDs) > 0
}

func nonEmptyVpcPrefixIDs(req *api.UpdateNodePoolRequest) bool {
	return req != nil && req.Nico != nil && req.Nico.VpcPrefixIDs != nil && len(req.Nico.VpcPrefixIDs) > 0
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
	var kubeReservedMemory *string
	if dbNodePool.KubeReservedMemory != "" {
		kubeReservedMemory = &dbNodePool.KubeReservedMemory
	}
	return api.NodePool{
		ID:                          dbNodePool.ID,
		ClusterID:                   dbNodePool.ClusterID,
		Name:                        dbNodePool.Name,
		NodeNamePrefix:              dbNodePool.NodeNamePrefix,
		Description:                 dbNodePool.Description,
		KubernetesVersion:           dbNodePool.KubernetesVersion,
		SystemReservedMemory:        &dbNodePool.SystemReservedMemory,
		KubeReservedMemory:          kubeReservedMemory,
		ControlPlane:                dbNodePool.ControlPlane,
		NodeLabels:                  dbNodePool.NodeLabels,
		DesiredNodeCount:            dbNodePool.DesiredCount,
		AutoProvisionDisabled:       dbNodePool.AutoProvisionDisabled,
		AutoModeDisabled:            dbNodePool.AutoModeDisabled,
		ProvisioningMonitorDisabled: dbNodePool.ProvisioningMonitorDisabled,
		RecycleBudget:               dbNodePool.RecycleBudget,
		MaxSurge:                    maxSurge,
		CurrentNodeSetID:            dbNodePool.CurrentNodeSetID,
		TeleportEnabled:             &dbNodePool.TeleportEnabled,

		Nico:  apiNicoNodePoolProviderFromDB(dbNodePool.Nico),
		Infra: apiInfraNodePoolProviderFromDB(dbNodePool.Capi),

		Conditions: api.NodePoolConditions{
			Provisioning: apiConditionFromDB(dbNodePool.Conditions.Provisioning),
			Provisioned:  apiConditionFromDB(dbNodePool.Conditions.Provisioned),
			Deleting:     apiConditionFromDB(dbNodePool.Conditions.Deleting),
		},
	}
}

func apiNicoNodePoolProviderFromDB(nicoNodePoolProvider *db.NicoNodePoolProvider) *api.NicoNodePoolProvider {
	if nicoNodePoolProvider == nil {
		return nil
	}
	return &api.NicoNodePoolProvider{
		SubnetIDs:                append([]uuid.UUID(nil), nicoNodePoolProvider.SubnetIDs...),
		VpcPrefixIDs:             append([]uuid.UUID(nil), nicoNodePoolProvider.VpcPrefixIDs...),
		InstanceTypeID:           nicoNodePoolProvider.InstanceTypeID,
		InfinibandPartitionID:    clonePtr(nicoNodePoolProvider.InfinibandPartitionID),
		NvLinkLogicalPartitionID: clonePtr(nicoNodePoolProvider.NvLinkLogicalPartitionID),
		SSHKeyGroupIDs:           cloneUUIDSlicePtr(nicoNodePoolProvider.SSHKeyGroupIDs),
		OSProfile:                nicoNodePoolProvider.OSProfile,
		ConfigProfile:            nicoNodePoolProvider.ConfigProfile,
		DiskSelectionStrategy:    api.DiskSelectionStrategyCreate(nicoNodePoolProvider.DiskSelectionStrategy),
	}
}

func apiInfraNodePoolProviderFromDB(capiNodePoolProvider *db.CAPINodePoolProvider) *api.InfraNodePoolProvider {
	if capiNodePoolProvider == nil {
		return nil
	}
	obj, err := objectFromRawManifest(capiNodePoolProvider.InfraMachineTemplate)
	if err != nil {
		obj = nil
	}
	out := &api.InfraNodePoolProvider{
		MachineTemplate:        obj,
		PreKubeadmCommands:     append([]string(nil), capiNodePoolProvider.PreKubeadmCommands...),
		SpireAttestationMethod: api.SpireAttestationMethodCreate(capiNodePoolProvider.SpireAttestationMethod),
	}
	if apiFiles := apiCloudInitFilesFromDB(capiNodePoolProvider.BootstrapFiles); apiFiles != nil {
		out.BootstrapFiles = *apiFiles
	}
	return out
}

// clonePtr copies the pointee so a normalized request never shares a scalar with
// the caller's payload. A nil pointer stays nil: for an optional update field that
// is the difference between "omitted" and "explicitly set", including set to empty.
func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func cloneUUIDSlicePtr(s []uuid.UUID) *[]uuid.UUID {
	if s == nil {
		return nil
	}
	out := append([]uuid.UUID(nil), s...)
	if out == nil {
		out = []uuid.UUID{}
	}
	return &out
}

func dbNodePoolFromAPI(req *api.CreateNodePoolRequest, kubernetesVersion string) (db.NodePool, error) {
	nodeLabels := make([]string, 0)
	if req.NodeLabels != nil {
		nodeLabels = *req.NodeLabels
	}

	nodePoolID := uuid.New()

	autoProvisionDisabled := req.AutoProvisionDisabled != nil && *req.AutoProvisionDisabled
	autoModeDisabled := req.AutoModeDisabled != nil && *req.AutoModeDisabled
	provisioningMonitorDisabled := req.ProvisioningMonitorDisabled != nil && *req.ProvisioningMonitorDisabled
	recycleBudget := 0
	if req.RecycleBudget != nil {
		recycleBudget = *req.RecycleBudget
	}

	var teleportEnabled bool
	if req.TeleportEnabled != nil {
		teleportEnabled = *req.TeleportEnabled
	}

	nodePool := db.NodePool{
		ID:                   nodePoolID,
		ClusterID:            req.ClusterID,
		Name:                 req.Name,
		NodeNamePrefix:       req.NodeNamePrefix,
		Description:          req.Description,
		KubernetesVersion:    kubernetesVersion,
		SystemReservedMemory: req.SystemReservedMemory,
		KubeReservedMemory:   req.KubeReservedMemory,
		ControlPlane:         req.ControlPlane,
		NodeLabels:           nodeLabels,
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
	}

	switch {
	case req.Nico != nil:
		sshKeyGroupIDs := make([]uuid.UUID, 0)
		if req.Nico.SSHKeyGroupIDs != nil {
			sshKeyGroupIDs = append(sshKeyGroupIDs, *req.Nico.SSHKeyGroupIDs...)
		}
		subnetIDs := make([]uuid.UUID, 0)
		if req.Nico.SubnetIDs != nil {
			subnetIDs = append(subnetIDs, req.Nico.SubnetIDs...)
		}
		vpcPrefixIDs := make([]uuid.UUID, 0)
		if req.Nico.VpcPrefixIDs != nil {
			vpcPrefixIDs = append(vpcPrefixIDs, req.Nico.VpcPrefixIDs...)
		}
		nodePool.SetNico(db.NicoNodePoolProvider{
			SubnetIDs:                subnetIDs,
			VpcPrefixIDs:             vpcPrefixIDs,
			InstanceTypeID:           req.Nico.InstanceTypeID,
			InfinibandPartitionID:    req.Nico.InfinibandPartitionID,
			NvLinkLogicalPartitionID: req.Nico.NvLinkLogicalPartitionID,
			SSHKeyGroupIDs:           sshKeyGroupIDs,
			OSProfile:                req.Nico.OSProfile,
			ConfigProfile:            req.Nico.ConfigProfile,
			DiskSelectionStrategy:    db.DiskSelectionStrategy(req.Nico.DiskSelectionStrategy),
		})
	case req.Infra != nil:
		infraMachineTemplate, err := rawManifestFromObject(req.Infra.MachineTemplate)
		if err != nil {
			return db.NodePool{}, err
		}
		nodePool.SetCapi(db.CAPINodePoolProvider{
			InfraMachineTemplate:   infraMachineTemplate,
			SpireAttestationMethod: db.SpireAttestationMethod(req.Infra.SpireAttestationMethod),
			BootstrapFiles:         dbCloudInitFilesFromAPI(req.Infra.BootstrapFiles),
			PreKubeadmCommands:     append([]string(nil), req.Infra.PreKubeadmCommands...),
		})
	}

	return nodePool, nil
}
