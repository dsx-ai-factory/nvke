// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/nodes"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/workflows"
)

type nodeService struct {
	logger *slog.Logger
	db     db.DB
}

func NewNodeService(logger *slog.Logger, database db.DB) *nodeService {
	return &nodeService{
		logger: logger,
		db:     database,
	}
}

func (s *nodeService) ListNodes(ctx context.Context, req api.ListNodesRequestObject) (api.ListNodesResponseObject, error) {
	filters := db.NodeFilters{
		ClusterID:    req.Params.ClusterID,
		NodePoolID:   req.Params.NodePoolID,
		ClusterName:  req.Params.ClusterName,
		Name:         req.Params.Name,
		ControlPlane: req.Params.ControlPlane,
	}

	dbNodes, err := s.db.ListNodes(ctx, filters)
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}
	rollout, err := controlPlaneRolloutByNode(ctx, s.db, dbNodes)
	if err != nil {
		return nil, err
	}
	apiNodes := make([]api.Node, len(dbNodes))
	for i := range dbNodes {
		apiNodes[i] = apiNodeFromDB(&dbNodes[i])
		apiNodes[i].ControlPlaneRollout = rollout[dbNodes[i].ID]
	}
	return api.ListNodes200JSONResponse{Nodes: apiNodes}, nil
}

func (s *nodeService) GetNode(ctx context.Context, req api.GetNodeRequestObject) (api.GetNodeResponseObject, error) {
	dbNode, err := s.db.GetNode(ctx, req.NodeID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return api.GetNode404Response{}, nil
		}
		return nil, fmt.Errorf("failed to get node: %w", err)
	}

	rollout, err := controlPlaneRolloutByNode(ctx, s.db, []db.Node{dbNode})
	if err != nil {
		return nil, err
	}
	apiNode := apiNodeFromDB(&dbNode)
	apiNode.ControlPlaneRollout = rollout[dbNode.ID]
	return api.GetNode200JSONResponse(apiNode), nil
}

func (s *nodeService) DeleteNode(ctx context.Context, req api.DeleteNodeRequestObject) (api.DeleteNodeResponseObject, error) {
	var machineHealthIssue *workflows.MachineHealthIssue
	if req.Body != nil && req.Body.MachineHealthIssue != nil {
		machineHealthIssue = apiMachineHealthIssueToWorkflow(req.Body.MachineHealthIssue)

		if err := machineHealthIssue.Validate(); err != nil {
			return api.DeleteNode400JSONResponse{
				Message: fmt.Sprintf("Invalid machine health issue: %s", err.Error()),
			}, nil
		}
	}

	var forceUnreachable bool
	if req.Body != nil {
		forceUnreachable = req.Body.ForceUnreachable
	}

	err := s.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		return nodes.DeleteWithTx(ctx, tx, req.NodeID,
			nodes.WithReason("API"),
			nodes.WithAllowCPNode(true),
			nodes.WithMachineHealthIssue(machineHealthIssue),
			nodes.WithForceUnreachable(forceUnreachable),
		)
	})
	if err != nil {
		if errors.Is(err, errClusterDeleting) || errors.Is(err, errNodePoolDeleting) || errors.Is(err, errNodeDeleting) ||
			errors.Is(err, nodes.ErrDeleteAlreadyRequested) {
			// Teardown already in progress via cluster, pool, or a prior node delete,
			// or already accepted and waiting on the control-plane controller.
			return api.DeleteNode202Response{}, nil
		}
		var quorumFloor *nodes.ControlPlaneQuorumFloorError
		if errors.As(err, &quorumFloor) {
			return api.DeleteNode400JSONResponse{Message: quorumFloor.Error()}, nil
		}
		if errors.Is(err, nodes.ErrLastJoinedControlPlaneNode) {
			return api.DeleteNode400JSONResponse{Message: err.Error()}, nil
		}
		if errors.Is(err, nodes.ErrControlPlaneVIPRequired) {
			return api.DeleteNode400JSONResponse{Message: err.Error()}, nil
		}
		if errors.Is(err, db.ErrNotFound) || errors.Is(err, errClusterNotFound) {
			return api.DeleteNode404Response{}, nil
		}
		return nil, err
	}

	return api.DeleteNode202Response{}, nil
}

