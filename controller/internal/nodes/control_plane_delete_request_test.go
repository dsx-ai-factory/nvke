// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodes_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/internal/constants"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/nodes"
	"github.com/dsx-ai-factory/nke/controller/internal/testutil"
)

var testClusterID = uuid.MustParse("650e8400-e29b-41d4-a716-446655440000")

// joinedCPNode builds a joined control-plane node in the shared test cluster.
func joinedCPNode(id uuid.UUID) db.Node {
	return db.Node{
		ID:           id,
		ClusterID:    testClusterID,
		ControlPlane: true,
		Name:         "cp-" + id.String()[:8],
		Conditions: db.NodeConditions{
			Joined:          db.NewCondition(true, "Joined"),
			KubernetesReady: db.NewCondition(true, "Ready"),
			NodeAgentReady:  db.NewCondition(true, "Ready"),
		},
	}
}

// unreachableCPNode is a joined control-plane node that has stopped reporting
// healthy: kubelet and agent are gone, so its etcd member is not answering.
func unreachableCPNode(id uuid.UUID) db.Node {
	node := joinedCPNode(id)
	node.Conditions.KubernetesReady = db.NewCondition(false, "Node is not ready")
	node.Conditions.NodeAgentReady = db.NewCondition(false, "Agent is not responding")
	return node
}

// cpNodeIDs returns n distinct control-plane node IDs, deterministic per index
// so a failing case names the same node on every run.
func cpNodeIDs(n int) []uuid.UUID {
	ids := make([]uuid.UUID, n)
	for i := range ids {
		ids[i] = uuid.MustParse(fmt.Sprintf("650e8400-e29b-41d4-a716-44665544%04d", i))
	}
	return ids
}

// expectControlPlaneCensus wires the two lookups the quorum floor makes.
func expectControlPlaneCensus(mockTx *testutil.MockTx, cpNodes []db.Node) {
	mockTx.On("ListNodePools", mock.Anything, mock.Anything).Return([]db.NodePool{}, nil).Maybe()
	mockTx.On("ListNodes", mock.Anything, mock.Anything).Return(cpNodes, nil).Maybe()
}

