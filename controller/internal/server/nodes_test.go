// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/nodes"
	"github.com/dsx-ai-factory/nvke/controller/internal/server"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/workflows"
)

func TestNodeService_ListNodes(t *testing.T) {
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440001")
	nodePoolID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440002")
	now := time.Now()
	tests := []struct {
		name      string
		filters   db.NodeFilters
		dbNodes   []db.Node
		dbError   error
		wantError bool
		wantNodes []api.Node
	}{
		{
			name: "success with both filters",
			filters: db.NodeFilters{
				ClusterID:  &clusterID,
				NodePoolID: &nodePoolID,
			},
			dbNodes: []db.Node{
				{
					ID:         uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
					NodePoolID: nodePoolID,
					Name:       "test-node",
					IP:         "10.0.0.1",
					Conditions: mockDBNodeConditions(now),
				},
			},
			wantNodes: []api.Node{
				{
					ID:         uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
					NodePoolID: nodePoolID,
					Name:       "test-node",
					IP:         "10.0.0.1",
					Conditions: mockAPINodeConditions(now),
				},
			},
		},
		{
			name: "success with cluster filter",
			filters: db.NodeFilters{
				ClusterID: &clusterID,
			},
			dbNodes: []db.Node{
				{
					ID:         uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
					NodePoolID: nodePoolID,
					Name:       "test-node",
					IP:         "10.0.0.1",
					Conditions: mockDBNodeConditions(now),
				},
			},
			wantNodes: []api.Node{
				{
					ID:         uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
					NodePoolID: nodePoolID,
					Name:       "test-node",
					IP:         "10.0.0.1",
					Conditions: mockAPINodeConditions(now),
				},
			},
		},
		{
			name:    "success without filters",
			filters: db.NodeFilters{},
			dbNodes: []db.Node{
				{
					ID:         uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
					NodePoolID: nodePoolID,
					Name:       "test-node",
					IP:         "10.0.0.1",
					Conditions: mockDBNodeConditions(now),
				},
			},
			wantNodes: []api.Node{
				{
					ID:         uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
					NodePoolID: nodePoolID,
					Name:       "test-node",
					IP:         "10.0.0.1",
					Conditions: mockAPINodeConditions(now),
				},
			},
		},
		{
			name: "success with cluster name filter",
			filters: db.NodeFilters{
				ClusterName: testutil.PtrTo("test-cluster"),
			},
			dbNodes: []db.Node{
				{
					ID:         uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
					NodePoolID: nodePoolID,
					Name:       "test-cluster-worker-0001",
					IP:         "10.0.0.1",
					Conditions: mockDBNodeConditions(now),
				},
			},
			wantNodes: []api.Node{
				{
					ID:         uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
					NodePoolID: nodePoolID,
					Name:       "test-cluster-worker-0001",
					IP:         "10.0.0.1",
					Conditions: mockAPINodeConditions(now),
				},
			},
		},
		{
			name: "success with node name filter",
			filters: db.NodeFilters{
				Name: testutil.PtrTo("test-cluster-worker-0001"),
			},
			dbNodes: []db.Node{
				{
					ID:         uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
					NodePoolID: nodePoolID,
					Name:       "test-cluster-worker-0001",
					IP:         "10.0.0.1",
					Conditions: mockDBNodeConditions(now),
				},
			},
			wantNodes: []api.Node{
				{
					ID:         uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
					NodePoolID: nodePoolID,
					Name:       "test-cluster-worker-0001",
					IP:         "10.0.0.1",
					Conditions: mockAPINodeConditions(now),
				},
			},
		},
		{
			name: "success with both cluster name and node name filters",
			filters: db.NodeFilters{
				ClusterName: testutil.PtrTo("test-cluster"),
				Name:        testutil.PtrTo("test-cluster-worker-0001"),
			},
			dbNodes: []db.Node{
				{
					ID:         uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
					NodePoolID: nodePoolID,
					Name:       "test-cluster-worker-0001",
					IP:         "10.0.0.1",
					Conditions: mockDBNodeConditions(now),
				},
			},
			wantNodes: []api.Node{
				{
					ID:         uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
					NodePoolID: nodePoolID,
					Name:       "test-cluster-worker-0001",
					IP:         "10.0.0.1",
					Conditions: mockAPINodeConditions(now),
				},
			},
		},
		{
			name:    "surfaces control-plane indicator",
			filters: db.NodeFilters{},
			dbNodes: []db.Node{
				{
					ID:           uuid.MustParse("550e8400-e29b-41d4-a716-446655440010"),
					NodePoolID:   nodePoolID,
					Name:         "cp-node",
					IP:           "10.0.0.2",
					ControlPlane: true,
					Conditions:   mockDBNodeConditions(now),
				},
				{
					ID:           uuid.MustParse("550e8400-e29b-41d4-a716-446655440011"),
					NodePoolID:   nodePoolID,
					Name:         "worker-node",
					IP:           "10.0.0.3",
					ControlPlane: false,
					Conditions:   mockDBNodeConditions(now),
				},
			},
			wantNodes: []api.Node{
				{
					ID:           uuid.MustParse("550e8400-e29b-41d4-a716-446655440010"),
					NodePoolID:   nodePoolID,
					Name:         "cp-node",
					IP:           "10.0.0.2",
					ControlPlane: true,
					Conditions:   mockAPINodeConditions(now),
				},
				{
					ID:           uuid.MustParse("550e8400-e29b-41d4-a716-446655440011"),
					NodePoolID:   nodePoolID,
					Name:         "worker-node",
					IP:           "10.0.0.3",
					ControlPlane: false,
					Conditions:   mockAPINodeConditions(now),
				},
			},
		},
		{
			name:    "filter control-plane true returns only control-plane nodes",
			filters: db.NodeFilters{ControlPlane: testutil.PtrTo(true)},
			dbNodes: []db.Node{
				{
					ID:           uuid.MustParse("550e8400-e29b-41d4-a716-446655440010"),
					NodePoolID:   nodePoolID,
					Name:         "cp-node",
					IP:           "10.0.0.2",
					ControlPlane: true,
					Conditions:   mockDBNodeConditions(now),
				},
			},
			wantNodes: []api.Node{
				{
					ID:           uuid.MustParse("550e8400-e29b-41d4-a716-446655440010"),
					NodePoolID:   nodePoolID,
					Name:         "cp-node",
					IP:           "10.0.0.2",
					ControlPlane: true,
					Conditions:   mockAPINodeConditions(now),
				},
			},
		},
		{
			name:    "filter control-plane false returns only worker nodes",
			filters: db.NodeFilters{ControlPlane: testutil.PtrTo(false)},
			dbNodes: []db.Node{
				{
					ID:           uuid.MustParse("550e8400-e29b-41d4-a716-446655440011"),
					NodePoolID:   nodePoolID,
					Name:         "worker-node",
					IP:           "10.0.0.3",
					ControlPlane: false,
					Conditions:   mockDBNodeConditions(now),
				},
			},
			wantNodes: []api.Node{
				{
					ID:           uuid.MustParse("550e8400-e29b-41d4-a716-446655440011"),
					NodePoolID:   nodePoolID,
					Name:         "worker-node",
					IP:           "10.0.0.3",
					ControlPlane: false,
					Conditions:   mockAPINodeConditions(now),
				},
			},
		},
		{
			name:      "db error",
			filters:   db.NodeFilters{},
			dbError:   errors.New("db error"),
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockDB := &mockDB{}
			mockDB.On("ListNodes", mock.Anything, tt.filters).Return(tt.dbNodes, tt.dbError)

			service := server.NewNodeService(&slog.Logger{}, mockDB)

			resp, err := service.ListNodes(context.Background(), api.ListNodesRequestObject{
				Params: api.ListNodesParams{
					ClusterID:    tt.filters.ClusterID,
					NodePoolID:   tt.filters.NodePoolID,
					ClusterName:  tt.filters.ClusterName,
					Name:         tt.filters.Name,
					ControlPlane: tt.filters.ControlPlane,
				},
			})

			if tt.wantError {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			response := resp.(api.ListNodes200JSONResponse)
			assert.Equal(t, tt.wantNodes, response.Nodes)
			mockDB.AssertExpectations(t)
		})
	}
}