func apiNodeFromDB(dbNode *db.Node) api.Node {
	nodeConditions := api.NodeConditions{
		Pending:                 apiConditionFromDB(dbNode.Conditions.Pending),
		ProvisioningClaimed:     apiConditionFromDB(dbNode.Conditions.ProvisioningClaimed),
		Provisioning:            apiConditionFromDB(dbNode.Conditions.Provisioning),
		ReadyToJoin:             apiConditionFromDB(dbNode.Conditions.ReadyToJoin),
		Joined:                  apiConditionFromDB(dbNode.Conditions.Joined),
		Provisioned:             apiConditionFromDB(dbNode.Conditions.Provisioned),
		Deleting:                apiConditionFromDB(dbNode.Conditions.Deleting),
		DeleteRequested:         apiConditionFromDB(dbNode.Conditions.DeleteRequested),
		KubernetesReady:         apiConditionFromDB(dbNode.Conditions.KubernetesReady),
		KubernetesUnschedulable: apiConditionFromDB(dbNode.Conditions.KubernetesUnschedulable),
		NodeAgentReady:          apiConditionFromDB(dbNode.Conditions.NodeAgentReady),
		Rebooting:               apiConditionFromDB(dbNode.Conditions.Rebooting),
		MarkedForRecycling:      apiConditionFromDB(dbNode.Conditions.MarkedForRecycling),
		MarkedForRepair:         apiConditionFromDB(dbNode.Conditions.MarkedForRepair),
	}

	// TeleportRegistered is optional — only include when the node has a meaningful Teleport state.
	// Check for both empty string (zero value, condition never set) and "Unknown" (explicitly unknown).
	if dbNode.Conditions.TeleportRegistered.Status != "" &&
		dbNode.Conditions.TeleportRegistered.Status != db.ConditionStatusUnknown {
		cond := apiConditionFromDB(dbNode.Conditions.TeleportRegistered)
		nodeConditions.TeleportRegistered = &cond
	}

	// ControlPlaneRolloutRequired is optional — only include when the node has a
	// meaningful state (control-plane-controller-managed nodes).
	if dbNode.Conditions.ControlPlaneRolloutRequired.Status != "" &&
		dbNode.Conditions.ControlPlaneRolloutRequired.Status != db.ConditionStatusUnknown {
		cond := apiConditionFromDB(dbNode.Conditions.ControlPlaneRolloutRequired)
		nodeConditions.ControlPlaneRolloutRequired = &cond
	}

	node := api.Node{
		ID:           dbNode.ID,
		NodePoolID:   dbNode.NodePoolID,
		NodeSetID:    dbNode.NodeSetID,
		Name:         dbNode.Name,
		IP:           dbNode.IP,
		Nico:         apiNicoNodeProviderFromDB(dbNode.Nico),
		LaunchLayer:  apiLaunchLayerNodeProviderFromDB(dbNode.LaunchLayer),
		ControlPlane: dbNode.ControlPlane,
		Conditions:   nodeConditions,
	}
	if domain := dbNode.EffectiveFailureDomain(); domain != "" {
		node.FailureDomain = &domain
	}
	return node
}

func apiLaunchLayerNodeProviderFromDB(provider *db.LaunchLayerNodeProvider) *api.LaunchLayerNodeProvider {
	if provider == nil {
		return nil
	}
	// An all-zero payload means the node is not LaunchLayer-backed (or not yet
	// stamped) — matches the Nico equivalent's behavior.
	if provider.InstanceID == "" && provider.MachineID == "" && provider.RackID == "" {
		return nil
	}
	return &api.LaunchLayerNodeProvider{
		InstanceID: provider.InstanceID,
		MachineID:  provider.MachineID,
		RackID:     provider.RackID,
	}
}

func apiNicoNodeProviderFromDB(nicoNodeProvider *db.NicoNodeProvider) *api.NicoNodeProvider {
	if nicoNodeProvider == nil {
		return nil
	}
	// An all-zero payload means the node is not Nico-backed (or not yet stamped).
	if nicoNodeProvider.ForgeInstanceID == "" && nicoNodeProvider.ForgeMachineID == "" && nicoNodeProvider.InstanceTypeID == uuid.Nil {
		return nil
	}
	return &api.NicoNodeProvider{
		InstanceID:     nicoNodeProvider.ForgeInstanceID,
		MachineID:      nicoNodeProvider.ForgeMachineID,
		InstanceTypeID: nicoNodeProvider.InstanceTypeID,
	}
}

