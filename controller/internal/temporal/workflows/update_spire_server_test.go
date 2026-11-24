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
	"go.temporal.io/sdk/workflow"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
)

func TestUpdateSpireServerWorkflow(t *testing.T) {
	clusterID := uuid.New()
	nodeA := db.Node{ID: uuid.New(), Name: "a", IP: "10.0.0.1", SpiffeEnabled: true}
	nodeB := db.Node{ID: uuid.New(), Name: "b", IP: "10.0.0.2", SpiffeEnabled: true}
	noSpiffe := db.Node{ID: uuid.New(), Name: "no-spiffe", IP: "10.0.0.3"}
	noIP := db.Node{ID: uuid.New(), Name: "no-ip", SpiffeEnabled: true}
	noSpireAgent := db.Node{ID: uuid.New(), Name: "no-spire-agent", IP: "10.0.0.4", SpiffeEnabled: true, NodeIdentity: db.NodeIdentityMetadataService}
	dialAddressOnly := db.Node{
		ID: uuid.New(), Name: "dial-address-only", SpiffeEnabled: true,
		Capi: &db.CAPINodeProvider{AgentDialAddress: "10.1.0.5"},
	}

	tests := []struct {
		name  string
		nodes []db.Node
		// preMarkerHistory replays a history recorded before nodes could take
		// their identity from the metadata service.
		preMarkerHistory bool
		failNode         uuid.UUID
		wantFailure      string
	}{
		{name: "all nodes succeed and the lock is cleared", nodes: []db.Node{nodeA, noSpiffe, nodeB}},
		{name: "a node without a SPIRE agent is skipped", nodes: []db.Node{nodeA, noSpireAgent, nodeB}},
		{
			name:             "a pre-marker history still updates a node without a SPIRE agent",
			nodes:            []db.Node{nodeA, noSpireAgent, nodeB},
			preMarkerHistory: true,
		},
		{
			name:  "a node reachable at a dial address is updated before it reports an IP",
			nodes: []db.Node{nodeA, dialAddressOnly, nodeB},
		},
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

			markerVersion := workflow.Version(1)
			if tt.preMarkerHistory {
				markerVersion = workflow.DefaultVersion
			}
			env.OnGetVersion(spireServerUpdateMetadataServiceChangeID, workflow.DefaultVersion, 1).
				Return(markerVersion).Once()

			// A node is updated when the workflow can reach it and it runs a
			// SPIRE agent, which a pre-marker history assumes of every node.
			updated := []db.Node{nodeA, nodeB}
			skipped := []db.Node{noSpiffe, noIP}
			for _, n := range tt.nodes {
				switch {
				case n.ID == noSpireAgent.ID && tt.preMarkerHistory:
					updated = append(updated, n)
				case n.ID == noSpireAgent.ID:
					skipped = append(skipped, n)
				case n.ID == dialAddressOnly.ID:
					updated = append(updated, n)
				}
			}

			env.OnActivity(a.ListNodes, mock.Anything, db.NodeFilters{ClusterID: &clusterID}).
				Return(tt.nodes, nil).Once()
			for _, n := range updated {
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
			for _, n := range skipped {
				env.AssertNotCalled(t, "ReconfigureNodeSpireAgent", mock.Anything, activities.ReconfigureNodeSpireAgentInput{
					NodeID: n.ID, Address: "spire.example.test", Port: 8081,
				})
			}
		})
	}
}
