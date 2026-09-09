// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"go.temporal.io/sdk/client"

	"github.com/NVIDIA/nke/controller/api"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/nodes"
	"github.com/NVIDIA/nke/controller/internal/temporal/workflows"
)

type nodeService struct {
	logger         *slog.Logger
	db             db.DB
	temporalClient client.Client
}

func NewNodeService(logger *slog.Logger, database db.DB, temporalClient client.Client) *nodeService {
	return &nodeService{
		logger:         logger,
		db:             database,
		temporalClient: temporalClient,
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
	apiNodes := make([]api.Node, len(dbNodes))
	for i := range dbNodes {
		apiNodes[i] = apiNodeFromDB(&dbNodes[i])
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

	return api.GetNode200JSONResponse(apiNodeFromDB(&dbNode)), nil
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
		if errors.Is(err, errClusterDeleting) || errors.Is(err, errNodePoolDeleting) || errors.Is(err, errNodeDeleting) {
			// Teardown already in progress via cluster, pool, or a prior node delete.
			return api.DeleteNode202Response{}, nil
		}
		if errors.Is(err, nodes.ErrLastJoinedControlPlaneNode) {
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
		Provisioning:            apiConditionFromDB(dbNode.Conditions.Provisioning),
		ReadyToJoin:             apiConditionFromDB(dbNode.Conditions.ReadyToJoin),
		Joined:                  apiConditionFromDB(dbNode.Conditions.Joined),
		Provisioned:             apiConditionFromDB(dbNode.Conditions.Provisioned),
		Deleting:                apiConditionFromDB(dbNode.Conditions.Deleting),
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

	return api.Node{
		ID:           dbNode.ID,
		NodePoolID:   dbNode.NodePoolID,
		NodeSetID:    dbNode.NodeSetID,
		Name:         dbNode.Name,
		IP:           dbNode.IP,
		Nico:         apiNicoNodeProviderFromDB(dbNode.Nico),
		ControlPlane: dbNode.ControlPlane,
		Conditions:   nodeConditions,
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
