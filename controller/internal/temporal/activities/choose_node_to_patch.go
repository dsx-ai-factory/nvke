// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

const (
	// If no patchable nodes are found, return a non-retryable Temporal ApplicationError of type NoPatchableNodesError will be returned.
	NoPatchableNodesErrorType = "NoPatchableNodesError"
)

type ChooseNodeToPatchInput struct {
	RemainingNodeIDs []uuid.UUID
}

type ChooseNodeToPatchResult struct {
	NodeID   uuid.UUID
	NodeName string
	NodeIP   string
}

type NoPatchableNodesErrorDetails struct {
	MissingNodeIDs      []uuid.UUID
	DeletingNodeIDs     []uuid.UUID
	PendingNodeIDs      []uuid.UUID
	ProvisioningNodeIDs []uuid.UUID
}

// ChooseNodeToPatch chooses a node from a list of node IDs, prioritizing nodes
// that have healthy conditions first. This makes it more likely that the
// patching workflow will make it through more nodes before hitting potential
// failures on unhealthy nodes. Unhealthy nodes will be chosen only when there
// are no healthy nodes left.
//
// Nodes that are missing, deleting, pending, or provisioning are ineligible
// for patching. If all remaining nodes are in these states, a non-retryable
// Temporal error of type NoPatchableNodesError will be returned.
func (a *Activities) ChooseNodeToPatch(ctx context.Context, input ChooseNodeToPatchInput) (ChooseNodeToPatchResult, error) {
	unhealthyNodes := []db.Node{}

	missingNodeIDs := []uuid.UUID{}
	deletingNodeIDs := []uuid.UUID{}
	pendingNodeIDs := []uuid.UUID{}
	provisioningNodeIDs := []uuid.UUID{}

	for _, nodeID := range input.RemainingNodeIDs {
		node, err := a.db.GetNode(ctx, nodeID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				missingNodeIDs = append(missingNodeIDs, nodeID)
				continue
			}
			return ChooseNodeToPatchResult{}, fmt.Errorf("failed to query node from DB: %w", err)
		}
		if node.Conditions.Deleting.Status == db.ConditionStatusTrue {
			deletingNodeIDs = append(deletingNodeIDs, nodeID)
			continue
		}
		if node.Conditions.Pending.Status == db.ConditionStatusTrue {
			pendingNodeIDs = append(pendingNodeIDs, nodeID)
			continue
		}
		if node.Conditions.Provisioning.Status == db.ConditionStatusTrue {
			provisioningNodeIDs = append(provisioningNodeIDs, nodeID)
			continue
		}

		if node.Conditions.KubernetesReady.Status != db.ConditionStatusTrue ||
			node.Conditions.KubernetesUnschedulable.Status != db.ConditionStatusFalse ||
			node.Conditions.NodeAgentReady.Status != db.ConditionStatusTrue {
			unhealthyNodes = append(unhealthyNodes, node)
			continue
		}

		// If we get here, the node is healthy, so choose it -- return early without iterating through the rest of the nodes.
		return ChooseNodeToPatchResult{
			NodeID:   nodeID,
			NodeName: node.Name,
			NodeIP:   node.IP,
		}, nil
	}

	// If we're here, something is wrong with all remaining nodes. If there's at least one unhealthy node, choose the first one.
	if len(unhealthyNodes) > 0 {
		return ChooseNodeToPatchResult{
			NodeID:   unhealthyNodes[0].ID,
			NodeName: unhealthyNodes[0].Name,
			NodeIP:   unhealthyNodes[0].IP,
		}, nil
	}

	// None of the remaining nodes are eligible to patch, so return an non-retryable error of type NoPatchableNodesError
	return ChooseNodeToPatchResult{}, temporal.NewApplicationErrorWithOptions(
		fmt.Sprintf("No patchable nodes remaining: missing=%d, pending=%d, provisioning=%d, deleting=%d",
			len(missingNodeIDs), len(pendingNodeIDs), len(provisioningNodeIDs), len(deletingNodeIDs)),
		NoPatchableNodesErrorType,
		temporal.ApplicationErrorOptions{
			NonRetryable: true,
			Details: []any{
				NoPatchableNodesErrorDetails{
					MissingNodeIDs:      missingNodeIDs,
					DeletingNodeIDs:     deletingNodeIDs,
					PendingNodeIDs:      pendingNodeIDs,
					ProvisioningNodeIDs: provisioningNodeIDs,
				},
			},
		},
	)
}