// controlPlaneRolloutByNode derives, for each control-plane-controller-managed
// node in dbNodes, whether it needs rollout: it compares the node's pinned
// control-plane snapshot against the cluster's current desired snapshot. Nodes
// that are not managed by the control-plane controller (node-pool-managed
// control-plane nodes and workers) are omitted from the returned map, so callers
// leave their controlPlaneRollout field nil.
//
// The pending decision uses db.IsControlPlaneRolloutRequired, the same predicate
// the control-plane controller's rollout loop uses, so the reported state and what
// the controller acts on cannot drift apart. actionable additionally reflects the
// cluster's recycling gate (EnableCPNodeRecycling): drift is only reconciled
// automatically when recycling is enabled.
func controlPlaneRolloutByNode(ctx context.Context, database db.DB, dbNodes []db.Node) (map[uuid.UUID]*api.ControlPlaneRolloutStatus, error) {
	clusterIDs := make(map[uuid.UUID]struct{})
	pinnedIDs := make(map[uuid.UUID]struct{})
	for i := range dbNodes {
		n := &dbNodes[i]
		if !n.IsManagedByControlPlaneController() {
			continue
		}
		clusterIDs[n.ClusterID] = struct{}{}
		if n.ControlPlaneConfigurationID != uuid.Nil {
			pinnedIDs[n.ControlPlaneConfigurationID] = struct{}{}
		}
	}
	result := make(map[uuid.UUID]*api.ControlPlaneRolloutStatus)
	if len(clusterIDs) == 0 {
		return result, nil
	}

	// The cluster's desired snapshot and recycling gate are loaded with the row.
	desiredByCluster := make(map[uuid.UUID]db.ControlPlaneConfigSpec, len(clusterIDs))
	desiredIDByCluster := make(map[uuid.UUID]uuid.UUID, len(clusterIDs))
	recyclingByCluster := make(map[uuid.UUID]bool, len(clusterIDs))
	for id := range clusterIDs {
		cluster, err := database.GetCluster(ctx, id)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				continue
			}
			return nil, fmt.Errorf("failed to load cluster %s for rollout status: %w", id, err)
		}
		recyclingByCluster[id] = cluster.EnableCPNodeRecycling
		if cluster.ControlPlaneConfig.ConfigurationID != uuid.Nil {
			desiredByCluster[id] = cluster.ControlPlaneConfig.ControlPlaneConfigSpec
			desiredIDByCluster[id] = cluster.ControlPlaneConfig.ConfigurationID
		}
	}

	pinnedByID := make(map[uuid.UUID]db.ControlPlaneConfigSpec, len(pinnedIDs))
	if len(pinnedIDs) > 0 {
		ids := make([]uuid.UUID, 0, len(pinnedIDs))
		for id := range pinnedIDs {
			ids = append(ids, id)
		}
		configs, err := database.ListControlPlaneConfigurationsByIDs(ctx, ids)
		if err != nil {
			return nil, fmt.Errorf("failed to load pinned control-plane configurations for rollout status: %w", err)
		}
		for i := range configs {
			pinnedByID[configs[i].ID] = configs[i].Config
		}
	}

	for i := range dbNodes {
		n := &dbNodes[i]
		if !n.IsManagedByControlPlaneController() {
			continue
		}
		desired, ok := desiredByCluster[n.ClusterID]
		if !ok {
			continue
		}
		pinned, ok := pinnedByID[n.ControlPlaneConfigurationID]
		if !ok {
			continue
		}
		changes := db.ComputeControlPlaneRolloutChanges(pinned, desired)
		pending := len(changes) > 0
		result[n.ID] = &api.ControlPlaneRolloutStatus{
			Pending:                pending,
			Actionable:             pending && recyclingByCluster[n.ClusterID],
			Changes:                apiRolloutChangesFromDiff(changes),
			PinnedConfigurationID:  n.ControlPlaneConfigurationID,
			DesiredConfigurationID: desiredIDByCluster[n.ClusterID],
		}
	}
	return result, nil
}

// apiRolloutChangesFromDiff maps the db-layer diff to the API change list.
func apiRolloutChangesFromDiff(changes []db.ControlPlaneRolloutChange) []api.ControlPlaneRolloutChange {
	if len(changes) == 0 {
		return nil
	}
	out := make([]api.ControlPlaneRolloutChange, len(changes))
	for i, c := range changes {
		out[i] = api.ControlPlaneRolloutChange{Field: c.Field, From: c.From, To: c.To}
	}
	return out
}

// apiMachineHealthIssueToWorkflow converts an API MachineHealthIssue to a workflow MachineHealthIssue.
func apiMachineHealthIssueToWorkflow(apiMHI *api.MachineHealthIssue) *workflows.MachineHealthIssue {
	if apiMHI == nil {
		return nil
	}

	mhi := workflows.MachineHealthIssue{
		Category: apiMHI.Category,
		Summary:  apiMHI.Summary,
	}

	if apiMHI.Details != "" {
		mhi.Details = &apiMHI.Details
	}
	return &mhi
}