func TestNodeService_GetNode(t *testing.T) {
	nodeID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	now := time.Now()
	tests := []struct {
		name      string
		nodeID    uuid.UUID
		dbNode    db.Node
		dbError   error
		wantError bool
		want404   bool
		wantNode  api.GetNode200JSONResponse
	}{
		{
			name:   "success",
			nodeID: nodeID,
			dbNode: db.Node{
				ID:         nodeID,
				NodePoolID: uuid.MustParse("550e8400-e29b-41d4-a716-446655440001"),
				NodeSetID:  uuid.MustParse("de58d168-75d7-4cb8-aae8-b1f4691440db"),
				Name:       "test-node",
				IP:         "10.0.0.1",
				Conditions: mockDBNodeConditions(now),
			},
			wantNode: api.GetNode200JSONResponse{
				ID:         nodeID,
				NodePoolID: uuid.MustParse("550e8400-e29b-41d4-a716-446655440001"),
				NodeSetID:  uuid.MustParse("de58d168-75d7-4cb8-aae8-b1f4691440db"),
				Name:       "test-node",
				IP:         "10.0.0.1",
				Conditions: mockAPINodeConditions(now),
			},
		},
		{
			name:   "surfaces control-plane indicator",
			nodeID: nodeID,
			dbNode: db.Node{
				ID:           nodeID,
				NodePoolID:   uuid.MustParse("550e8400-e29b-41d4-a716-446655440001"),
				NodeSetID:    uuid.MustParse("de58d168-75d7-4cb8-aae8-b1f4691440db"),
				Name:         "cp-node",
				IP:           "10.0.0.2",
				ControlPlane: true,
				Conditions:   mockDBNodeConditions(now),
			},
			wantNode: api.GetNode200JSONResponse{
				ID:           nodeID,
				NodePoolID:   uuid.MustParse("550e8400-e29b-41d4-a716-446655440001"),
				NodeSetID:    uuid.MustParse("de58d168-75d7-4cb8-aae8-b1f4691440db"),
				Name:         "cp-node",
				IP:           "10.0.0.2",
				ControlPlane: true,
				Conditions:   mockAPINodeConditions(now),
			},
		},
		{
			name:    "not found",
			nodeID:  nodeID,
			dbError: db.ErrNotFound,
			want404: true,
		},
		{
			name:      "db error",
			nodeID:    nodeID,
			dbError:   errors.New("db error"),
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockDB := &mockDB{}
			mockDB.On("GetNode", mock.Anything, tt.nodeID).Return(tt.dbNode, tt.dbError)

			service := server.NewNodeService(&slog.Logger{}, mockDB)

			resp, err := service.GetNode(context.Background(), api.GetNodeRequestObject{
				NodeID: tt.nodeID,
			})

			if tt.wantError {
				assert.Error(t, err)
				return
			}
			if tt.want404 {
				assert.IsType(t, api.GetNode404Response{}, resp)
				return
			}

			require.NoError(t, err)
			response := resp.(api.GetNode200JSONResponse)
			assert.Equal(t, tt.wantNode, response)
			mockDB.AssertExpectations(t)
		})
	}
}