// TestControlPlaneQuorumFloor pins the refuse/allow boundary of the
// controller-level guard: a delete is accepted only when at least three joined
// control-plane nodes survive it.
func TestControlPlaneQuorumFloor(t *testing.T) {
	t.Parallel()

	ids := cpNodeIDs(5)

	tests := []struct {
		name       string
		cpNodes    func() []db.Node
		target     int
		wantRefuse bool
		wantSurviv int
	}{
		{
			name: "four joined control-plane nodes: delete leaves three, accepted",
			cpNodes: func() []db.Node {
				return []db.Node{joinedCPNode(ids[0]), joinedCPNode(ids[1]), joinedCPNode(ids[2]), joinedCPNode(ids[3])}
			},
			target: 0,
		},
		{
			name: "three joined control-plane nodes: delete leaves two, refused",
			cpNodes: func() []db.Node {
				return []db.Node{joinedCPNode(ids[0]), joinedCPNode(ids[1]), joinedCPNode(ids[2])}
			},
			target:     0,
			wantRefuse: true,
			wantSurviv: 2,
		},
		{
			name: "three joined control-plane nodes, target unreachable: accepted",
			cpNodes: func() []db.Node {
				return []db.Node{unreachableCPNode(ids[0]), joinedCPNode(ids[1]), joinedCPNode(ids[2])}
			},
			target: 0,
		},
		{
			name: "two joined control-plane nodes, target unreachable: accepted",
			cpNodes: func() []db.Node {
				return []db.Node{unreachableCPNode(ids[0]), joinedCPNode(ids[1])}
			},
			target: 0,
		},
		{
			name: "two joined control-plane nodes, target healthy: refused",
			cpNodes: func() []db.Node {
				return []db.Node{joinedCPNode(ids[0]), joinedCPNode(ids[1])}
			},
			target:     0,
			wantRefuse: true,
			wantSurviv: 1,
		},
		{
			name: "an unreachable survivor does not exempt a healthy target",
			cpNodes: func() []db.Node {
				return []db.Node{joinedCPNode(ids[0]), joinedCPNode(ids[1]), unreachableCPNode(ids[2])}
			},
			target:     0,
			wantRefuse: true,
			wantSurviv: 2,
		},
		{
			name: "five joined control-plane nodes: delete leaves four, accepted",
			cpNodes: func() []db.Node {
				return []db.Node{
					joinedCPNode(ids[0]), joinedCPNode(ids[1]), joinedCPNode(ids[2]),
					joinedCPNode(ids[3]), joinedCPNode(ids[4]),
				}
			},
			target: 0,
		},
		{
			name: "a node already tearing down does not count as a survivor",
			cpNodes: func() []db.Node {
				tearing := joinedCPNode(ids[3])
				tearing.Conditions.Deleting = db.NewCondition(true, "Node deletion in progress")
				return []db.Node{joinedCPNode(ids[0]), joinedCPNode(ids[1]), joinedCPNode(ids[2]), tearing}
			},
			target:     0,
			wantRefuse: true,
			wantSurviv: 2,
		},
		{
			name: "a node with a delete already requested does not count as a survivor",
			cpNodes: func() []db.Node {
				queued := joinedCPNode(ids[3])
				queued.Conditions.DeleteRequested = db.NewCondition(true, "Delete accepted")
				return []db.Node{joinedCPNode(ids[0]), joinedCPNode(ids[1]), joinedCPNode(ids[2]), queued}
			},
			target:     0,
			wantRefuse: true,
			wantSurviv: 2,
		},
		{
			name: "an unjoined node does not count as a survivor",
			cpNodes: func() []db.Node {
				pending := joinedCPNode(ids[3])
				pending.Conditions.Joined = db.NewCondition(false, "Not yet joined")
				return []db.Node{joinedCPNode(ids[0]), joinedCPNode(ids[1]), joinedCPNode(ids[2]), pending}
			},
			target:     0,
			wantRefuse: true,
			wantSurviv: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			cpNodes := tt.cpNodes()
			target := cpNodes[tt.target]

			mockTx := &testutil.MockTx{}
			mockTx.On("GetNodeForUpdate", mock.Anything, target.ID).Return(target, nil)
			expectControlPlaneCensus(mockTx, cpNodes)
			if !tt.wantRefuse {
				mockTx.On("UpdateNode", mock.Anything, mock.Anything).Return(target, nil)
			}

			err := nodes.DeleteWithTx(ctx, mockTx, target.ID,
				nodes.WithSkipLiveDeleteGuard(),
				nodes.WithControlPlaneControllerDelete(true),
			)

			if !tt.wantRefuse {
				require.NoError(t, err)
				mockTx.AssertExpectations(t)
				return
			}

			var refusal *nodes.ControlPlaneQuorumFloorError
			require.ErrorAs(t, err, &refusal)
			require.Equal(t, tt.wantSurviv, refusal.Surviving)
			require.Equal(t, nodes.MinFaultTolerantControlPlaneNodes, refusal.Minimum)
			require.Equal(t, target.Name, refusal.NodeName)
			// The refusal must not have written anything.
			mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
			mockTx.AssertNotCalled(t, "CreateWorkflowOutboxEntry",
				mock.Anything, mock.Anything, mock.Anything, mock.Anything)
		})
	}
}

// TestControlPlaneDeleteRecordsIntentWithoutStartingTeardown covers the routing
// half of the story: an accepted control-plane delete leaves the node marked
// DeleteRequested with its teardown input recorded, and queues no workflow.
func TestControlPlaneDeleteRecordsIntentWithoutStartingTeardown(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	ids := cpNodeIDs(4)
	cpNodes := []db.Node{joinedCPNode(ids[0]), joinedCPNode(ids[1]), joinedCPNode(ids[2]), joinedCPNode(ids[3])}
	target := cpNodes[0]

	mockTx := &testutil.MockTx{}
	mockTx.On("GetNodeForUpdate", mock.Anything, target.ID).Return(target, nil)
	expectControlPlaneCensus(mockTx, cpNodes)

	var written db.Node
	mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(updated *db.Node) bool {
		written = *updated
		return updated.ID == target.ID
	})).Return(target, nil)

	require.NoError(t, nodes.DeleteWithTx(ctx, mockTx, target.ID,
		nodes.WithSkipLiveDeleteGuard(),
		nodes.WithControlPlaneControllerDelete(true),
		nodes.WithReason("API"),
		nodes.WithAllowCPNode(true),
		nodes.WithForceUnreachable(true),
		nodes.WithMachineHealthIssue(newMachineHealthIssue("Hardware", "Disk failure detected")),
	))

	require.True(t, nodes.IsDeleteRequested(written), "the node should be marked DeleteRequested")
	require.False(t, nodes.IsDeleting(written), "the teardown must not have started")

	// Assert on the persisted JSON, which is the contract the row actually keeps.
	// The node it targets is the row itself, so the record does not repeat it.
	var recorded struct {
		Reason             string `json:"reason"`
		ForceUnreachable   bool   `json:"forceUnreachable"`
		MachineHealthIssue *struct {
			Summary string `json:"summary"`
		} `json:"machineHealthIssue"`
	}
	require.NoError(t, json.Unmarshal(written.DeleteRequest, &recorded))
	require.Equal(t, "API", recorded.Reason)
	require.True(t, recorded.ForceUnreachable, "forceUnreachable must survive the hand-off")
	require.NotNil(t, recorded.MachineHealthIssue)
	require.Equal(t, "Disk failure detected", recorded.MachineHealthIssue.Summary)

	mockTx.AssertNotCalled(t, "CreateWorkflowOutboxEntry",
		mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	mockTx.AssertExpectations(t)
}

