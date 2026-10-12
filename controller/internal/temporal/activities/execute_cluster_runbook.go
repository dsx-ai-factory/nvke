// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"
	"math/rand"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/nodes"
)

type ExecuteClusterRunbookInput struct {
	ClusterID         uuid.UUID
	RunbookName       string
	RunbookEnv        map[string]string
	RunbookArgs       []string
	ExpectedExitCodes []int
	ExcludeNodeID     *uuid.UUID
}

type ExecuteClusterRunbookResult struct {
	// NodeID is the ID of the control plane node that was used to execute the runbook.
	NodeID uuid.UUID
	// NodeName is the name of the control plane node that was used to execute the runbook.
	NodeName string
	// NodeIP is the IP address of the control plane node that was used to execute the runbook.
	NodeIP string
	// Output is the stdout logs from the execution of the runbook.
	Output string
	// ErrorMessage is the stderr logs from the execution of the runbook.
	ErrorMessage string
	// ExitCode is the process exit code from the execution of the runbook.
	ExitCode int
}

func pointerTo[T any](v T) *T {
	return &v
}

func (a *Activities) ExecuteClusterRunbook(
	ctx context.Context,
	input ExecuteClusterRunbookInput,
) (ExecuteClusterRunbookResult, error) {
	logger := activity.GetLogger(ctx)

	controlPlaneNodes, err := a.db.ListNodes(ctx, db.NodeFilters{
		ClusterID:    &input.ClusterID,
		ControlPlane: pointerTo(true),
	})
	if err != nil {
		return ExecuteClusterRunbookResult{}, fmt.Errorf("failed to list control plane nodes: %w", err)
	}

	// Teardown runbooks may target a control-plane node. In that case, run the
	// script from a surviving control-plane node instead of the node being removed.
	if input.ExcludeNodeID != nil {
		filteredNodes := make([]db.Node, 0, len(controlPlaneNodes))
		for _, node := range controlPlaneNodes {
			if node.ID == *input.ExcludeNodeID {
				logger.Debug("Excluding control plane node from runbook executor candidates", "clusterID", input.ClusterID, "nodeID", node.ID, "nodeName", node.Name, "runbookName", input.RunbookName)
				continue
			}
			filteredNodes = append(filteredNodes, node)
		}
		controlPlaneNodes = filteredNodes
	}

	candidates, healthyCandidates := nodes.PickControlPlaneRunbookCandidates(controlPlaneNodes)
	if len(candidates) == 0 {
		return ExecuteClusterRunbookResult{}, fmt.Errorf("no joined control plane nodes are available for runbook execution")
	}
	if !healthyCandidates {
		activity.GetLogger(ctx).Warn(
			"No healthy joined control plane nodes; falling back to all joined nodes for runbook execution",
			"clusterID", input.ClusterID,
			"runbookName", input.RunbookName,
			"candidateCount", len(candidates),
		)
	}

	//nolint:gosec // Non-secure random number generator is acceptable here
	controlPlaneNode := candidates[rand.Intn(len(candidates))]

	result, err := a.ExecuteHostRunbook(ctx, ExecuteHostRunbookInput{
		HostID:            controlPlaneNode.ID, // Assumes host ID is the same as node ID
		RunbookName:       input.RunbookName,
		RunbookEnv:        input.RunbookEnv,
		RunbookArgs:       input.RunbookArgs,
		ExpectedExitCodes: input.ExpectedExitCodes,
	})
	if err != nil {
		return ExecuteClusterRunbookResult{}, fmt.Errorf("failed to execute control plane runbook: %w", err)
	}

	return ExecuteClusterRunbookResult{
		NodeID:       controlPlaneNode.ID,
		NodeName:     controlPlaneNode.Name,
		NodeIP:       controlPlaneNode.IP,
		Output:       result.Output,
		ErrorMessage: result.ErrorMessage,
		ExitCode:     result.ExitCode,
	}, nil
}
