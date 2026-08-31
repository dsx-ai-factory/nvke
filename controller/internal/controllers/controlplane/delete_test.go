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

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/nodes"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

// cpControllerConfig is a config with the control-plane-controller path on.
func cpControllerConfig(on bool) config.Config {
	return config.Config{ProvisionControlPlaneWithControlPlaneController: on}
}

// requestedDeleteNode builds a control-plane node whose delete was accepted at
// requestedAt but whose teardown has not started.
func requestedDeleteNode(id, clusterID uuid.UUID, name string, requestedAt time.Time) db.Node {
	return db.Node{
		ID:           id,
		ClusterID:    clusterID,
		ControlPlane: true,
		Name:         name,
		// The row persists a purpose-built record, not the workflow input, so the
		// fixture writes that shape directly.
		DeleteRequest: []byte(`{"reason":"API"}`),
		Conditions: db.NodeConditions{
			Joined: db.NewCondition(true, "Joined"),
			DeleteRequested: db.Condition{
				Status:             db.ConditionStatusTrue,
				Message:            "Delete accepted",
				LastTransitionTime: requestedAt,
			},
		},
	}
}

// TestReconcileRequestedDeletes_StartsTeardown is the routing half of the story:
// a recorded request becomes an actual teardown, queued by the controller.
func TestReconcileRequestedDeletes_StartsTeardown(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	cluster := db.Cluster{ID: clusterID, Name: "test-cluster"}
	target := requestedDeleteNode(uuid.New(), clusterID, "cp-1", time.Now())

	mockTx := &testutil.MockTx{}
	mockTx.On("GetNodeForUpdate", mock.Anything, target.ID).Return(target, nil)

	var written db.Node
	mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(updated *db.Node) bool {
		written = *updated
		return true
	})).Return(target, nil)
	mockTx.On("CreateWorkflowOutboxEntry",
		mock.Anything,
		constants.TeardownNodeWorkflowType,
		constants.TeardownNodeWorkflowPrefix+target.ID.String(),
		mock.MatchedBy(func(input nodes.TeardownNodeInput) bool {
			return input.NodeID == target.ID && input.Reason == "API" && input.AllowCPNode
		}),
	).Return(db.WorkflowOutbox{}, nil)

	r := &reconciler{cfg: cpControllerConfig(true), db: mockTx, logger: slog.Default()}
	cp := &controlPlane{nodes: []db.Node{target}}

	started, err := r.reconcileRequestedDeletes(context.Background(), cluster, cp)
	require.NoError(t, err)
	assert.True(t, started)
	assert.True(t, nodes.IsDeleting(written))
	assert.False(t, nodes.IsDeleteRequested(written))
	mockTx.AssertExpectations(t)
}