func TestNodeService_DeleteNode(t *testing.T) {
	nodeID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440001")
	tests := []struct {
		name               string
		nodeID             uuid.UUID
		machineHealthIssue *api.MachineHealthIssue
		forceUnreachable   bool
		setupMockCalls     func(*mockTx)
		wantError          bool
		want400            bool
		want400Contains    string
		want404            bool
		verifyConditions   func(t *testing.T, conditions db.NodeConditions)
	}{
		{
			name:   "success",
			nodeID: nodeID,
			setupMockCalls: func(tx *mockTx) {
				tx.On("GetNode", mock.Anything, nodeID).Return(db.Node{
					ID:        nodeID,
					ClusterID: clusterID,
				}, nil)
				tx.On("GetNodeForUpdate", mock.Anything, nodeID).Return(db.Node{
					ID:        nodeID,
					ClusterID: clusterID,
				}, nil)
				tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
					ID: clusterID,
				}, nil)
				tx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(n *db.Node) bool {
					return n.ID == nodeID && n.Conditions.Deleting.Status == db.ConditionStatusTrue
				})).Return(db.Node{}, nil)
				tx.On("CreateWorkflowOutboxEntry", mock.Anything, constants.TeardownNodeWorkflowType, mock.Anything, mock.MatchedBy(func(input workflows.TeardownNodeInput) bool {
					return input.NodeID == nodeID
				})).Return(db.WorkflowOutbox{}, nil)
			},
			verifyConditions: func(t *testing.T, conditions db.NodeConditions) {
				t.Helper()
				assert.Equal(t, db.ConditionStatusTrue, conditions.Deleting.Status)
			},
		},
		{
			name:   "not found",
			nodeID: nodeID,
			setupMockCalls: func(tx *mockTx) {
				tx.On("GetNode", mock.Anything, nodeID).Return(db.Node{}, db.ErrNotFound)
			},
			want404: true,
		},
		{
			name:   "reject last joined control-plane node",
			nodeID: nodeID,
			setupMockCalls: func(tx *mockTx) {
				tx.On("GetNode", mock.Anything, nodeID).Return(db.Node{
					ID:        nodeID,
					ClusterID: clusterID,
				}, nil)
				tx.On("GetNodeForUpdate", mock.Anything, nodeID).Return(db.Node{
					ID:           nodeID,
					ClusterID:    clusterID,
					ControlPlane: true,
					Conditions: db.NodeConditions{
						Joined: db.NewCondition(true, "joined"),
					},
				}, nil)
				tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
					ID:              clusterID,
					ControlPlaneVIP: "10.0.0.100",
				}, nil)
				controlPlane := true
				tx.On("ListNodePools", mock.Anything, db.NodePoolFilters{
					ClusterID:    &clusterID,
					ControlPlane: &controlPlane,
				}).Return([]db.NodePool{}, nil)
				tx.On("ListNodes", mock.Anything, db.NodeFilters{
					ClusterID:    &clusterID,
					ControlPlane: &controlPlane,
				}).Return([]db.Node{
					{
						ID:           nodeID,
						ClusterID:    clusterID,
						ControlPlane: true,
						Conditions: db.NodeConditions{
							Joined: db.NewCondition(true, "joined"),
						},
					},
				}, nil)
			},
			want400:         true,
			want400Contains: "last joined control-plane node",
		},
		{
			name:   "success control-plane node records delete request when another joined CP remains",
			nodeID: nodeID,
			setupMockCalls: func(tx *mockTx) {
				otherCP := uuid.MustParse("550e8400-e29b-41d4-a716-446655440021")
				tx.On("GetNode", mock.Anything, nodeID).Return(db.Node{
					ID:        nodeID,
					ClusterID: clusterID,
				}, nil)
				tx.On("GetNodeForUpdate", mock.Anything, nodeID).Return(db.Node{
					ID:           nodeID,
					ClusterID:    clusterID,
					ControlPlane: true,
				}, nil)
				tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
					ID:              clusterID,
					ControlPlaneVIP: "10.0.0.100",
				}, nil)
				controlPlane := true
				tx.On("ListNodePools", mock.Anything, db.NodePoolFilters{
					ClusterID:    &clusterID,
					ControlPlane: &controlPlane,
				}).Return([]db.NodePool{}, nil)
				tx.On("ListNodes", mock.Anything, db.NodeFilters{
					ClusterID:    &clusterID,
					ControlPlane: &controlPlane,
				}).Return([]db.Node{
					{
						ID:           otherCP,
						ClusterID:    clusterID,
						ControlPlane: true,
						Conditions: db.NodeConditions{
							Joined: db.NewCondition(true, "joined"),
						},
					},
				}, nil)
				tx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(n *db.Node) bool {
					return n.ID == nodeID && n.Conditions.DeleteRequested.Status == db.ConditionStatusTrue && n.DeleteRequest != nil
				})).Return(db.Node{}, nil)
			},
			verifyConditions: func(t *testing.T, conditions db.NodeConditions) {
				t.Helper()
				assert.Equal(t, db.ConditionStatusTrue, conditions.DeleteRequested.Status)
				assert.NotEqual(t, db.ConditionStatusTrue, conditions.Deleting.Status)
			},
		},
		{
			name:   "reject control-plane node replacement without VIP",
			nodeID: nodeID,
			setupMockCalls: func(tx *mockTx) {
				tx.On("GetNode", mock.Anything, nodeID).Return(db.Node{
					ID: nodeID, ClusterID: clusterID,
				}, nil)
				tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID}, nil)
				tx.On("GetNodeForUpdate", mock.Anything, nodeID).Return(db.Node{
					ID: nodeID, ClusterID: clusterID, ControlPlane: true,
				}, nil)
			},
			want400:         true,
			want400Contains: "controlPlaneVIP",
		},
		{
			name:   "noop while cluster deleting",
			nodeID: nodeID,
			setupMockCalls: func(tx *mockTx) {
				tx.On("GetNode", mock.Anything, nodeID).Return(db.Node{
					ID:        nodeID,
					ClusterID: clusterID,
				}, nil)
				tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
					ID: clusterID,
					Conditions: db.ClusterConditions{
						Deleting: db.NewCondition(true, "deleting"),
					},
				}, nil)
			},
		},
		{
			name:   "noop while node already deleting",
			nodeID: nodeID,
			setupMockCalls: func(tx *mockTx) {
				tx.On("GetNode", mock.Anything, nodeID).Return(db.Node{
					ID:        nodeID,
					ClusterID: clusterID,
				}, nil)
				tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
					ID: clusterID,
				}, nil)
				tx.On("GetNodeForUpdate", mock.Anything, nodeID).Return(db.Node{
					ID:        nodeID,
					ClusterID: clusterID,
					Conditions: db.NodeConditions{
						Deleting: db.NewCondition(true, "deleting"),
					},
				}, nil)
			},
		},
		{
			name:   "noop while parent node pool deleting",
			nodeID: nodeID,
			setupMockCalls: func(tx *mockTx) {
				nodePoolID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440030")
				tx.On("GetNode", mock.Anything, nodeID).Return(db.Node{
					ID:        nodeID,
					ClusterID: clusterID,
				}, nil)
				tx.On("GetNodeForUpdate", mock.Anything, nodeID).Return(db.Node{
					ID:         nodeID,
					ClusterID:  clusterID,
					NodePoolID: nodePoolID,
				}, nil)
				tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
					ID: clusterID,
				}, nil)
				tx.On("GetNodePool", mock.Anything, nodePoolID).Return(db.NodePool{
					ID:        nodePoolID,
					ClusterID: clusterID,
					Conditions: db.NodePoolConditions{
						Deleting: db.NewCondition(true, "deleting"),
					},
				}, nil)
			},
		},
		{
			name:   "success with machine health issue",
			nodeID: nodeID,
			machineHealthIssue: &api.MachineHealthIssue{
				Category: "Hardware",
				Summary:  "Node removed due to hardware failure",
			},
			setupMockCalls: func(tx *mockTx) {
				tx.On("GetNode", mock.Anything, nodeID).Return(db.Node{
					ID:        nodeID,
					ClusterID: clusterID,
				}, nil)
				tx.On("GetNodeForUpdate", mock.Anything, nodeID).Return(db.Node{
					ID:        nodeID,
					ClusterID: clusterID,
				}, nil)
				tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{
					ID: clusterID,
				}, nil)
				tx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(n *db.Node) bool {
					return n.ID == nodeID && n.Conditions.Deleting.Status == db.ConditionStatusTrue
				})).Return(db.Node{}, nil)
				tx.On("CreateWorkflowOutboxEntry", mock.Anything, constants.TeardownNodeWorkflowType, mock.Anything, workflows.TeardownNodeInput{
					NodeID:             nodeID,
					Reason:             "API",
					AllowCPNode:        true,
					MachineHealthIssue: newMachineHealthIssue("Hardware", "Node removed due to hardware failure"),
				}).Return(db.WorkflowOutbox{}, nil)
			},
			verifyConditions: func(t *testing.T, conditions db.NodeConditions) {
				t.Helper()
				assert.Equal(t, db.ConditionStatusTrue, conditions.Deleting.Status)
			},
		},
		{
			name:   "Bad request - invalid machine health issue category",
			nodeID: nodeID,
			machineHealthIssue: &api.MachineHealthIssue{
				Category: "Invalid",
				Summary:  "Node removed due to invalid hardware failure",
			},
			want400: true,
		},
		{
			name:   "Bad request - invalid machine health issue summary",
			nodeID: nodeID,
			machineHealthIssue: &api.MachineHealthIssue{
				Category: "Hardware",
				Summary:  "",
			},
			want400: true,
		},
		{
			name:             "success with force unreachable (no etcd overrides, no gate needed)",
			nodeID:           nodeID,
			forceUnreachable: true,
			setupMockCalls: func(tx *mockTx) {
				tx.On("GetNode", mock.Anything, nodeID).Return(db.Node{ID: nodeID, ClusterID: clusterID}, nil)
				tx.On("GetNodeForUpdate", mock.Anything, nodeID).Return(db.Node{ID: nodeID, ClusterID: clusterID}, nil)
				tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID}, nil)
				tx.On("UpdateNode", mock.Anything, mock.Anything).Return(db.Node{}, nil)
				tx.On("CreateWorkflowOutboxEntry", mock.Anything, constants.TeardownNodeWorkflowType, mock.Anything, workflows.TeardownNodeInput{
					NodeID:           nodeID,
					Reason:           "API",
					AllowCPNode:      true,
					ForceUnreachable: true,
				}).Return(db.WorkflowOutbox{}, nil)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockDB := &mockDB{}
			mockDB.MockTx = &mockTx{}

			if tt.setupMockCalls != nil {
				tt.setupMockCalls(mockDB.MockTx)
			}

			mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

			service := server.NewNodeService(&slog.Logger{}, mockDB)

			var body *api.DeleteNodeJSONRequestBody
			if tt.machineHealthIssue != nil || tt.forceUnreachable {
				body = &api.DeleteNodeJSONRequestBody{
					MachineHealthIssue: tt.machineHealthIssue,
					ForceUnreachable:   tt.forceUnreachable,
				}
			}

			resp, err := service.DeleteNode(context.Background(), api.DeleteNodeRequestObject{
				NodeID: tt.nodeID,
				Body:   body,
			})

			if tt.verifyConditions != nil {
				var conditions db.NodeConditions
				for _, call := range mockDB.MockTx.Calls {
					if call.Method == "UpdateNode" {
						node := call.Arguments[1].(*db.Node)
						conditions = node.Conditions
						break
					}
				}
				tt.verifyConditions(t, conditions)
			}

			if tt.wantError {
				assert.Error(t, err)
				return
			}
			if tt.want400 {
				require.IsType(t, api.DeleteNode400JSONResponse{}, resp)
				if tt.want400Contains != "" {
					badRequest := resp.(api.DeleteNode400JSONResponse)
					assert.Contains(t, badRequest.Message, tt.want400Contains)
				}
				return
			}
			if tt.want404 {
				assert.IsType(t, api.DeleteNode404Response{}, resp)
				return
			}

			require.NoError(t, err)
			require.IsType(t, api.DeleteNode202Response{}, resp)

			mockDB.AssertExpectations(t)
			mockDB.MockTx.AssertExpectations(t)
		})
	}
}

