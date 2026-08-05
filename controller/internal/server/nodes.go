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
		ClusterID:   req.Params.ClusterID,
		NodePoolID:  req.Params.NodePoolID,
		ClusterName: req.Params.ClusterName,
		Name:        req.Params.Name,
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

	err := s.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		return nodes.DeleteWithTx(ctx, tx, req.NodeID,
			nodes.WithReason("API"),
			nodes.WithAllowCPNode(true),
			nodes.WithMachineHealthIssue(machineHealthIssue),
		)
	})
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
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
		ID:              dbNode.ID,
		NodePoolID:      dbNode.NodePoolID,
		NodeSetID:       dbNode.NodeSetID,
		Name:            dbNode.Name,
		ForgeInstanceID: dbNode.ForgeInstanceID,
		ForgeMachineID:  dbNode.ForgeMachineID,
		IP:              dbNode.IP,
		InstanceTypeID:  dbNode.InstanceTypeID,
		Conditions:      nodeConditions,
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
