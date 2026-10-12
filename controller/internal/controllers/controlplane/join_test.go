// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestControlPlaneJoinCandidates(t *testing.T) {
	t.Parallel()

	older := time.Now().Add(-2 * time.Minute)
	newer := time.Now().Add(-1 * time.Minute)

	readyOlder := readyNode("cp-old", older)
	readyNewer := readyNode("cp-new", newer)
	alreadyProvisioned := db.Node{
		ID:        uuid.New(),
		Name:      "cp-done",
		CreatedAt: older.Add(-time.Minute),
		Conditions: db.NodeConditions{
			ReadyToJoin: db.NewCondition(true, "ready"),
			Provisioned: db.NewCondition(true, "done"),
		},
	}
	deleting := db.Node{
		ID:        uuid.New(),
		Name:      "cp-deleting",
		CreatedAt: older.Add(-2 * time.Minute),
		Conditions: db.NodeConditions{
			ReadyToJoin: db.NewCondition(true, "ready"),
			Provisioned: db.NewCondition(false, "not yet"),
			Deleting:    db.NewCondition(true, "deleting"),
		},
	}
	notReady := db.Node{
		ID:        uuid.New(),
		Name:      "cp-booting",
		CreatedAt: older.Add(-3 * time.Minute),
		Conditions: db.NodeConditions{
			ReadyToJoin: db.NewCondition(false, "not ready"),
			Provisioned: db.NewCondition(false, "not yet"),
		},
	}

	got := controlPlaneJoinCandidates([]db.Node{readyNewer, alreadyProvisioned, deleting, notReady, readyOlder})
	// Provisioned, deleting, and not-ready nodes are filtered out.
	assert.ElementsMatch(t, []uuid.UUID{readyOlder.ID, readyNewer.ID}, nodeIDs(got))

	assert.Empty(t, controlPlaneJoinCandidates([]db.Node{alreadyProvisioned, notReady}))
}

func TestSelectControlPlaneJoin(t *testing.T) {
	t.Parallel()

	base := time.Now().Add(-10 * time.Minute)
	// Node creation times (older node = smaller offset).
	nodeA := readyNode("cp-a", base)
	nodeB := readyNode("cp-b", base.Add(1*time.Minute))
	nodeC := readyNode("cp-c", base.Add(2*time.Minute))

	tests := []struct {
		name       string
		candidates []joinCandidate
		wantNodeID uuid.UUID
		wantOK     bool
	}{
		{
			name:       "no candidates",
			candidates: nil,
			wantOK:     false,
		},
		{
			name:       "single fresh node starts",
			candidates: []joinCandidate{{node: nodeA, status: joinStatusNotStarted}},
			wantNodeID: nodeA.ID,
			wantOK:     true,
		},
		{
			name: "fresh: starts the oldest node",
			candidates: []joinCandidate{
				{node: nodeB, status: joinStatusNotStarted},
				{node: nodeA, status: joinStatusNotStarted},
			},
			wantNodeID: nodeA.ID,
			wantOK:     true,
		},
		{
			name: "waits while any node's join is in flight",
			candidates: []joinCandidate{
				{node: nodeA, status: joinStatusNotStarted},
				{node: nodeB, status: joinStatusJoining},
			},
			wantOK: false,
		},
		{
			// Post-join work does not hold the serial slot: a node whose kubeadm
			// join already finished (Joined) must not block the next node's join.
			name: "starts next join while another node is in post-join",
			candidates: []joinCandidate{
				{node: nodeA, status: joinStatusPostJoin},
				{node: nodeB, status: joinStatusNotStarted},
			},
			wantNodeID: nodeB.ID,
			wantOK:     true,
		},
		{
			// Settling (completed, awaiting Provisioned) also does not block.
			name: "starts next join while another node is settling",
			candidates: []joinCandidate{
				{node: nodeA, status: joinStatusSettling},
				{node: nodeB, status: joinStatusNotStarted},
			},
			wantNodeID: nodeB.ID,
			wantOK:     true,
		},
		{
			name: "post-join and settling nodes alone start nothing new",
			candidates: []joinCandidate{
				{node: nodeA, status: joinStatusPostJoin},
				{node: nodeB, status: joinStatusSettling},
			},
			wantOK: false,
		},
		{
			// The rotation: an exhausted node is deprioritized behind a fresh one,
			// even though the exhausted node is older.
			name: "prefers a fresh node over an older exhausted one",
			candidates: []joinCandidate{
				{node: nodeA, status: joinStatusExhausted, closeTime: base.Add(5 * time.Minute)},
				{node: nodeB, status: joinStatusNotStarted},
			},
			wantNodeID: nodeB.ID,
			wantOK:     true,
		},
		{
			// Once nothing fresh remains, retry exhausted nodes oldest-failure-first.
			name: "retries the exhausted node whose failure is oldest",
			candidates: []joinCandidate{
				{node: nodeB, status: joinStatusExhausted, closeTime: base.Add(9 * time.Minute)},
				{node: nodeC, status: joinStatusExhausted, closeTime: base.Add(6 * time.Minute)},
			},
			wantNodeID: nodeC.ID,
			wantOK:     true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			node, ok := selectControlPlaneJoin(tc.candidates)
			assert.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				assert.Equal(t, tc.wantNodeID, node.ID)
			}
		})
	}
}