func mockDBNodeConditions(now time.Time) db.NodeConditions {
	return db.NodeConditions{
		Pending: db.Condition{
			Status:             db.ConditionStatusFalse,
			Message:            "",
			LastTransitionTime: now,
		},
		ProvisioningClaimed: db.Condition{
			Status:             db.ConditionStatusTrue,
			Message:            "Provisioning workflow claimed node",
			LastTransitionTime: now,
		},
		Provisioning: db.Condition{
			Status:             db.ConditionStatusFalse,
			Message:            "Finished provisioning",
			LastTransitionTime: now,
		},
		ReadyToJoin: db.Condition{
			Status:             db.ConditionStatusTrue,
			Message:            "Ready to join",
			LastTransitionTime: now,
		},
		Joined: db.Condition{
			Status:             db.ConditionStatusTrue,
			Message:            "Joined",
			LastTransitionTime: now,
		},
		Provisioned: db.Condition{
			Status:             db.ConditionStatusTrue,
			Message:            "Provisioned",
			LastTransitionTime: now,
		},
		Deleting: db.Condition{
			Status:             db.ConditionStatusFalse,
			Message:            "Not deleting",
			LastTransitionTime: now,
		},
		KubernetesReady: db.Condition{
			Status:             db.ConditionStatusUnknown,
			Message:            "",
			LastTransitionTime: now,
		},
		KubernetesUnschedulable: db.Condition{
			Status:             db.ConditionStatusUnknown,
			Message:            "",
			LastTransitionTime: now,
		},
		NodeAgentReady: db.Condition{
			Status:             db.ConditionStatusUnknown,
			Message:            "",
			LastTransitionTime: now,
		},
		Rebooting: db.Condition{
			Status:             db.ConditionStatusFalse,
			Message:            "Not rebooting",
			LastTransitionTime: now,
		},
		MarkedForRecycling: db.Condition{
			Status:             db.ConditionStatusFalse,
			Message:            "Not marked for recycling",
			LastTransitionTime: now,
		},
		MarkedForRepair: db.Condition{
			Status:             db.ConditionStatusFalse,
			Message:            "Not marked for repair",
			LastTransitionTime: now,
		},
		TeleportRegistered: db.Condition{
			Status:             db.ConditionStatusFalse,
			Message:            "",
			LastTransitionTime: now,
		},
	}
}

