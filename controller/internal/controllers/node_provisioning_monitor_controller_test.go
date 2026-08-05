// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package controllers_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/controllers"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/temporal/workflows"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func mockNodeContextFromPool(mockTx *testutil.MockTx, nodeSetID uuid.UUID, nodePool db.NodePool) {
	mockTx.On("GetNodeSet", mock.Anything, nodeSetID).Return(db.NodeSet{
		ID:         nodeSetID,
		NodePoolID: nodePool.ID,
	}, nil)
	mockTx.On("GetNodePool", mock.Anything, nodePool.ID).Return(nodePool, nil)
}

func TestNodeProvisioningMonitorController_Reconcile(t *testing.T) {
	t.Run("skips when timeout disabled", func(t *testing.T) {
		mockDB := &testutil.MockDB{}
		controller := controllers.NewNodeProvisioningMonitorController(config.Config{}, slog.Default(), mockDB)

		err := controller.Reconcile(context.Background(), db.Site{ID: uuid.New()})
		require.NoError(t, err)
		mockDB.AssertNotCalled(t, "RunInTx", mock.Anything, mock.Anything)
	})

	t.Run("skips stale control plane node", func(t *testing.T) {
		ctx := context.Background()
		siteID := uuid.New()
		clusterID := uuid.New()
		nodePoolID := uuid.New()
		nodeSetID := uuid.New()
		nodeID := uuid.New()
		timeout := 30 * time.Minute
		provisioningStartedAt := time.Now().Add(-2 * timeout)

		nodePool := db.NodePool{
			ID:           nodePoolID,
			ClusterID:    clusterID,
			Name:         "control-plane",
			ControlPlane: true,
			DesiredCount: 3,
			Conditions:   db.NodePoolConditions{},
		}
		node := db.Node{
			ID:         nodeID,
			NodePoolID: nodePoolID,
			NodeSetID:  nodeSetID,
			Name:       "cp-0",
			CreatedAt:  provisioningStartedAt,
			Conditions: db.NodeConditions{
				Provisioning: db.Condition{
					Status:             db.ConditionStatusTrue,
					Message:            "Waiting for Forge instance to be ready",
					LastTransitionTime: provisioningStartedAt,
				},
				Provisioned: db.NewCondition(false, "Not yet provisioned"),
				Deleting:    db.NewCondition(false, "Node is not being deleted"),
			},
		}

		mockDB := &testutil.MockDB{}
		mockTx := &testutil.MockTx{}
		mockDB.MockTx = mockTx

		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

		mockTx.On("ListNodes", mock.Anything, mock.MatchedBy(func(f db.NodeFilters) bool {
			return f.SiteID != nil && *f.SiteID == siteID &&
				f.Provisioning != nil && *f.Provisioning &&
				f.Deleting != nil && !*f.Deleting
		})).Return([]db.Node{node}, nil)
		mockNodeContextFromPool(mockTx, nodeSetID, nodePool)

		controller := controllers.NewNodeProvisioningMonitorController(
			config.Config{NodeProvisioningTimeout: timeout},
			slog.Default(),
			mockDB,
		)

		err := controller.Reconcile(ctx, db.Site{ID: siteID})
		require.NoError(t, err)
		mockDB.AssertExpectations(t)
		mockTx.AssertExpectations(t)
		mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
		mockTx.AssertNotCalled(t, "GetCluster", mock.Anything, mock.Anything)
		mockTx.AssertNotCalled(t, "CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("skips control plane node already marked for recycling", func(t *testing.T) {
		ctx := context.Background()
		siteID := uuid.New()
		clusterID := uuid.New()
		nodePoolID := uuid.New()
		nodeSetID := uuid.New()
		nodeID := uuid.New()
		timeout := 30 * time.Minute
		provisioningStartedAt := time.Now().Add(-2 * timeout)

		nodePool := db.NodePool{
			ID:           nodePoolID,
			ClusterID:    clusterID,
			Name:         "control-plane",
			ControlPlane: true,
			DesiredCount: 3,
			Conditions:   db.NodePoolConditions{},
		}
		node := db.Node{
			ID:         nodeID,
			NodePoolID: nodePoolID,
			NodeSetID:  nodeSetID,
			Name:       "cp-0",
			CreatedAt:  provisioningStartedAt,
			Conditions: db.NodeConditions{
				Provisioning: db.Condition{
					Status:             db.ConditionStatusTrue,
					Message:            "Waiting for Forge instance to be ready",
					LastTransitionTime: provisioningStartedAt,
				},
				Provisioned:        db.NewCondition(false, "Not yet provisioned"),
				Deleting:           db.NewCondition(false, "Node is not being deleted"),
				MarkedForRecycling: db.NewCondition(true, "Node provisioning exceeded timeout of 30m0s"),
			},
		}

		mockDB := &testutil.MockDB{}
		mockTx := &testutil.MockTx{}
		mockDB.MockTx = mockTx

		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

		mockTx.On("ListNodes", mock.Anything, mock.MatchedBy(func(f db.NodeFilters) bool {
			return f.SiteID != nil && *f.SiteID == siteID &&
				f.Provisioning != nil && *f.Provisioning &&
				f.Deleting != nil && !*f.Deleting
		})).Return([]db.Node{node}, nil)
		mockNodeContextFromPool(mockTx, nodeSetID, nodePool)

		controller := controllers.NewNodeProvisioningMonitorController(
			config.Config{NodeProvisioningTimeout: timeout},
			slog.Default(),
			mockDB,
		)

		err := controller.Reconcile(ctx, db.Site{ID: siteID})
		require.NoError(t, err)
		mockDB.AssertExpectations(t)
		mockTx.AssertExpectations(t)
		// Must not update the node or create any workflow
		mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
		mockTx.AssertNotCalled(t, "GetCluster", mock.Anything, mock.Anything)
		mockTx.AssertNotCalled(t, "CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("does not recycle no-nodepool control plane node when cluster config pauses monitor", func(t *testing.T) {
		ctx := context.Background()
		siteID := uuid.New()
		clusterID := uuid.New()
		nodeID := uuid.New()
		timeout := 30 * time.Minute
		provisioningStartedAt := time.Now().Add(-2 * timeout)

		node := db.Node{
			ID:           nodeID,
			ClusterID:    clusterID,
			ControlPlane: true,
			Name:         "cp-no-pool",
			CreatedAt:    provisioningStartedAt,
			Conditions: db.NodeConditions{
				Provisioning: db.Condition{
					Status:             db.ConditionStatusTrue,
					Message:            "Waiting for Forge instance to be ready",
					LastTransitionTime: provisioningStartedAt,
				},
				Provisioned: db.NewCondition(false, "Not yet provisioned"),
				Deleting:    db.NewCondition(false, "Node is not being deleted"),
			},
		}

		mockDB := &testutil.MockDB{}
		mockTx := &testutil.MockTx{}
		mockDB.MockTx = mockTx

		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
		mockTx.On("ListNodes", mock.Anything, mock.MatchedBy(func(f db.NodeFilters) bool {
			return f.SiteID != nil && *f.SiteID == siteID &&
				f.Provisioning != nil && *f.Provisioning &&
				f.Deleting != nil && !*f.Deleting
		})).Return([]db.Node{node}, nil)
		mockTx.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{
			ID: clusterID,
			ControlPlaneConfig: db.ControlPlaneConfig{
				ControlPlaneConfigSpec: db.ControlPlaneConfigSpec{
					ProvisioningMonitorDisabled: true,
				},
			},
		}, nil)

		controller := controllers.NewNodeProvisioningMonitorController(
			config.Config{NodeProvisioningTimeout: timeout},
			slog.Default(),
			mockDB,
		)

		err := controller.Reconcile(ctx, db.Site{ID: siteID})
		require.NoError(t, err)
		mockDB.AssertExpectations(t)
		mockTx.AssertExpectations(t)
		mockTx.AssertNotCalled(t, "GetNodeSet", mock.Anything, mock.Anything)
		mockTx.AssertNotCalled(t, "GetNodePool", mock.Anything, mock.Anything)
		mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
		mockTx.AssertNotCalled(t, "CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("does not recycle node when provisioning monitor is paused", func(t *testing.T) {
		ctx := context.Background()
		siteID := uuid.New()
		clusterID := uuid.New()
		nodePoolID := uuid.New()
		nodeSetID := uuid.New()
		nodeID := uuid.New()
		timeout := 30 * time.Minute
		provisioningStartedAt := time.Now().Add(-2 * timeout)

		nodePool := db.NodePool{
			ID:                          nodePoolID,
			ClusterID:                   clusterID,
			Name:                        "workers",
			DesiredCount:                1,
			ProvisioningMonitorDisabled: true,
			Conditions:                  db.NodePoolConditions{},
		}
		node := db.Node{
			ID:         nodeID,
			NodePoolID: nodePoolID,
			NodeSetID:  nodeSetID,
			Name:       "worker-0",
			CreatedAt:  provisioningStartedAt,
			Conditions: db.NodeConditions{
				Provisioning: db.Condition{
					Status:             db.ConditionStatusTrue,
					Message:            "Waiting for Forge instance to be ready",
					LastTransitionTime: provisioningStartedAt,
				},
				Provisioned: db.NewCondition(false, "Not yet provisioned"),
				Deleting:    db.NewCondition(false, "Node is not being deleted"),
			},
		}

		mockDB := &testutil.MockDB{}
		mockTx := &testutil.MockTx{}
		mockDB.MockTx = mockTx

		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
		mockTx.On("ListNodes", mock.Anything, mock.MatchedBy(func(f db.NodeFilters) bool {
			return f.SiteID != nil && *f.SiteID == siteID &&
				f.Provisioning != nil && *f.Provisioning &&
				f.Deleting != nil && !*f.Deleting
		})).Return([]db.Node{node}, nil)
		mockNodeContextFromPool(mockTx, nodeSetID, nodePool)

		controller := controllers.NewNodeProvisioningMonitorController(
			config.Config{NodeProvisioningTimeout: timeout},
			slog.Default(),
			mockDB,
		)

		err := controller.Reconcile(ctx, db.Site{ID: siteID})
		require.NoError(t, err)
		mockDB.AssertExpectations(t)
		mockTx.AssertExpectations(t)
		// Paused pool: must not delete the node, fetch the cluster, or queue any teardown.
		mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
		mockTx.AssertNotCalled(t, "GetCluster", mock.Anything, mock.Anything)
		mockTx.AssertNotCalled(t, "CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("recycles stale worker node", func(t *testing.T) {
		ctx := context.Background()
		siteID := uuid.New()
		clusterID := uuid.New()
		nodePoolID := uuid.New()
		nodeSetID := uuid.New()
		nodeID := uuid.New()
		timeout := 30 * time.Minute
		provisioningStartedAt := time.Now().Add(-2 * timeout)

		nodePool := db.NodePool{
			ID:           nodePoolID,
			ClusterID:    clusterID,
			Name:         "workers",
			DesiredCount: 1,
			Conditions:   db.NodePoolConditions{},
		}
		node := db.Node{
			ID:         nodeID,
			NodePoolID: nodePoolID,
			NodeSetID:  nodeSetID,
			Name:       "worker-0",
			CreatedAt:  provisioningStartedAt,
			Conditions: db.NodeConditions{
				Provisioning: db.Condition{
					Status:             db.ConditionStatusTrue,
					Message:            "Waiting for Forge instance to be ready",
					LastTransitionTime: provisioningStartedAt,
				},
				Provisioned: db.NewCondition(false, "Not yet provisioned"),
				Deleting:    db.NewCondition(false, "Node is not being deleted"),
			},
		}

		mockDB := &testutil.MockDB{}
		mockTx := &testutil.MockTx{}
		mockDB.MockTx = mockTx

		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

		mockTx.On("ListNodes", mock.Anything, mock.MatchedBy(func(f db.NodeFilters) bool {
			return f.SiteID != nil && *f.SiteID == siteID &&
				f.Provisioning != nil && *f.Provisioning &&
				f.Deleting != nil && !*f.Deleting
		})).Return([]db.Node{node}, nil)
		mockNodeContextFromPool(mockTx, nodeSetID, nodePool)
		mockTx.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{
			ID:   clusterID,
			Name: "test-cluster",
		}, nil)
		mockTx.On("GetNodeForUpdate", mock.Anything, nodeID).Return(node, nil)
		mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(updated *db.Node) bool {
			return updated.ID == nodeID &&
				updated.Conditions.Deleting.Status == db.ConditionStatusTrue &&
				updated.Conditions.Deleting.Message == "Node deletion in progress"
		})).Return(node, nil)
		mockTx.On(
			"CreateWorkflowOutboxEntry",
			mock.Anything,
			constants.TeardownNodeWorkflowType,
			constants.TeardownNodeWorkflowPrefix+nodeID.String(),
			mock.MatchedBy(func(input workflows.TeardownNodeInput) bool {
				return input.NodeID == nodeID && input.Reason == "Node provisioning exceeded timeout of 30m0s"
			}),
		).Return(db.WorkflowOutbox{}, nil)

		controller := controllers.NewNodeProvisioningMonitorController(
			config.Config{NodeProvisioningTimeout: timeout},
			slog.Default(),
			mockDB,
		)

		err := controller.Reconcile(ctx, db.Site{ID: siteID})
		require.NoError(t, err)
		mockDB.AssertExpectations(t)
		mockTx.AssertExpectations(t)
	})

	t.Run("does not recycle node still within timeout", func(t *testing.T) {
		ctx := context.Background()
		siteID := uuid.New()
		nodePoolID := uuid.New()
		timeout := 30 * time.Minute
		provisioningStartedAt := time.Now().Add(-5 * time.Minute)

		node := db.Node{
			ID:         uuid.New(),
			NodePoolID: nodePoolID,
			Name:       "worker-0",
			CreatedAt:  provisioningStartedAt,
			Conditions: db.NodeConditions{
				Provisioning: db.Condition{
					Status:             db.ConditionStatusTrue,
					Message:            "Waiting for Forge instance to be ready",
					LastTransitionTime: provisioningStartedAt,
				},
				Provisioned: db.NewCondition(false, "Not yet provisioned"),
				Deleting:    db.NewCondition(false, "Node is not being deleted"),
			},
		}

		mockDB := &testutil.MockDB{}
		mockTx := &testutil.MockTx{}
		mockDB.MockTx = mockTx

		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
		mockTx.On("ListNodes", mock.Anything, mock.MatchedBy(func(f db.NodeFilters) bool {
			return f.SiteID != nil && *f.SiteID == siteID &&
				f.Provisioning != nil && *f.Provisioning &&
				f.Deleting != nil && !*f.Deleting
		})).Return([]db.Node{node}, nil)

		controller := controllers.NewNodeProvisioningMonitorController(
			config.Config{NodeProvisioningTimeout: timeout},
			slog.Default(),
			mockDB,
		)

		err := controller.Reconcile(ctx, db.Site{ID: siteID})
		require.NoError(t, err)
		mockDB.AssertExpectations(t)
		mockTx.AssertExpectations(t)
		mockTx.AssertNotCalled(t, "CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})
}
