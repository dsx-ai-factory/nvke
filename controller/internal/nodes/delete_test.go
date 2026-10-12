// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodes_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/nodes"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/workflows"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestDelete(t *testing.T) {
	nodeID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	nodePoolID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440001")
	nodeSetID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440002")

	tests := []struct {
		name             string
		nodeID           uuid.UUID
		seedNodes        []db.Node
		opts             []nodes.DeleteOpt
		wantErr          error
		wantTeardownNode workflows.TeardownNodeInput
	}{
		{
			name:   "success - basic delete",
			nodeID: nodeID,
			seedNodes: []db.Node{
				newTestNode(nodeID, nodePoolID, nodeSetID),
			},
			wantTeardownNode: workflows.TeardownNodeInput{
				NodeID: nodeID,
			},
		},
		{
			name:    "node not found",
			nodeID:  nodeID,
			wantErr: db.ErrNotFound,
		},
		{
			name:   "success with reason option",
			nodeID: nodeID,
			seedNodes: []db.Node{
				newTestNode(nodeID, nodePoolID, nodeSetID),
			},
			opts: []nodes.DeleteOpt{nodes.WithReason("manual deletion")},
			wantTeardownNode: workflows.TeardownNodeInput{
				NodeID: nodeID,
				Reason: "manual deletion",
			},
		},
		{
			name:   "success with allow CP node option",
			nodeID: nodeID,
			seedNodes: []db.Node{
				newTestNode(nodeID, nodePoolID, nodeSetID),
			},
			opts: []nodes.DeleteOpt{nodes.WithAllowCPNode(true)},
			wantTeardownNode: workflows.TeardownNodeInput{
				NodeID:      nodeID,
				AllowCPNode: true,
			},
		},
		{
			name:   "success with machine health issue option",
			nodeID: nodeID,
			seedNodes: []db.Node{
				newTestNode(nodeID, nodePoolID, nodeSetID),
			},
			opts: []nodes.DeleteOpt{nodes.WithMachineHealthIssue(newMachineHealthIssue("Hardware", "Disk failure detected"))},
			wantTeardownNode: workflows.TeardownNodeInput{
				NodeID:             nodeID,
				MachineHealthIssue: newMachineHealthIssue("Hardware", "Disk failure detected"),
			},
		},
		{
			name:   "success with multiple options",
			nodeID: nodeID,
			seedNodes: []db.Node{
				newTestNode(nodeID, nodePoolID, nodeSetID),
			},
			opts: []nodes.DeleteOpt{
				nodes.WithReason("hardware failure"),
				nodes.WithAllowCPNode(true),
				nodes.WithMachineHealthIssue(newMachineHealthIssue("Hardware", "Memory failure")),
			},
			wantTeardownNode: workflows.TeardownNodeInput{
				NodeID:             nodeID,
				Reason:             "hardware failure",
				AllowCPNode:        true,
				MachineHealthIssue: newMachineHealthIssue("Hardware", "Memory failure"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			mockTx := &testutil.MockTx{}

			if tt.wantErr != nil {
				mockTx.On("GetNodeForUpdate", mock.Anything, tt.nodeID).Return(db.Node{}, tt.wantErr)
			} else {
				node := newTestNode(tt.nodeID, nodePoolID, nodeSetID)
				mockTx.On("GetNodeForUpdate", mock.Anything, tt.nodeID).Return(node, nil)
				mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(updated *db.Node) bool {
					return updated.ID == tt.nodeID &&
						updated.Conditions.Deleting.Status == db.ConditionStatusTrue &&
						updated.Conditions.Deleting.Message == "Node deletion in progress"
				})).Return(node, nil)
				mockTx.On(
					"CreateWorkflowOutboxEntry",
					mock.Anything,
					constants.TeardownNodeWorkflowType,
					constants.TeardownNodeWorkflowPrefix+tt.nodeID.String(),
					mock.MatchedBy(func(input workflows.TeardownNodeInput) bool {
						if input.NodeID != tt.wantTeardownNode.NodeID ||
							input.Reason != tt.wantTeardownNode.Reason ||
							input.AllowCPNode != tt.wantTeardownNode.AllowCPNode ||
							input.SkipRemoveFromCluster != tt.wantTeardownNode.SkipRemoveFromCluster {
							return false
						}
						if (input.MachineHealthIssue == nil) != (tt.wantTeardownNode.MachineHealthIssue == nil) {
							return false
						}
						if input.MachineHealthIssue != nil && tt.wantTeardownNode.MachineHealthIssue != nil {
							return input.MachineHealthIssue.Category == tt.wantTeardownNode.MachineHealthIssue.Category &&
								input.MachineHealthIssue.Summary == tt.wantTeardownNode.MachineHealthIssue.Summary
						}
						return true
					}),
				).Return(db.WorkflowOutbox{}, nil)
			}

			// Execute Delete
			opts := append([]nodes.DeleteOpt{nodes.WithSkipLiveDeleteGuard()}, tt.opts...)
			err := nodes.DeleteWithTx(ctx, mockTx, tt.nodeID, opts...)

			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				mockTx.AssertExpectations(t)
				return
			}

			require.NoError(t, err)
			mockTx.AssertExpectations(t)
		})
	}
}

func newTestNode(id, nodePoolID, nodeSetID uuid.UUID) db.Node {
	return db.Node{
		ID:         id,
		NodePoolID: nodePoolID,
		NodeSetID:  nodeSetID,
		Name:       "test-node-" + id.String()[:8],
		IP:         "10.0.0.1",
		Conditions: db.NodeConditions{},
		Nico: &db.NicoNodeProvider{
			ForgeInstanceID: "forge-instance-" + id.String()[:8],
			ForgeMachineID:  "forge-machine-" + id.String()[:8],
		},
	}
}

//nolint:unparam // it's ok that category is always set to "Hardware" in tests
func newMachineHealthIssue(category, summary string) *workflows.MachineHealthIssue {
	mhi := workflows.MachineHealthIssue{
		Category: category,
		Summary:  summary,
	}
	return &mhi
}