func mockAPINodeConditions(now time.Time) api.NodeConditions {
	return api.NodeConditions{
		Pending: api.Condition{
			Status:             api.ConditionStatusFalse,
			Message:            "",
			LastTransitionTime: now,
		},
		ProvisioningClaimed: api.Condition{
			Status:             api.ConditionStatusTrue,
			Message:            "Provisioning workflow claimed node",
			LastTransitionTime: now,
		},
		Provisioning: api.Condition{
			Status:             api.ConditionStatusFalse,
			Message:            "Finished provisioning",
			LastTransitionTime: now,
		},
		ReadyToJoin: api.Condition{
			Status:             api.ConditionStatusTrue,
			Message:            "Ready to join",
			LastTransitionTime: now,
		},
		Joined: api.Condition{
			Status:             api.ConditionStatusTrue,
			Message:            "Joined",
			LastTransitionTime: now,
		},
		Provisioned: api.Condition{
			Status:             api.ConditionStatusTrue,
			Message:            "Provisioned",
			LastTransitionTime: now,
		},
		Deleting: api.Condition{
			Status:             api.ConditionStatusFalse,
			Message:            "Not deleting",
			LastTransitionTime: now,
		},
		// The db fixture leaves DeleteRequested unset, which apiConditionFromDB
		// renders as Unknown with a zero transition time.
		DeleteRequested: api.Condition{
			Status: api.ConditionStatusUnknown,
		},
		KubernetesReady: api.Condition{
			Status:             api.ConditionStatusUnknown,
			Message:            "",
			LastTransitionTime: now,
		},
		KubernetesUnschedulable: api.Condition{
			Status:             api.ConditionStatusUnknown,
			Message:            "",
			LastTransitionTime: now,
		},
		NodeAgentReady: api.Condition{
			Status:             api.ConditionStatusUnknown,
			Message:            "",
			LastTransitionTime: now,
		},
		Rebooting: api.Condition{
			Status:             api.ConditionStatusFalse,
			Message:            "Not rebooting",
			LastTransitionTime: now,
		},
		MarkedForRecycling: api.Condition{
			Status:             api.ConditionStatusFalse,
			Message:            "Not marked for recycling",
			LastTransitionTime: now,
		},
		MarkedForRepair: api.Condition{
			Status:             api.ConditionStatusFalse,
			Message:            "Not marked for repair",
			LastTransitionTime: now,
		},
		TeleportRegistered: &api.Condition{
			Status:             api.ConditionStatusFalse,
			Message:            "",
			LastTransitionTime: now,
		},
	}
}

