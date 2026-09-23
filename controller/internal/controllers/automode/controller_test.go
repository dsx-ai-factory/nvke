// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package automode

import (
	"context"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/constants"
	"github.com/dsx-ai-factory/nke/controller/internal/controllers/dbreconcile"
	"github.com/dsx-ai-factory/nke/controller/internal/controllers/dbreconcile/dbwrapper"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/temporal/workflows"
	"github.com/dsx-ai-factory/nke/controller/internal/testutil"
)

func TestUnit_ReconcileSkipsNodePools(t *testing.T) {
	ctx := context.Background()
	nodePoolID := uuid.New()
	currentNodeSetID := uuid.New()

	type parameters struct {
		nodePool db.NodePool
	}

	tests := map[string]parameters{
		"missing node pool": {
			nodePool: db.NodePool{},
		},
		"auto mode disabled": {
			nodePool: baseNodePool(nodePoolID, currentNodeSetID, func(nodePool *db.NodePool) {
				nodePool.AutoModeDisabled = true
			}),
		},
		"control plane": {
			nodePool: baseNodePool(nodePoolID, currentNodeSetID, func(nodePool *db.NodePool) {
				nodePool.ControlPlane = true
			}),
		},
		"deleting": {
			nodePool: baseNodePool(nodePoolID, currentNodeSetID, func(nodePool *db.NodePool) {
				nodePool.Conditions.Deleting = db.NewCondition(true, "Deleting")
			}),
		},
		"no recycle budget": {
			nodePool: baseNodePool(nodePoolID, currentNodeSetID, func(nodePool *db.NodePool) {
				nodePool.RecycleBudget = 0
			}),
		},
		"no current node set": {
			nodePool: baseNodePool(nodePoolID, currentNodeSetID, func(nodePool *db.NodePool) {
				nodePool.CurrentNodeSetID = nil
			}),
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			mockDB := &testutil.MockDB{}
			mockTx := &testutil.MockTx{}
			mockDB.MockTx = mockTx
			mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
			if name == "missing node pool" {
				mockTx.On("GetNodePoolForUpdate", mock.Anything, nodePoolID).Return(db.NodePool{}, db.ErrNotFound)
			} else {
				mockTx.On("GetNodePoolForUpdate", mock.Anything, nodePoolID).Return(params.nodePool, nil)
			}

			controller := NewController(config.Config{}, slog.Default(), dbwrapper.New(mockDB, slog.Default()))

			result, err := controller.Reconcile(ctx, dbreconcile.NodePoolKey(nodePoolID))

			require.NoError(t, err)
			assert.Equal(t, reconcile.Result{}, result)
			mockDB.AssertExpectations(t)
			mockTx.AssertExpectations(t)
			mockTx.AssertNotCalled(t, "ListNodes", mock.Anything, mock.Anything)
			mockTx.AssertNotCalled(t, "ListNodesEligibleForRecycling", mock.Anything, mock.Anything, mock.Anything)
			mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
			mockTx.AssertNotCalled(t, "CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
		})
	}
}

func TestUnit_ReconcileMarksDeprecatedNodesWithinBudget(t *testing.T) {
	ctx := context.Background()
	nodePoolID := uuid.New()
	currentNodeSetID := uuid.New()
	nodePool := baseNodePool(nodePoolID, currentNodeSetID)
	nodePool.DesiredCount = 3
	nodePool.RecycleBudget = 2
	availableNodes := []db.Node{
		{ID: uuid.New(), NodePoolID: nodePoolID, NodeSetID: currentNodeSetID},
		{ID: uuid.New(), NodePoolID: nodePoolID, NodeSetID: currentNodeSetID},
	}
	recyclingNode := db.Node{
		ID:         uuid.New(),
		NodePoolID: nodePoolID,
		NodeSetID:  uuid.New(),
		Name:       "worker-old-0",
	}

	mockDB := &testutil.MockDB{}
	mockTx := &testutil.MockTx{}
	mockDB.MockTx = mockTx
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
	mockTx.On("GetNodePoolForUpdate", mock.Anything, nodePoolID).Return(nodePool, nil)
	mockTx.On("ListNodes", mock.Anything, availableNodeFilters(nodePoolID)).Return(availableNodes, nil)
	mockTx.On("ListNodesEligibleForRecycling", mock.Anything, nodePoolID, 1).Return([]db.Node{recyclingNode}, nil)
	mockTx.On("GetNodeForUpdate", mock.Anything, recyclingNode.ID).Return(recyclingNode, nil)
	mockTx.On("GetNode", mock.Anything, recyclingNode.ID).Return(recyclingNode, nil)
	mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(node *db.Node) bool {
		return node.ID == recyclingNode.ID &&
			node.Conditions.MarkedForRecycling.Status == db.ConditionStatusTrue &&
			node.Conditions.MarkedForRecycling.Message == "Marked for auto-mode recycling"
	})).Return(recyclingNode, nil)
	mockTx.On(
		"CreateWorkflowOutboxEntry",
		mock.Anything,
		workflows.ApplyK8sNodeConditionWorkflowName,
		constants.ApplyK8sNodeConditionWorkflowPrefix+recyclingNode.ID.String(),
		mock.MatchedBy(func(input workflows.ApplyK8sNodeConditionInput) bool {
			return input.NodeID == recyclingNode.ID &&
				input.NodeCondition.Type == constants.NodeConditionTypeNeedsRecycle &&
				input.NodeCondition.Status == corev1.ConditionTrue &&
				input.NodeCondition.Reason == "Deprecated" &&
				input.NodeCondition.Message == "NKE auto-mode recycling deprecated node"
		}),
	).Return(db.WorkflowOutbox{}, nil)

	controller := NewController(config.Config{}, slog.Default(), dbwrapper.New(mockDB, slog.Default()))

	result, err := controller.Reconcile(ctx, dbreconcile.NodePoolKey(nodePoolID))

	require.NoError(t, err)
	assert.Equal(t, reconcile.Result{}, result)
	mockDB.AssertExpectations(t)
	mockTx.AssertExpectations(t)
}