// TestUnhealthyControlPlaneDeleteRecordsForceUnreachable pairs the floor
// exemption with the teardown it hands off to: the node the floor waived as
// unhealthy must be torn down as unreachable, or the accepted delete stalls on a
// drain that can never finish.
func TestUnhealthyControlPlaneDeleteRecordsForceUnreachable(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	ids := cpNodeIDs(3)
	cpNodes := []db.Node{unreachableCPNode(ids[0]), joinedCPNode(ids[1]), joinedCPNode(ids[2])}
	target := cpNodes[0]

	mockTx := &testutil.MockTx{}
	mockTx.On("GetNodeForUpdate", mock.Anything, target.ID).Return(target, nil)
	expectControlPlaneCensus(mockTx, cpNodes)

	var written db.Node
	mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(updated *db.Node) bool {
		written = *updated
		return updated.ID == target.ID
	})).Return(target, nil)

	require.NoError(t, nodes.DeleteWithTx(ctx, mockTx, target.ID,
		nodes.WithSkipLiveDeleteGuard(),
		nodes.WithControlPlaneControllerDelete(true),
		nodes.WithReason("API"),
		nodes.WithAllowCPNode(true),
	))

	var recorded struct {
		ForceUnreachable bool `json:"forceUnreachable"`
	}
	require.NoError(t, json.Unmarshal(written.DeleteRequest, &recorded))
	require.True(t, recorded.ForceUnreachable,
		"a delete the floor waived as unhealthy must tear the node down as unreachable")
	mockTx.AssertExpectations(t)
}

// TestWorkerDeleteIgnoresControlPlaneControllerFlag guards the "worker deletes
// are unchanged" acceptance criterion.
func TestWorkerDeleteIgnoresControlPlaneControllerFlag(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	workerID := uuid.MustParse("750e8400-e29b-41d4-a716-446655440001")
	worker := db.Node{ID: workerID, ClusterID: testClusterID, Name: "worker-1"}

	mockTx := &testutil.MockTx{}
	mockTx.On("GetNodeForUpdate", mock.Anything, workerID).Return(worker, nil)
	mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(updated *db.Node) bool {
		return nodes.IsDeleting(*updated) && !nodes.IsDeleteRequested(*updated)
	})).Return(worker, nil)
	mockTx.On("CreateWorkflowOutboxEntry",
		mock.Anything,
		constants.TeardownNodeWorkflowType,
		constants.TeardownNodeWorkflowPrefix+workerID.String(),
		mock.Anything,
	).Return(db.WorkflowOutbox{}, nil)

	require.NoError(t, nodes.DeleteWithTx(ctx, mockTx, workerID,
		nodes.WithSkipLiveDeleteGuard(),
		nodes.WithControlPlaneControllerDelete(true),
	))
	mockTx.AssertExpectations(t)
}

// TestControlPlaneDeleteWithFlagOffStaysDirect guards the legacy path: with the
// flag off a control-plane delete still starts its teardown inline.
func TestControlPlaneDeleteWithFlagOffStaysDirect(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	target := joinedCPNode(cpNodeIDs(1)[0])

	mockTx := &testutil.MockTx{}
	mockTx.On("GetNodeForUpdate", mock.Anything, target.ID).Return(target, nil)
	mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(updated *db.Node) bool {
		return nodes.IsDeleting(*updated) && !nodes.IsDeleteRequested(*updated)
	})).Return(target, nil)
	mockTx.On("CreateWorkflowOutboxEntry",
		mock.Anything,
		constants.TeardownNodeWorkflowType,
		constants.TeardownNodeWorkflowPrefix+target.ID.String(),
		mock.Anything,
	).Return(db.WorkflowOutbox{}, nil)

	require.NoError(t, nodes.DeleteWithTx(ctx, mockTx, target.ID,
		nodes.WithSkipLiveDeleteGuard(),
		nodes.WithControlPlaneControllerDelete(false),
	))
	mockTx.AssertExpectations(t)
}