func newMachineHealthIssue(category, summary string) *workflows.MachineHealthIssue {
	mhi := workflows.MachineHealthIssue{
		Category: category,
		Summary:  summary,
	}
	return &mhi
}

func TestNodeService_ListNodes_ControlPlaneRollout(t *testing.T) {
	clusterID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	nodePoolID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	desiredCfgID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	driftedCfgID := uuid.MustParse("33333333-3333-3333-3333-333333333333")

	driftedNodeID := uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000001")
	inSyncNodeID := uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000002")
	poolNodeID := uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000003")

	desiredSpec := db.ControlPlaneConfigSpec{KubernetesVersion: "1.34.2"}
	desiredSpec.SetNico(db.NicoControlPlaneProvider{Release: "os-v2", ConfigProfile: "cfg-v2"})
	driftedSpec := db.ControlPlaneConfigSpec{KubernetesVersion: "1.33.6"}
	driftedSpec.SetNico(db.NicoControlPlaneProvider{Release: "os-v1", ConfigProfile: "cfg-v1"})

	now := time.Now()
	dbNodes := []db.Node{
		// Control-plane-controller-managed (no node pool), pinned to an older
		// snapshot than the cluster desires -> rollout pending.
		{
			ID:                          driftedNodeID,
			ClusterID:                   clusterID,
			ControlPlane:                true,
			ControlPlaneConfigurationID: driftedCfgID,
			Conditions:                  mockDBNodeConditions(now),
		},
		// Control-plane-controller-managed, pinned to the cluster's desired
		// snapshot -> present but not pending.
		{
			ID:                          inSyncNodeID,
			ClusterID:                   clusterID,
			ControlPlane:                true,
			ControlPlaneConfigurationID: desiredCfgID,
			Conditions:                  mockDBNodeConditions(now),
		},
		// Node-pool-managed control-plane node -> no rollout status at all.
		{
			ID:                          poolNodeID,
			ClusterID:                   clusterID,
			NodePoolID:                  nodePoolID,
			ControlPlane:                true,
			ControlPlaneConfigurationID: driftedCfgID,
			Conditions:                  mockDBNodeConditions(now),
		},
	}

	mockDB := &mockDB{}
	mockDB.On("ListNodes", mock.Anything, mock.Anything).Return(dbNodes, nil)
	mockDB.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{
		ID:                    clusterID,
		EnableCPNodeRecycling: true,
		ControlPlaneConfig: db.ControlPlaneConfig{
			ConfigurationID:        desiredCfgID,
			ControlPlaneConfigSpec: desiredSpec,
		},
	}, nil)
	mockDB.On("ListControlPlaneConfigurationsByIDs", mock.Anything, mock.Anything).Return(
		[]db.ControlPlaneConfiguration{
			{ID: driftedCfgID, Config: driftedSpec},
			{ID: desiredCfgID, Config: desiredSpec},
		}, nil,
	)

	service := server.NewNodeService(&slog.Logger{}, mockDB)

	resp, err := service.ListNodes(context.Background(), api.ListNodesRequestObject{})
	require.NoError(t, err)

	byID := map[uuid.UUID]api.Node{}
	for _, n := range resp.(api.ListNodes200JSONResponse).Nodes {
		byID[n.ID] = n
	}

	drifted := byID[driftedNodeID]
	require.NotNil(t, drifted.ControlPlaneRollout)
	assert.True(t, drifted.ControlPlaneRollout.Pending)
	// Recycling is enabled, so the pending drift is actionable.
	assert.True(t, drifted.ControlPlaneRollout.Actionable)
	// The status carries the snapshot IDs so a caller can resolve the pinned and
	// desired configs via the config-history endpoint.
	assert.Equal(t, driftedCfgID, drifted.ControlPlaneRollout.PinnedConfigurationID)
	assert.Equal(t, desiredCfgID, drifted.ControlPlaneRollout.DesiredConfigurationID)
	// changes enumerates each drifted rollout-relevant field so a client can render
	// the drift without enumerating fields itself.
	assert.ElementsMatch(t, []api.ControlPlaneRolloutChange{
		{Field: "kubernetesVersion", From: "1.33.6", To: "1.34.2"},
		{Field: "release", From: "os-v1", To: "os-v2"},
		{Field: "configProfile", From: "cfg-v1", To: "cfg-v2"},
	}, drifted.ControlPlaneRollout.Changes)

	inSync := byID[inSyncNodeID]
	require.NotNil(t, inSync.ControlPlaneRollout)
	assert.False(t, inSync.ControlPlaneRollout.Pending)
	// Nothing to do, so not actionable even though recycling is enabled.
	assert.False(t, inSync.ControlPlaneRollout.Actionable)
	assert.Empty(t, inSync.ControlPlaneRollout.Changes)

	assert.Nil(t, byID[poolNodeID].ControlPlaneRollout, "node-pool-managed CP node carries no rollout status")

	mockDB.AssertExpectations(t)
	// Both controller-managed nodes share one cluster, so it is loaded once.
	mockDB.AssertNumberOfCalls(t, "GetCluster", 1)
}