func TestUnit_ReconcileSelfEnqueueIsIdempotent(t *testing.T) {
	ctx := context.Background()
	nodePoolID := uuid.New()
	currentNodeSetID := uuid.New()
	nodePool := baseNodePool(nodePoolID, currentNodeSetID)
	nodePool.DesiredCount = 1
	nodePool.RecycleBudget = 1
	recyclingNode := db.Node{
		ID:         uuid.New(),
		NodePoolID: nodePoolID,
		NodeSetID:  uuid.New(),
		Name:       "worker-old-0",
	}

	mockDB := &testutil.MockDB{}
	mockTx := &testutil.MockTx{}
	mockDB.MockTx = mockTx
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Twice()
	mockTx.On("GetNodePoolForUpdate", mock.Anything, nodePoolID).Return(nodePool, nil).Twice()
	mockTx.On("ListNodes", mock.Anything, availableNodeFilters(nodePoolID)).Return([]db.Node{{ID: uuid.New(), NodePoolID: nodePoolID}}, nil).Twice()
	mockTx.On("ListNodesEligibleForRecycling", mock.Anything, nodePoolID, 1).Return([]db.Node{recyclingNode}, nil).Once()
	mockTx.On("ListNodesEligibleForRecycling", mock.Anything, nodePoolID, 1).Return([]db.Node{}, nil).Once()
	mockTx.On("GetNodeForUpdate", mock.Anything, recyclingNode.ID).Return(recyclingNode, nil).Once()
	mockTx.On("GetNode", mock.Anything, recyclingNode.ID).Return(recyclingNode, nil).Once()
	mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(node *db.Node) bool {
		return node.ID == recyclingNode.ID &&
			node.Conditions.MarkedForRecycling.Status == db.ConditionStatusTrue
	})).Return(recyclingNode, nil).Once()
	mockTx.On("CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(db.WorkflowOutbox{}, nil).
		Once()

	controller := NewController(config.Config{}, slog.Default(), dbwrapper.New(mockDB, slog.Default()))

	_, err := controller.Reconcile(ctx, dbreconcile.NodePoolKey(nodePoolID))
	require.NoError(t, err)
	_, err = controller.Reconcile(ctx, dbreconcile.NodePoolKey(nodePoolID))
	require.NoError(t, err)

	mockDB.AssertExpectations(t)
	mockTx.AssertExpectations(t)
}

func TestUnit_NodePoolKeysForNodeSet(t *testing.T) {
	nodePoolID := uuid.New()

	keys, err := nodePoolKeysForNodeSet(context.Background(), db.NodeSet{NodePoolID: nodePoolID})
	require.NoError(t, err)
	assert.Equal(t,
		[]dbreconcile.Key{dbreconcile.NodePoolKey(nodePoolID)},
		keys,
	)
	keys, err = nodePoolKeysForNodeSet(context.Background(), db.NodeSet{})
	require.NoError(t, err)
	assert.Nil(t, keys)
}

func TestUnit_NodePoolKeysForNode(t *testing.T) {
	nodePoolID := uuid.New()

	keys, err := nodePoolKeysForNode(context.Background(), db.Node{NodePoolID: nodePoolID})
	require.NoError(t, err)
	assert.Equal(t,
		[]dbreconcile.Key{dbreconcile.NodePoolKey(nodePoolID)},
		keys,
	)
	keys, err = nodePoolKeysForNode(context.Background(), db.Node{})
	require.NoError(t, err)
	assert.Nil(t, keys)
}

func baseNodePool(id, currentNodeSetID uuid.UUID, opts ...func(*db.NodePool)) db.NodePool {
	nodePool := db.NodePool{
		ID:               id,
		ClusterID:        uuid.New(),
		Name:             "workers",
		DesiredCount:     1,
		RecycleBudget:    1,
		CurrentNodeSetID: &currentNodeSetID,
		Conditions:       db.NodePoolConditions{},
	}
	for _, opt := range opts {
		opt(&nodePool)
	}
	return nodePool
}

func availableNodeFilters(nodePoolID uuid.UUID) db.NodeFilters {
	return db.NodeFilters{
		NodePoolID:              &nodePoolID,
		KubernetesReady:         ptrTo(true),
		KubernetesUnschedulable: ptrTo(false),
		Deleting:                ptrTo(false),
		MarkedForRecycling:      ptrTo(false),
		MarkedForRepair:         ptrTo(false),
	}
}

func ptrTo[T any](v T) *T {
	return &v
}