// TestReconcileRequestedDeletes_SerializesOnInFlightTeardown is the heart of the
// serialized delete loop: a second accepted delete waits for the first teardown.
func TestReconcileRequestedDeletes_SerializesOnInFlightTeardown(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	cluster := db.Cluster{ID: clusterID, Name: "test-cluster"}

	tearingDown := db.Node{
		ID: uuid.New(), ClusterID: clusterID, ControlPlane: true, Name: "cp-1",
		Conditions: db.NodeConditions{Deleting: db.NewCondition(true, "Node deletion in progress")},
	}
	queued := requestedDeleteNode(uuid.New(), clusterID, "cp-2", time.Now())

	mockTx := &testutil.MockTx{}
	r := &reconciler{cfg: cpControllerConfig(true), db: mockTx, logger: slog.Default()}
	cp := &controlPlane{nodes: []db.Node{tearingDown, queued}}

	started, err := r.reconcileRequestedDeletes(context.Background(), cluster, cp)
	require.NoError(t, err)
	assert.False(t, started, "no second teardown may start while one is in flight")
	mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
	mockTx.AssertNotCalled(t, "CreateWorkflowOutboxEntry",
		mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// TestReconcileRequestedDeletes_NoOpCases covers the paths that must write
// nothing: the flag being off, and there being no accepted requests.
func TestReconcileRequestedDeletes_NoOpCases(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	cluster := db.Cluster{ID: clusterID, Name: "test-cluster"}
	requested := requestedDeleteNode(uuid.New(), clusterID, "cp-1", time.Now())
	healthy := db.Node{
		ID: uuid.New(), ClusterID: clusterID, ControlPlane: true, Name: "cp-2",
		Conditions: db.NodeConditions{Joined: db.NewCondition(true, "Joined")},
	}

	tests := []struct {
		name    string
		flagOn  bool
		cpNodes []db.Node
	}{
		{name: "flag off leaves the legacy direct-delete path alone", flagOn: false, cpNodes: []db.Node{requested}},
		{name: "no accepted requests", flagOn: true, cpNodes: []db.Node{healthy}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mockTx := &testutil.MockTx{}
			r := &reconciler{cfg: cpControllerConfig(tt.flagOn), db: mockTx, logger: slog.Default()}
			cp := &controlPlane{nodes: tt.cpNodes}

			started, err := r.reconcileRequestedDeletes(context.Background(), cluster, cp)
			require.NoError(t, err)
			assert.False(t, started)
			mockTx.AssertNotCalled(t, "GetNodeForUpdate", mock.Anything, mock.Anything)
			mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
		})
	}
}

// TestSelectNextControlPlaneNodeToDelete drains accepted requests oldest-first so
// a queued delete cannot starve behind newer ones.
func TestSelectNextControlPlaneNodeToDelete(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	base := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

	oldest := requestedDeleteNode(uuid.New(), clusterID, "cp-oldest", base)
	middle := requestedDeleteNode(uuid.New(), clusterID, "cp-middle", base.Add(time.Minute))
	newest := requestedDeleteNode(uuid.New(), clusterID, "cp-newest", base.Add(2*time.Minute))

	got := selectNextControlPlaneNodeToDelete([]db.Node{newest, oldest, middle})
	assert.Equal(t, oldest.ID, got.ID)

	// Same instant: the choice must still be stable rather than map-order.
	tieA := requestedDeleteNode(uuid.MustParse("00000000-0000-0000-0000-00000000000a"), clusterID, "cp-a", base)
	tieB := requestedDeleteNode(uuid.MustParse("00000000-0000-0000-0000-00000000000b"), clusterID, "cp-b", base)
	assert.Equal(t, tieA.ID, selectNextControlPlaneNodeToDelete([]db.Node{tieB, tieA}).ID)
	assert.Equal(t, tieA.ID, selectNextControlPlaneNodeToDelete([]db.Node{tieA, tieB}).ID)
}

// TestControlPlaneDeleteStateHelpers pins the mutually exclusive reading of the
// two delete conditions that the rest of the reconciler relies on.
func TestControlPlaneDeleteStateHelpers(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	requested := requestedDeleteNode(uuid.New(), clusterID, "cp-1", time.Now())
	tearingDown := db.Node{
		ID: uuid.New(), ControlPlane: true, Name: "cp-2",
		Conditions: db.NodeConditions{Deleting: db.NewCondition(true, "Node deletion in progress")},
	}
	healthy := db.Node{ID: uuid.New(), ControlPlane: true, Name: "cp-3"}

	assert.False(t, (&controlPlane{nodes: []db.Node{healthy}}).hasDeleteInFlight())
	assert.True(t, (&controlPlane{nodes: []db.Node{healthy, requested}}).hasDeleteInFlight(),
		"an accepted-but-not-started delete counts as in flight")
	assert.True(t, (&controlPlane{nodes: []db.Node{healthy, tearingDown}}).hasDeleteInFlight())

	_, ok := (&controlPlane{nodes: []db.Node{healthy, requested}}).nodeTearingDown()
	assert.False(t, ok, "a queued request is not yet a running teardown")

	got, ok := (&controlPlane{nodes: []db.Node{healthy, tearingDown}}).nodeTearingDown()
	require.True(t, ok)
	assert.Equal(t, tearingDown.ID, got.ID)

	assert.Len(t, (&controlPlane{nodes: []db.Node{healthy, requested, tearingDown}}).nodesWithDeleteRequested(), 1)
}