func TestNodeService_GetNode_ControlPlaneRollout(t *testing.T) {
	clusterID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	nodeID := uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000001")
	desiredCfgID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	driftedCfgID := uuid.MustParse("33333333-3333-3333-3333-333333333333")

	desiredSpec := db.ControlPlaneConfigSpec{KubernetesVersion: "1.34.2"}
	desiredSpec.SetNico(db.NicoControlPlaneProvider{Release: "os-v2", ConfigProfile: "cfg-v2"})
	driftedSpec := db.ControlPlaneConfigSpec{KubernetesVersion: "1.34.2"}
	driftedSpec.SetNico(db.NicoControlPlaneProvider{Release: "os-v2", ConfigProfile: "cfg-v1"})

	now := time.Now()
	dbNode := db.Node{
		ID:                          nodeID,
		ClusterID:                   clusterID,
		ControlPlane:                true,
		ControlPlaneConfigurationID: driftedCfgID,
		Conditions:                  mockDBNodeConditions(now),
	}

	mockDB := &mockDB{}
	mockDB.On("GetNode", mock.Anything, nodeID).Return(dbNode, nil)
	mockDB.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{
		ID: clusterID,
		ControlPlaneConfig: db.ControlPlaneConfig{
			ConfigurationID:        desiredCfgID,
			ControlPlaneConfigSpec: desiredSpec,
		},
	}, nil)
	mockDB.On("ListControlPlaneConfigurationsByIDs", mock.Anything, mock.Anything).Return(
		[]db.ControlPlaneConfiguration{{ID: driftedCfgID, Config: driftedSpec}}, nil,
	)

	service := server.NewNodeService(&slog.Logger{}, mockDB)

	resp, err := service.GetNode(context.Background(), api.GetNodeRequestObject{NodeID: nodeID})
	require.NoError(t, err)

	node := api.Node(resp.(api.GetNode200JSONResponse))
	require.NotNil(t, node.ControlPlaneRollout)
	// Only the config profile drifted, which still requires a rollout.
	assert.True(t, node.ControlPlaneRollout.Pending)
	// Recycling is disabled on this cluster, so the drift is gated, not actionable.
	assert.False(t, node.ControlPlaneRollout.Actionable)
	// Only the config profile drifted, so changes carries exactly that field.
	assert.Equal(t, []api.ControlPlaneRolloutChange{
		{Field: "configProfile", From: "cfg-v1", To: "cfg-v2"},
	}, node.ControlPlaneRollout.Changes)
	assert.Equal(t, driftedCfgID, node.ControlPlaneRollout.PinnedConfigurationID)
	assert.Equal(t, desiredCfgID, node.ControlPlaneRollout.DesiredConfigurationID)
	mockDB.AssertExpectations(t)
}

