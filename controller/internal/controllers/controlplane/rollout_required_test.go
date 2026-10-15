// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestControlPlaneRolloutRequiredMessage(t *testing.T) {
	t.Parallel()

	assert.Equal(t,
		"Pinned control-plane configuration matches the cluster's desired configuration",
		controlPlaneRolloutRequiredMessage(nil),
	)

	msg := controlPlaneRolloutRequiredMessage([]db.ControlPlaneRolloutChange{
		{Field: "kubernetesVersion", From: "1.33.6", To: "1.34.2"},
		{Field: "release", From: "os-v1", To: "os-v2"},
	})
	assert.Contains(t, msg, "differs from desired")
	assert.Contains(t, msg, "kubernetesVersion 1.33.6->1.34.2")
	assert.Contains(t, msg, "release os-v1->os-v2")
}

func TestRefreshControlPlaneRolloutRequiredCondition(t *testing.T) {
	desired := db.ControlPlaneConfigSpec{KubernetesVersion: "1.34.2"}
	desired.SetNico(db.NicoControlPlaneProvider{Release: "os-v2", ConfigProfile: "cfg-v2"})
	drifted := db.ControlPlaneConfigSpec{KubernetesVersion: "1.33.6"}
	drifted.SetNico(db.NicoControlPlaneProvider{Release: "os-v1", ConfigProfile: "cfg-v1"})
	desiredID := uuid.New()
	driftedID := uuid.New()

	newCP := func() *controlPlane {
		return &controlPlane{
			nodePinnedControlPlaneConfigs: map[uuid.UUID]db.ControlPlaneConfigSpec{
				desiredID: desired,
				driftedID: drifted,
			},
			clusterControlPlaneConfig: clusterWithCurrentSnapshot(desired, 0).ControlPlaneConfig,
		}
	}

	t.Run("converged node is not rollout-required and is idempotent", func(t *testing.T) {
		cp := newCP()
		node := makeCPControllerNode("cp-in-sync", desiredID)

		assert.True(t, cp.refreshControlPlaneRolloutRequiredCondition(&node))
		assert.Equal(t, db.ConditionStatusFalse, node.Conditions.ControlPlaneRolloutRequired.Status)
		assert.False(t, node.Conditions.ControlPlaneRolloutRequired.LastTransitionTime.IsZero())

		// Second refresh is a no-op once the condition already reflects reality.
		assert.False(t, cp.refreshControlPlaneRolloutRequiredCondition(&node))
	})

	t.Run("drifted node is rollout-required with a diff message", func(t *testing.T) {
		cp := newCP()
		node := makeCPControllerNode("cp-drifted", driftedID)

		assert.True(t, cp.refreshControlPlaneRolloutRequiredCondition(&node))
		assert.Equal(t, db.ConditionStatusTrue, node.Conditions.ControlPlaneRolloutRequired.Status)
		assert.Contains(t, node.Conditions.ControlPlaneRolloutRequired.Message, "differs from desired")
		assert.False(t, cp.refreshControlPlaneRolloutRequiredCondition(&node))
	})

	t.Run("status flip bumps LastTransitionTime", func(t *testing.T) {
		cp := newCP()
		node := makeCPControllerNode("cp-flip", desiredID)
		old := time.Now().Add(-time.Hour)
		node.Conditions.ControlPlaneRolloutRequired = db.Condition{
			Status:             db.ConditionStatusFalse,
			Message:            controlPlaneRolloutRequiredMessage(nil),
			LastTransitionTime: old,
		}

		// Re-point the node at the drifted snapshot: False -> True.
		node.ControlPlaneConfigurationID = driftedID
		require.True(t, cp.refreshControlPlaneRolloutRequiredCondition(&node))
		assert.Equal(t, db.ConditionStatusTrue, node.Conditions.ControlPlaneRolloutRequired.Status)
		assert.True(t, node.Conditions.ControlPlaneRolloutRequired.LastTransitionTime.After(old))
	})

	t.Run("node-pool-managed node is left untouched", func(t *testing.T) {
		cp := newCP()
		node := makeCPControllerNode("cp-pool", driftedID)
		node.NodePoolID = uuid.New() // now node-pool-managed, not CP-controller-managed

		assert.False(t, cp.refreshControlPlaneRolloutRequiredCondition(&node))
		assert.Empty(t, node.Conditions.ControlPlaneRolloutRequired.Status)
	})

	t.Run("node with no preloaded pinned snapshot is left untouched", func(t *testing.T) {
		cp := newCP()
		node := makeCPControllerNode("cp-unknown", uuid.New())

		assert.False(t, cp.refreshControlPlaneRolloutRequiredCondition(&node))
		assert.Empty(t, node.Conditions.ControlPlaneRolloutRequired.Status)
	})
}

