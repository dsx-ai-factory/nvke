// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

// fenceOwners are the operations whose retained ownership must fence control-plane mutations.
var fenceOwners = []db.OperationType{db.OperationTypeEtcdKeyRotation, db.OperationTypeCertRenewal}

func TestCredentialOperationFencesControlPlaneNodePoolAPIsAndPreservesWorkerChanges(t *testing.T) {
	for _, owner := range fenceOwners {
		for _, op := range []string{"update_cp", "delete_cp", "update_worker", "delete_worker"} {
			t.Run(string(owner)+"/"+op, func(t *testing.T) {
				cluster := db.Cluster{ID: uuid.New(), Name: "tenant", ProviderType: db.ProviderTypeNico, Conditions: db.ClusterConditions{UpdateInProgress: &db.UpdateInProgress{Type: owner}}}
				cp := op == "update_cp" || op == "delete_cp"
				pool := db.NodePool{ID: uuid.New(), ClusterID: cluster.ID, Name: "pool", ControlPlane: cp}
				pool.SetNico(db.NicoNodePoolProvider{})
				tx := &testutil.MockTx{}
				store := &testutil.MockDB{MockTx: tx}
				store.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
				clusterCall := tx.On("GetClusterForUpdate", mock.Anything, cluster.ID).Return(cluster, nil).Once()
				service := NewNodePoolService(slog.Default(), store, config.TeleportConfig{}, nil)
				tx.On("GetNodePool", mock.Anything, pool.ID).Return(pool, nil).Once()
				tx.On("GetNodePoolForUpdate", mock.Anything, pool.ID).Return(pool, nil).NotBefore(clusterCall).Once()
				if !cp {
					tx.On("UpdateNodePool", mock.Anything, mock.Anything).Return(pool, nil).Once()
				}
				if op == "update_cp" || op == "update_worker" {
					response, err := service.UpdateNodePool(t.Context(), api.UpdateNodePoolRequestObject{NodePoolID: pool.ID, Body: &api.UpdateNodePoolRequest{}})
					require.NoError(t, err)
					if cp {
						// Updating a control-plane pool is never valid, lock or not, so
						// this stays a 400 rather than a retryable 409.
						refused, ok := response.(api.UpdateNodePool400JSONResponse)
						require.True(t, ok, "expected a 400, got %T", response)
						require.Equal(t, errControlPlaneNodePoolNotAllowed.Error(), refused.Message)
					} else {
						require.IsType(t, api.UpdateNodePool200JSONResponse{}, response)
					}
				} else {
					if !cp {
						tx.On("CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(db.WorkflowOutbox{}, nil).Once()
					}
					response, err := service.DeleteNodePool(t.Context(), api.DeleteNodePoolRequestObject{NodePoolID: pool.ID})
					require.NoError(t, err)
					if cp {
						require.IsType(t, api.DeleteNodePool409JSONResponse{}, response)
					} else {
						require.IsType(t, api.DeleteNodePool202Response{}, response)
					}
				}
				if cp {
					tx.AssertNotCalled(t, "UpdateNodePool", mock.Anything, mock.Anything)
					tx.AssertNotCalled(t, "CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
				}
				tx.AssertExpectations(t)
				store.AssertExpectations(t)
			})
		}
	}
}

// TestRenewalFencesControlPlaneNodeDelete reports a blocked control-plane node
// delete as a retryable 409 and leaves worker deletes untouched.
func TestRenewalFencesControlPlaneNodeDelete(t *testing.T) {
	for _, owner := range fenceOwners {
		for _, controlPlane := range []bool{true, false} {
			name := "worker"
			if controlPlane {
				name = "control_plane"
			}
			t.Run(string(owner)+"/"+name, func(t *testing.T) {
				cluster := db.Cluster{
					ID:              uuid.New(),
					Name:            "tenant",
					ControlPlaneVIP: "10.0.0.100",
					Conditions:      db.ClusterConditions{UpdateInProgress: &db.UpdateInProgress{Type: owner}},
				}
				node := db.Node{ID: uuid.New(), ClusterID: cluster.ID, Name: "node", ControlPlane: controlPlane}
				tx := &testutil.MockTx{}
				store := &testutil.MockDB{MockTx: tx}
				store.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
				tx.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
				tx.On("GetClusterForUpdate", mock.Anything, cluster.ID).Return(cluster, nil).Once()
				tx.On("GetNodeForUpdate", mock.Anything, node.ID).Return(node, nil).Once()
				if !controlPlane {
					tx.On("UpdateNode", mock.Anything, mock.Anything).Return(db.Node{}, nil).Once()
					tx.On("CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(db.WorkflowOutbox{}, nil).Once()
				}
				service := NewNodeService(slog.Default(), store)

				response, err := service.DeleteNode(t.Context(), api.DeleteNodeRequestObject{NodeID: node.ID})

				require.NoError(t, err)
				if controlPlane {
					blocked, ok := response.(api.DeleteNode409JSONResponse)
					require.True(t, ok, "expected a 409, got %T", response)
					require.Contains(t, blocked.Message, "control-plane changes are rejected while etcd encryption, etcd key rotation or certificate renewal holds the cluster mutation lock; retry after the operation completes, or, if a failed certificate renewal retained the lock, after an operator clears it (see controller/docs/certificate-renewal.md)")
					tx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
					tx.AssertNotCalled(t, "CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
				} else {
					require.IsType(t, api.DeleteNode202Response{}, response)
				}
				tx.AssertExpectations(t)
			})
		}
	}
}

// TestClusterDeletionPreemptsCredentialOwners keeps deletion available while a
// credential cycle holds, or a failed cycle retains, the mutation lock.
func TestClusterDeletionPreemptsCredentialOwners(t *testing.T) {
	for _, owner := range fenceOwners {
		t.Run(string(owner), func(t *testing.T) {
			clusterID := uuid.New()
			tx := &testutil.MockTx{}
			store := &testutil.MockDB{MockTx: tx}
			store.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
			tx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID, Conditions: db.ClusterConditions{UpdateInProgress: &db.UpdateInProgress{Type: owner}}}, nil).Once()
			tx.On("UpdateCluster", mock.Anything, mock.MatchedBy(func(cluster *db.Cluster) bool {
				return cluster.Conditions.Deleting.Status == db.ConditionStatusTrue
			})).Return(db.Cluster{}, nil).Once()
			tx.On("CreateWorkflowOutboxEntry", mock.Anything, "TeardownCluster", mock.Anything, mock.Anything).Return(db.WorkflowOutbox{}, nil).Once()
			service := NewClusterService(slog.Default(), store, nil, config.TeleportConfig{}, false, nil, false, "")
			response, err := service.DeleteCluster(t.Context(), api.DeleteClusterRequestObject{ClusterID: clusterID})
			require.NoError(t, err)
			require.IsType(t, api.DeleteCluster202Response{}, response)
			tx.AssertExpectations(t)
		})
	}
}
