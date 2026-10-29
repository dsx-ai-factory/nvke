// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
)

func TestUpdateSpireServerWorkflow(t *testing.T) {
	clusterID := uuid.New()
	nodeA := db.Node{ID: uuid.New(), Name: "a", IP: "10.0.0.1", SpiffeEnabled: true}
	nodeB := db.Node{ID: uuid.New(), Name: "b", IP: "10.0.0.2", SpiffeEnabled: true}
	noSpiffe := db.Node{ID: uuid.New(), Name: "no-spiffe", IP: "10.0.0.3"}
	noIP := db.Node{ID: uuid.New(), Name: "no-ip", SpiffeEnabled: true}

	tests := []struct {
		name        string
		nodes       []db.Node
		failNode    uuid.UUID
		wantFailure string
	}{
		{name: "all nodes succeed and the lock is cleared", nodes: []db.Node{nodeA, noSpiffe, nodeB}},
		{
			name:        "one node fails and UpdateFailed names it",
			nodes:       []db.Node{nodeA, noSpiffe, nodeB},
			failNode:    nodeB.ID,
			wantFailure: "SPIRE server update failed on nodes: b",
		},
		{
			name:        "a SPIFFE node without an IP is reported as failed",
			nodes:       []db.Node{nodeA, noSpiffe, nodeB, noIP},
			wantFailure: "SPIRE server update failed on nodes: no-ip",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
			var a *activities.Activities

			env.OnActivity(a.ListNodes, mock.Anything, db.NodeFilters{ClusterID: &clusterID}).
				Return(tt.nodes, nil).Once()
			for _, n := range []db.Node{nodeA, nodeB} {
				var err error
				if n.ID == tt.failNode {
					err = errors.New("no SVID")
				}
				env.OnActivity(a.ReconfigureNodeSpireAgent, mock.Anything, activities.ReconfigureNodeSpireAgentInput{
					NodeID: n.ID, Address: "spire.example.test", Port: 8081,
				}).Return(activities.ReconfigureNodeSpireAgentResult{}, err)
			}
			env.OnActivity(a.ClearSpireServerUpdateInProgress, mock.Anything, activities.ClearSpireServerUpdateInProgressInput{
				ClusterID: clusterID, FailureMessage: tt.wantFailure,
			}).Return(activities.ClearSpireServerUpdateInProgressResult{}, nil).Once()

			env.ExecuteWorkflow(UpdateSpireServer, UpdateSpireServerInput{ClusterID: clusterID, Address: "spire.example.test", Port: 8081})

			require.True(t, env.IsWorkflowCompleted())
			if tt.wantFailure == "" {
				require.NoError(t, env.GetWorkflowError())
			} else {
				require.ErrorContains(t, env.GetWorkflowError(), tt.wantFailure)
			}
			env.AssertExpectations(t)
			for _, skipped := range []db.Node{noSpiffe, noIP} {
				env.AssertNotCalled(t, "ReconfigureNodeSpireAgent", mock.Anything, activities.ReconfigureNodeSpireAgentInput{
					NodeID: skipped.ID, Address: "spire.example.test", Port: 8081,
				})
			}
		})
	}
}