// TestNodeService_DeleteControlPlaneNodeThroughController covers the API half of
// the controller-owned delete: a control-plane delete is
// accepted as intent (202, no teardown queued) or refused at the quorum floor
// (400), while a worker delete keeps starting its teardown inline.
func TestNodeService_DeleteControlPlaneNodeThroughController(t *testing.T) {
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440001")

	cpNode := func(n int, joined bool) db.Node {
		return db.Node{
			ID:           uuid.MustParse(fmt.Sprintf("550e8400-e29b-41d4-a716-44665544%04d", n)),
			ClusterID:    clusterID,
			ControlPlane: true,
			Name:         fmt.Sprintf("cp-%d", n),
			Conditions: db.NodeConditions{
				Joined: db.NewCondition(joined, "Joined"),
				// A joined control-plane node is a serving one. The quorum floor
				// reads health to tell a live member from a dead one, so a fixture
				// that omits it would read as unreachable and skip the floor.
				KubernetesReady: db.NewCondition(joined, "Ready"),
				NodeAgentReady:  db.NewCondition(joined, "Ready"),
			},
		}
	}

	fourNodeCP := []db.Node{cpNode(1, true), cpNode(2, true), cpNode(3, true), cpNode(4, true)}
	threeNodeCP := []db.Node{cpNode(1, true), cpNode(2, true), cpNode(3, true)}

	tests := []struct {
		name            string
		target          db.Node
		clusterNodes    []db.Node
		want400Contains string
		wantIntent      bool
		wantTeardown    bool
	}{
		{
			name:         "four-node control plane: delete accepted as intent",
			target:       fourNodeCP[0],
			clusterNodes: fourNodeCP,
			wantIntent:   true,
		},
		{
			name:            "three-node control plane: delete refused at the quorum floor",
			target:          threeNodeCP[0],
			clusterNodes:    threeNodeCP,
			want400Contains: "below the minimum of 3",
		},
		{
			name:         "worker delete still starts its teardown inline",
			target:       db.Node{ID: uuid.MustParse("550e8400-e29b-41d4-a716-446655449999"), ClusterID: clusterID, Name: "worker-1"},
			clusterNodes: threeNodeCP,
			wantTeardown: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tx := &mockTx{}
			tx.On("GetNode", mock.Anything, tt.target.ID).Return(tt.target, nil)
			tx.On("GetNodeForUpdate", mock.Anything, tt.target.ID).Return(tt.target, nil)
			tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID, ControlPlaneVIP: "10.0.0.100"}, nil)
			tx.On("ListNodePools", mock.Anything, mock.Anything).Return([]db.NodePool{}, nil).Maybe()
			tx.On("ListNodes", mock.Anything, mock.Anything).Return(tt.clusterNodes, nil).Maybe()

			var written *db.Node
			tx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(n *db.Node) bool {
				written = n
				return true
			})).Return(db.Node{}, nil).Maybe()
			tx.On("CreateWorkflowOutboxEntry",
				mock.Anything, constants.TeardownNodeWorkflowType, mock.Anything, mock.Anything,
			).Return(db.WorkflowOutbox{}, nil).Maybe()

			mockDB := &mockDB{MockTx: tx}
			mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

			service := server.NewNodeService(&slog.Logger{}, mockDB)

			resp, err := service.DeleteNode(context.Background(), api.DeleteNodeRequestObject{
				NodeID: tt.target.ID,
			})
			require.NoError(t, err)

			if tt.want400Contains != "" {
				require.IsType(t, api.DeleteNode400JSONResponse{}, resp)
				assert.Contains(t, resp.(api.DeleteNode400JSONResponse).Message, tt.want400Contains)
				tx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
				tx.AssertNotCalled(t, "CreateWorkflowOutboxEntry",
					mock.Anything, mock.Anything, mock.Anything, mock.Anything)
				return
			}

			require.IsType(t, api.DeleteNode202Response{}, resp)
			require.NotNil(t, written, "the delete should have written the node")

			if tt.wantIntent {
				assert.True(t, nodes.IsDeleteRequested(*written), "the delete should be recorded as intent")
				assert.False(t, nodes.IsDeleting(*written), "the teardown must not start inline")
				assert.NotEmpty(t, written.DeleteRequest, "the teardown input should be recorded")
				tx.AssertNotCalled(t, "CreateWorkflowOutboxEntry",
					mock.Anything, mock.Anything, mock.Anything, mock.Anything)
			}
			if tt.wantTeardown {
				assert.True(t, nodes.IsDeleting(*written), "a worker delete still starts its teardown")
				assert.False(t, nodes.IsDeleteRequested(*written))
				tx.AssertCalled(t, "CreateWorkflowOutboxEntry",
					mock.Anything, constants.TeardownNodeWorkflowType, mock.Anything, mock.Anything)
			}
		})
	}
}