func TestReconcileControlPlaneRolloutRequiredCondition(t *testing.T) {
	desired := db.ControlPlaneConfigSpec{KubernetesVersion: "1.34.2"}
	desired.SetNico(db.NicoControlPlaneProvider{Release: "os-v2", ConfigProfile: "cfg-v2"})
	drifted := db.ControlPlaneConfigSpec{KubernetesVersion: "1.33.6"}
	drifted.SetNico(db.NicoControlPlaneProvider{Release: "os-v1", ConfigProfile: "cfg-v1"})
	desiredID := uuid.New()
	driftedID := uuid.New()

	newReconcilerAndCP := func(mockTx *testutil.MockTx, nodes []db.Node) (*reconciler, *controlPlane) {
		r := &reconciler{
			cfg:    config.Config{},
			logger: slog.New(slog.DiscardHandler),
			db:     mockTx,
		}
		cp := &controlPlane{
			nodes: nodes,
			nodePinnedControlPlaneConfigs: map[uuid.UUID]db.ControlPlaneConfigSpec{
				desiredID: desired,
				driftedID: drifted,
			},
			clusterControlPlaneConfig: clusterWithCurrentSnapshot(desired, 0).ControlPlaneConfig,
		}
		return r, cp
	}

	t.Run("drifted node is locked and persisted as rollout-required", func(t *testing.T) {
		node := makeCPControllerNode("cp-drifted", driftedID)

		mockTx := &testutil.MockTx{}
		mockTx.On("GetNodeForUpdate", mock.Anything, node.ID).Return(node, nil).Once()
		mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(n *db.Node) bool {
			return n.ID == node.ID && n.Conditions.ControlPlaneRolloutRequired.Status == db.ConditionStatusTrue
		})).Return(db.Node{}, nil).Once()

		r, cp := newReconcilerAndCP(mockTx, []db.Node{node})
		require.NoError(t, r.reconcileControlPlaneRolloutRequiredCondition(context.Background(), cp))
		mockTx.AssertExpectations(t)
	})

	t.Run("already converged node takes no lock and no write", func(t *testing.T) {
		node := makeCPControllerNode("cp-in-sync", desiredID)
		node.Conditions.ControlPlaneRolloutRequired = db.Condition{
			Status:             db.ConditionStatusFalse,
			Message:            controlPlaneRolloutRequiredMessage(nil),
			LastTransitionTime: time.Now(),
		}

		mockTx := &testutil.MockTx{}
		r, cp := newReconcilerAndCP(mockTx, []db.Node{node})
		require.NoError(t, r.reconcileControlPlaneRolloutRequiredCondition(context.Background(), cp))
		mockTx.AssertNotCalled(t, "GetNodeForUpdate", mock.Anything, mock.Anything)
		mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
	})

	t.Run("node-pool-managed node is skipped", func(t *testing.T) {
		node := makeCPControllerNode("cp-pool", driftedID)
		node.NodePoolID = uuid.New()

		mockTx := &testutil.MockTx{}
		r, cp := newReconcilerAndCP(mockTx, []db.Node{node})
		require.NoError(t, r.reconcileControlPlaneRolloutRequiredCondition(context.Background(), cp))
		mockTx.AssertNotCalled(t, "GetNodeForUpdate", mock.Anything, mock.Anything)
	})
}
