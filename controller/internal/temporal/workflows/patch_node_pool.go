// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
)

const (
	// A Temporal ApplicationError of type SomeNodesUnpatchableError is returned when we have patched all the nodes we can, but some nodes
	// are in states that prevent them from being patched.
	SomeNodesUnpatchableErrorType = "SomeNodesUnpatchableError"

	// A Temporal ApplicationError of type PatchFailedError is returned when an error is encountered when attempting to patch a node.
	// It's a custom type mainly so that it can include the patched and remaining node IDs, which will be visible in Temporal.
	PatchFailedErrorType = "PatchFailedError"
)

type PatchNodePoolInput struct {
	// NodePoolID is the ID of the node pool to patch -- all nodes in this pool will be patched, unless NodeIDs is specified.
	NodePoolID uuid.UUID

	// NodeIDs is a list of node IDs to patch. If specified, only these nodes will be patched (useful for resuming patching of remaining
	// nodes after a failure has occurred on a previous PatchNodePool attempt).
	NodeIDs []uuid.UUID

	NodeWorkflowType  string
	NodeWorkflowInput json.RawMessage
}

type PatchNodePoolResult struct {
	PatchedNodeIDs []uuid.UUID
}

type SomeNodesUnpatchableErrorDetails struct {
	PatchedNodeIDs      []uuid.UUID
	MissingNodeIDs      []uuid.UUID
	DeletingNodeIDs     []uuid.UUID
	PendingNodeIDs      []uuid.UUID
	ProvisioningNodeIDs []uuid.UUID
}

type PatchFailedErrorDetails struct {
	NodeID           uuid.UUID
	Err              error
	PatchedNodeIDs   []uuid.UUID
	RemainingNodeIDs []uuid.UUID
}

func PatchNodePool(ctx workflow.Context, input PatchNodePoolInput) (PatchNodePoolResult, error) {
	nodePoolNodeIDs, err := getNodeIDs(ctx, input.NodePoolID)
	if err != nil {
		return PatchNodePoolResult{}, fmt.Errorf("failed to get node IDs: %w", err)
	}

	var nodeIDs []uuid.UUID
	if input.NodeIDs != nil {
		// User specified nodeIDs -- double check that the specified nodes are actually in the node pool.
		for _, nodeID := range input.NodeIDs {
			if !slices.Contains(nodePoolNodeIDs, nodeID) {
				return PatchNodePoolResult{}, fmt.Errorf("node ID %s is not in node pool %s", nodeID, input.NodePoolID)
			}
		}
		nodeIDs = input.NodeIDs
	} else {
		nodeIDs = nodePoolNodeIDs
	}

	return patchNodes(ctx, nodeIDs, input)
}

func getNodeIDs(ctx workflow.Context, nodePoolID uuid.UUID) ([]uuid.UUID, error) {
	var a *activities.Activities

	nodeIDs := make([]uuid.UUID, 0)
	if err := workflow.ExecuteActivity(withDBOptions(ctx), a.ListNodeIDs, db.NodeFilters{
		NodePoolID: &nodePoolID,
	}).Get(ctx, &nodeIDs); err != nil {
		return nil, fmt.Errorf("failed to get node pool node IDs: %w", err)
	}

	return nodeIDs, nil
}

func patchNodes(ctx workflow.Context, nodeIDs []uuid.UUID, input PatchNodePoolInput) (PatchNodePoolResult, error) {
	remainingNodeIDs := slices.Clone(nodeIDs)
	patchedNodeIDs := make([]uuid.UUID, 0, len(nodeIDs))

	for len(remainingNodeIDs) > 0 {
		nodeID, err := chooseNodeToPatch(ctx, patchedNodeIDs, remainingNodeIDs)
		if err != nil {
			return PatchNodePoolResult{}, err
		}

		workflowOptions := workflow.ChildWorkflowOptions{
			WorkflowID:            fmt.Sprintf("%s%s", constants.PatchNodeWorkflowPrefix, nodeID),
			WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
		}

		if err := workflow.ExecuteChildWorkflow(
			workflow.WithChildOptions(ctx, workflowOptions),
			input.NodeWorkflowType,
			nodeID,
			input.NodeWorkflowInput,
		).Get(ctx, nil); err != nil {
			return PatchNodePoolResult{}, temporal.NewApplicationErrorWithOptions(
				fmt.Sprintf("failed to patch node %s: %s", nodeID, err.Error()),
				PatchFailedErrorType,
				temporal.ApplicationErrorOptions{
					Cause: err,
					Details: []any{
						PatchFailedErrorDetails{
							NodeID:           nodeID,
							Err:              err,
							PatchedNodeIDs:   patchedNodeIDs,
							RemainingNodeIDs: remainingNodeIDs,
						},
					},
				},
			)
		}

		patchedNodeIDs = append(patchedNodeIDs, nodeID)
		remainingNodeIDs = slices.DeleteFunc(remainingNodeIDs, func(id uuid.UUID) bool {
			return id == nodeID
		})
	}

	return PatchNodePoolResult{
		PatchedNodeIDs: patchedNodeIDs,
	}, nil
}

func chooseNodeToPatch(ctx workflow.Context, patchedNodeIDs, remainingNodeIDs []uuid.UUID) (uuid.UUID, error) {
	var a *activities.Activities

	var result activities.ChooseNodeToPatchResult
	if err := workflow.ExecuteActivity(withDBOptions(ctx), a.ChooseNodeToPatch, activities.ChooseNodeToPatchInput{
		RemainingNodeIDs: remainingNodeIDs,
	}).Get(ctx, &result); err != nil {
		var applicationError *temporal.ApplicationError
		if errors.As(err, &applicationError) {
			if applicationError.Type() == activities.NoPatchableNodesErrorType {
				return uuid.Nil, handleNoPatchableNodesError(applicationError, patchedNodeIDs)
			}
		}

		return uuid.Nil, fmt.Errorf("failed to choose node to patch: %w", err)
	}

	return result.NodeID, nil
}

func handleNoPatchableNodesError(applicationError *temporal.ApplicationError, patchedNodeIDs []uuid.UUID) error {
	// We have patched all the nodes we can, but some nodes are in states that prevent them from being patched.
	// Return an error indicating which nodes we patched and which nodes were unpatchable.

	var details activities.NoPatchableNodesErrorDetails
	if err := applicationError.Details(&details); err != nil {
		return fmt.Errorf("failed to get NoPatchableNodesError details: %w", err)
	}

	return temporal.NewApplicationErrorWithOptions(
		fmt.Sprintf("some nodes were not in a state that allowed them to be patched: "+
			"patched=%d, missing=%d, deleting=%d, pending=%d, provisioning=%d",
			len(patchedNodeIDs), len(details.MissingNodeIDs), len(details.DeletingNodeIDs), len(details.PendingNodeIDs), len(details.ProvisioningNodeIDs)),
		SomeNodesUnpatchableErrorType,
		temporal.ApplicationErrorOptions{
			Details: []any{
				SomeNodesUnpatchableErrorDetails{
					PatchedNodeIDs:      patchedNodeIDs,
					MissingNodeIDs:      details.MissingNodeIDs,
					DeletingNodeIDs:     details.DeletingNodeIDs,
					PendingNodeIDs:      details.PendingNodeIDs,
					ProvisioningNodeIDs: details.ProvisioningNodeIDs,
				},
			},
		},
	)
}
