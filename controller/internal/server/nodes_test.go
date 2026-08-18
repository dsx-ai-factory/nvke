// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/internal/testutil"

	"github.com/NVIDIA/nke/controller/api"
	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/server"
	"github.com/NVIDIA/nke/controller/internal/temporal/workflows"
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

			service := server.NewNodeService(&slog.Logger{}, mockDB, nil, false)

			resp, err := service.ListNodes(context.Background(), api.ListNodesRequestObject{
				Params: api.ListNodesParams{
					ClusterID:   tt.filters.ClusterID,
					NodePoolID:  tt.filters.NodePoolID,
					ClusterName: tt.filters.ClusterName,
					Name:        tt.filters.Name,
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

			service := server.NewNodeService(&slog.Logger{}, mockDB, nil, false)

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
		name                     string
		nodeID                   uuid.UUID
		machineHealthIssue       *api.MachineHealthIssue
		forceUnreachable         bool
		allowQuorumLoss          bool
		allowSingleVoter         bool
		allowEtcdSafetyOverrides bool
		setupMockCalls           func(*mockTx)
		wantError                bool
		want400                  bool
		want400Contains          string
		want404                  bool
		verifyConditions         func(t *testing.T, conditions db.NodeConditions)
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
					ID: clusterID,
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
			name:   "success control-plane node when another joined CP remains",
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
					ID: clusterID,
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
				tx.On("UpdateNode", mock.Anything, mock.Anything).Return(db.Node{}, nil)
				tx.On("CreateWorkflowOutboxEntry", mock.Anything, constants.TeardownNodeWorkflowType, mock.Anything, mock.MatchedBy(func(input workflows.TeardownNodeInput) bool {
					return input.NodeID == nodeID
				})).Return(db.WorkflowOutbox{}, nil)
			},
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
		{
			name:                     "success single-voter rotation with override enabled",
			nodeID:                   nodeID,
			forceUnreachable:         true,
			allowSingleVoter:         true,
			allowEtcdSafetyOverrides: true,
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
					AllowSingleVoter: true,
				}).Return(db.WorkflowOutbox{}, nil)
			},
		},
		{
			name:             "Bad request - single-voter override rejected when gate disabled",
			nodeID:           nodeID,
			allowSingleVoter: true,
			want400:          true,
		},
		{
			name:            "Bad request - quorum-loss override rejected when gate disabled",
			nodeID:          nodeID,
			allowQuorumLoss: true,
			want400:         true,
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

			service := server.NewNodeService(&slog.Logger{}, mockDB, nil, tt.allowEtcdSafetyOverrides)

			var body *api.DeleteNodeJSONRequestBody
			if tt.machineHealthIssue != nil || tt.forceUnreachable || tt.allowQuorumLoss || tt.allowSingleVoter {
				body = &api.DeleteNodeJSONRequestBody{
					MachineHealthIssue: tt.machineHealthIssue,
					ForceUnreachable:   tt.forceUnreachable,
					AllowQuorumLoss:    tt.allowQuorumLoss,
					AllowSingleVoter:   tt.allowSingleVoter,
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

func TestNodeService_GetNodeHistory(t *testing.T) {
	nodeID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")

	t.Run("returns empty workflows when temporal client is nil", func(t *testing.T) {
		mockDB := &mockDB{}
		mockDB.On("GetNode", mock.Anything, nodeID).Return(db.Node{ID: nodeID}, nil)

		service := server.NewNodeService(&slog.Logger{}, mockDB, nil, false)
		resp, err := service.GetNodeHistory(context.Background(), api.GetNodeHistoryRequestObject{
			NodeID: nodeID,
		})

		require.NoError(t, err)
		result, ok := resp.(api.GetNodeHistory200JSONResponse)
		require.True(t, ok)
		assert.NotNil(t, result.Workflows)
		assert.Empty(t, result.Workflows)
		mockDB.AssertExpectations(t)
	})

	t.Run("returns 404 when node does not exist", func(t *testing.T) {
		mockDB := &mockDB{}
		mockDB.On("GetNode", mock.Anything, nodeID).Return(db.Node{}, db.ErrNotFound)

		service := server.NewNodeService(&slog.Logger{}, mockDB, nil, false)
		resp, err := service.GetNodeHistory(context.Background(), api.GetNodeHistoryRequestObject{
			NodeID: nodeID,
		})

		require.NoError(t, err)
		assert.IsType(t, api.GetNodeHistory404Response{}, resp)
		mockDB.AssertExpectations(t)
	})
}

func mockDBNodeConditions(now time.Time) db.NodeConditions {
	return db.NodeConditions{
		Pending: db.Condition{
			Status:             db.ConditionStatusFalse,
			Message:            "",
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