// TestUnjoinedControlPlaneDeleteSkipsTheQuorumFloor: a node that never joined is
// not an etcd member, so clearing a stuck pending node must stay possible even
// while the control plane is still below three.
func TestUnjoinedControlPlaneDeleteSkipsTheQuorumFloor(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	ids := cpNodeIDs(2)

	joined := joinedCPNode(ids[0])
	pending := joinedCPNode(ids[1])
	pending.Conditions.Joined = db.NewCondition(false, "Not yet joined")
	pending.Conditions.Pending = db.NewCondition(true, "Pending control-plane node provisioning")

	mockTx := &testutil.MockTx{}
	mockTx.On("GetNodeForUpdate", mock.Anything, pending.ID).Return(pending, nil)
	expectControlPlaneCensus(mockTx, []db.Node{joined, pending})

	var written db.Node
	mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(updated *db.Node) bool {
		written = *updated
		return true
	})).Return(pending, nil)

	require.NoError(t, nodes.DeleteWithTx(ctx, mockTx, pending.ID,
		nodes.WithSkipLiveDeleteGuard(),
		nodes.WithControlPlaneControllerDelete(true),
	))
	require.True(t, nodes.IsDeleteRequested(written),
		"deleting a never-joined control-plane node must not hit the quorum floor")
}

// TestFlagOffAbsorbsAPendingDeleteRequest: if the control-plane-controller path
// is turned off while a request is recorded, nothing would ever promote it, so
// the direct delete must take it over rather than refuse it forever.
func TestFlagOffAbsorbsAPendingDeleteRequest(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	target := joinedCPNode(cpNodeIDs(1)[0])
	target.Conditions.DeleteRequested = db.NewCondition(true, "Delete accepted")
	target.DeleteRequest = []byte(`{"NodeID":"` + target.ID.String() + `"}`)

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
		mock.Anything,
	).Return(db.WorkflowOutbox{}, nil)

	require.NoError(t, nodes.DeleteWithTx(ctx, mockTx, target.ID,
		nodes.WithSkipLiveDeleteGuard(),
		nodes.WithControlPlaneControllerDelete(false),
	))
	require.True(t, nodes.IsDeleting(written))
	require.False(t, nodes.IsDeleteRequested(written), "the stale request should be cleared, not left set")
	require.Nil(t, written.DeleteRequest)
	mockTx.AssertExpectations(t)
}

// TestControlPlaneDeleteIsIdempotent covers a repeated DELETE: the second call
// reports the request is already recorded rather than recording it twice.
func TestControlPlaneDeleteIsIdempotent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	target := joinedCPNode(cpNodeIDs(1)[0])
	target.Conditions.DeleteRequested = db.NewCondition(true, "Delete accepted")

	mockTx := &testutil.MockTx{}
	mockTx.On("GetNodeForUpdate", mock.Anything, target.ID).Return(target, nil)

	err := nodes.DeleteWithTx(ctx, mockTx, target.ID,
		nodes.WithSkipLiveDeleteGuard(),
		nodes.WithControlPlaneControllerDelete(true),
	)
	require.ErrorIs(t, err, nodes.ErrDeleteAlreadyRequested)
	mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
}

// TestStartRequestedTeardown covers the promotion step: the recorded request is
// rebuilt into the workflow input and the node flips to Deleting.
func TestStartRequestedTeardown(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	target := joinedCPNode(cpNodeIDs(1)[0])
	target.Conditions.DeleteRequested = db.NewCondition(true, "Delete accepted")
	// Written as raw JSON on purpose: the record's shape is what the node row
	// persists, so the test pins that rather than round-tripping the type.
	target.DeleteRequest = []byte(`{"reason":"API","forceUnreachable":true}`)

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
			return input.NodeID == target.ID &&
				input.Reason == "API" &&
				input.ForceUnreachable &&
				input.AllowCPNode
		}),
	).Return(db.WorkflowOutbox{}, nil)

	require.NoError(t, nodes.StartRequestedTeardown(ctx, mockTx, target.ID))

	require.True(t, nodes.IsDeleting(written), "the node should now be Deleting")
	require.False(t, nodes.IsDeleteRequested(written), "the request should be cleared once started")
	require.Nil(t, written.DeleteRequest, "the recorded payload should be cleared once replayed")
	mockTx.AssertExpectations(t)
}

// TestStartRequestedTeardownWithoutRequest rejects promoting a node that carries
// no recorded request, so a stale enqueue cannot delete a healthy node.
func TestStartRequestedTeardownWithoutRequest(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	target := joinedCPNode(cpNodeIDs(1)[0])

	mockTx := &testutil.MockTx{}
	mockTx.On("GetNodeForUpdate", mock.Anything, target.ID).Return(target, nil)

	require.ErrorIs(t, nodes.StartRequestedTeardown(ctx, mockTx, target.ID), nodes.ErrNoDeleteRequested)
	mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
}