func TestFirstJoiningNode(t *testing.T) {
	t.Parallel()

	a := readyNode("a", time.Now())
	b := readyNode("b", time.Now())
	c := readyNode("c", time.Now())

	blocker, ok := firstJoiningNode([]joinCandidate{
		{node: a, status: joinStatusPostJoin},
		{node: b, status: joinStatusJoining},
		{node: c, status: joinStatusSettling},
	})
	assert.True(t, ok)
	assert.Equal(t, b.ID, blocker.node.ID)
	assert.Equal(t, joinStatusJoining, blocker.status)

	// Post-join and settling nodes are not "joining" — nothing holds the slot.
	_, ok = firstJoiningNode([]joinCandidate{
		{node: a, status: joinStatusPostJoin},
		{node: b, status: joinStatusSettling},
	})
	assert.False(t, ok)
}

func readyNode(name string, createdAt time.Time) db.Node {
	return db.Node{
		ID:        uuid.New(),
		Name:      name,
		CreatedAt: createdAt,
		Conditions: db.NodeConditions{
			ReadyToJoin: db.NewCondition(true, "ready"),
			Provisioned: db.NewCondition(false, "not yet"),
		},
	}
}

func nodeIDs(nodes []db.Node) []uuid.UUID {
	ids := make([]uuid.UUID, len(nodes))
	for i, n := range nodes {
		ids[i] = n.ID
	}
	return ids
}

// TestControlPlaneJoinDeleteGate covers what reconcileControlPlaneJoins holds
// joins on. A join and an etcd member removal both mutate etcd membership, so a
// teardown actually in flight blocks a join. A delete that is only queued does
// not: it has not touched etcd, and the reconcile order deliberately finishes
// growing before it starts shrinking.
func TestControlPlaneJoinDeleteGate(t *testing.T) {
	t.Parallel()

	healthy := db.Node{
		ID:   uuid.MustParse("00000000-0000-0000-0000-0000000000a1"),
		Name: "cp-healthy",
		Conditions: db.NodeConditions{
			Joined:          db.NewCondition(true, "Joined"),
			KubernetesReady: db.NewCondition(true, "Ready"),
		},
	}
	tearingDown := db.Node{
		ID:         uuid.MustParse("00000000-0000-0000-0000-0000000000b2"),
		Name:       "cp-tearing-down",
		Conditions: db.NodeConditions{Deleting: db.NewCondition(true, "Node deletion in progress")},
	}
	deleteRequested := db.Node{
		ID:         uuid.MustParse("00000000-0000-0000-0000-0000000000c3"),
		Name:       "cp-delete-requested",
		Conditions: db.NodeConditions{DeleteRequested: db.NewCondition(true, "Delete accepted")},
	}

	tests := []struct {
		name        string
		nodes       []db.Node
		wantInFlght bool
		wantName    string
	}{
		{
			name:  "no delete in flight lets joins proceed",
			nodes: []db.Node{healthy},
		},
		{
			name:        "a running teardown holds joins",
			nodes:       []db.Node{healthy, tearingDown},
			wantInFlght: true,
			wantName:    "cp-tearing-down",
		},
		{
			// Grow before shrink: a queued delete waits for the join, not the
			// other way round, so it must not register here.
			name:  "an accepted but not yet started delete does not hold joins",
			nodes: []db.Node{healthy, deleteRequested},
		},
		{
			name:  "no control-plane nodes at all",
			nodes: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			node, inFlight := (&controlPlane{nodes: tt.nodes}).nodeTearingDown()
			assert.Equal(t, tt.wantInFlght, inFlight)
			if tt.wantInFlght {
				assert.Equal(t, tt.wantName, node.Name, "the blocked-on node should be named for the log line")
			}
		})
	}
}
