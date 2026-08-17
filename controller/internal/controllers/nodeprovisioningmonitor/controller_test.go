// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package nodeprovisioningmonitor_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile"
	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile/dbwrapper"
	"github.com/NVIDIA/nke/controller/internal/controllers/nodeprovisioningmonitor"
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

func TestController_Reconcile(t *testing.T) {
	t.Run("skips when timeout disabled", func(t *testing.T) {
		mockDB := &testutil.MockDB{}
		controller := nodeprovisioningmonitor.NewController(
			config.Config{},
			slog.Default(),
			dbwrapper.New(mockDB, slog.Default()),
		)

		result, err := controller.Reconcile(context.Background(), dbreconcile.NodeKey(uuid.New()))
		require.NoError(t, err)
		assert.Zero(t, result)
		mockDB.AssertNotCalled(t, "RunInTx", mock.Anything, mock.Anything)
	})

	t.Run("requeues node still within timeout", func(t *testing.T) {
		ctx := context.Background()
		nodeID := uuid.New()
		timeout := 30 * time.Minute
		provisioningStartedAt := time.Now().Add(-5 * time.Minute)
		node := db.Node{
			ID:        nodeID,
			Name:      "worker-0",
			CreatedAt: provisioningStartedAt,
			Conditions: db.NodeConditions{
				Provisioning: db.Condition{
					Status:             db.ConditionStatusTrue,
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
		mockTx.On("GetNodeForUpdate", mock.Anything, nodeID).Return(node, nil)

		controller := nodeprovisioningmonitor.NewController(
			config.Config{NodeProvisioningTimeout: timeout},
			slog.Default(),
			dbwrapper.New(mockDB, slog.Default()),
		)

		result, err := controller.Reconcile(ctx, dbreconcile.NodeKey(nodeID))
		require.NoError(t, err)
		expected := timeout - time.Since(provisioningStartedAt)
		assert.InDelta(t, expected.Seconds(), result.RequeueAfter.Seconds(), 2.0)
		assert.Positive(t, result.RequeueAfter)
		mockDB.AssertExpectations(t)
		mockTx.AssertExpectations(t)
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
		mockTx.On("GetNodeForUpdate", mock.Anything, nodeID).Return(node, nil)
		mockNodeContextFromPool(mockTx, nodeSetID, nodePool)
		mockTx.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{
			ID:     clusterID,
			SiteID: siteID,
			Name:   "test-cluster",
		}, nil)
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

		controller := nodeprovisioningmonitor.NewController(
			config.Config{NodeProvisioningTimeout: timeout},
			slog.Default(),
			dbwrapper.New(mockDB, slog.Default()),
		)

		result, err := controller.Reconcile(ctx, dbreconcile.NodeKey(nodeID))
		require.NoError(t, err)
		assert.Zero(t, result)
		mockDB.AssertExpectations(t)
		mockTx.AssertExpectations(t)
	})
}
