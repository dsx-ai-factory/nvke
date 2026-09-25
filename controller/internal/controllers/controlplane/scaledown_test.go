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

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/nodes"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

// healthyCPNode builds a fully healthy, joined control-plane-controller node:
// the shape scaleDownEligible accepts and reconcileControlPlaneScaleDown may
// remove.
func healthyCPNode(clusterID uuid.UUID, name string, createdAt time.Time) db.Node {
	return db.Node{
		ID:           uuid.New(),
		ClusterID:    clusterID,
		ControlPlane: true,
		Name:         name,
		CreatedAt:    createdAt,
		Conditions: db.NodeConditions{
			Joined:          db.NewCondition(true, "Joined"),
			Provisioned:     db.NewCondition(true, "Provisioned"),
			KubernetesReady: db.NewCondition(true, "Ready"),
			NodeAgentReady:  db.NewCondition(true, "Ready"),
		},
	}
}

func TestScaleDownEligible(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	base := time.Now()

	eligibleA := healthyCPNode(clusterID, "cp-a", base)
	eligibleB := healthyCPNode(clusterID, "cp-b", base)

	pending := healthyCPNode(clusterID, "cp-pending", base)
	pending.Conditions.Pending = db.NewCondition(true, "Pending")

	notJoined := healthyCPNode(clusterID, "cp-not-joined", base)
	notJoined.Conditions.Joined = db.NewCondition(false, "Not joined")

	notProvisioned := healthyCPNode(clusterID, "cp-not-provisioned", base)
	notProvisioned.Conditions.Provisioned = db.NewCondition(false, "Not provisioned")

	notKubeReady := healthyCPNode(clusterID, "cp-not-kube", base)
	notKubeReady.Conditions.KubernetesReady = db.NewCondition(false, "Not ready")

	notAgentReady := healthyCPNode(clusterID, "cp-not-agent", base)
	notAgentReady.Conditions.NodeAgentReady = db.NewCondition(false, "Not ready")

	deleteRequested := healthyCPNode(clusterID, "cp-delete-requested", base)
	deleteRequested.Conditions.DeleteRequested = db.NewCondition(true, "Delete accepted")

	deleting := healthyCPNode(clusterID, "cp-deleting", base)
	deleting.Conditions.Deleting = db.NewCondition(true, "Deleting")

	recycling := healthyCPNode(clusterID, "cp-recycling", base)
	recycling.Conditions.MarkedForRecycling = db.NewCondition(true, "Marked")

	nodePoolNode := healthyCPNode(clusterID, "cp-nodepool", base)
	nodePoolNode.NodePoolID = uuid.New() // managed by a node pool, not the controller

	cp := &controlPlane{nodes: []db.Node{
		eligibleA, eligibleB,
		pending, notJoined, notProvisioned, notKubeReady, notAgentReady,
		deleteRequested, deleting, recycling, nodePoolNode,
	}}

	eligible := cp.scaleDownEligible()

	gotNames := make(map[string]bool, len(eligible))
	for _, n := range eligible {
		gotNames[n.Name] = true
	}
	assert.Equal(t, map[string]bool{"cp-a": true, "cp-b": true}, gotNames,
		"only fully healthy, joined, controller-managed nodes that are not on their way out are eligible")
}

func TestSelectControlPlaneNodeToScaleDown(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	preferredType := uuid.New()
	lessPreferredType := uuid.New()
	// Provider-agnostic input: Nico supplies the ordering, the selector sees
	// only the db.ControlPlaneScaleDownRanker interface.
	ranker := db.NicoControlPlaneProvider{InstanceTypePreference: []uuid.UUID{preferredType, lessPreferredType}}

	withType := func(node db.Node, instanceType uuid.UUID) db.Node {
		node.Nico = &db.NicoNodeProvider{InstanceTypeID: instanceType}
		return node
	}

	t.Run("least-preferred instance type is shed first", func(t *testing.T) {
		t.Parallel()

		preferred := withType(healthyCPNode(clusterID, "cp-preferred", base.Add(-time.Hour)), preferredType)
		lessPreferred := withType(healthyCPNode(clusterID, "cp-less", base), lessPreferredType)

		// Even though the preferred node is older, the less-preferred type goes first.
		got := selectControlPlaneNodeToScaleDown([]db.Node{preferred, lessPreferred}, ranker)
		assert.Equal(t, lessPreferred.ID, got.ID)
	})

	t.Run("a type off the preference list is shed before any listed type", func(t *testing.T) {
		t.Parallel()

		listed := withType(healthyCPNode(clusterID, "cp-listed", base.Add(-time.Hour)), lessPreferredType)
		unlisted := withType(healthyCPNode(clusterID, "cp-unlisted", base), uuid.New())

		got := selectControlPlaneNodeToScaleDown([]db.Node{listed, unlisted}, ranker)
		assert.Equal(t, unlisted.ID, got.ID)
	})

	t.Run("within the same preference rank, oldest first", func(t *testing.T) {
		t.Parallel()

		oldest := withType(healthyCPNode(clusterID, "cp-oldest", base.Add(-2*time.Hour)), preferredType)
		newer := withType(healthyCPNode(clusterID, "cp-newer", base), preferredType)

		got := selectControlPlaneNodeToScaleDown([]db.Node{newer, oldest}, ranker)
		assert.Equal(t, oldest.ID, got.ID)
	})

	t.Run("nil ranker: falls back to oldest, then stable ID", func(t *testing.T) {
		t.Parallel()

		tieA := healthyCPNode(clusterID, "cp-a", base)
		tieA.ID = uuid.MustParse("00000000-0000-0000-0000-00000000000a")
		tieB := healthyCPNode(clusterID, "cp-b", base)
		tieB.ID = uuid.MustParse("00000000-0000-0000-0000-00000000000b")

		assert.Equal(t, tieA.ID, selectControlPlaneNodeToScaleDown([]db.Node{tieB, tieA}, nil).ID)
		assert.Equal(t, tieA.ID, selectControlPlaneNodeToScaleDown([]db.Node{tieA, tieB}, nil).ID)
	})
}

func TestControlPlaneHasNodeBelowPrimaryRank(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	primaryType := uuid.New()
	lessPreferredType := uuid.New()
	ranker := db.NicoControlPlaneProvider{InstanceTypePreference: []uuid.UUID{primaryType, lessPreferredType}}

	withType := func(name string, instanceType uuid.UUID) db.Node {
		node := healthyCPNode(clusterID, name, base)
		node.Nico = &db.NicoNodeProvider{InstanceTypeID: instanceType}
		return node
	}

	onPrimary := withType("cp-primary", primaryType)
	onLess := withType("cp-less", lessPreferredType)
	offList := withType("cp-off", uuid.New())

	tests := []struct {
		name     string
		eligible []db.Node
		ranker   db.ControlPlaneScaleDownRanker
		want     bool
	}{
		{name: "all on primary type: balanced", eligible: []db.Node{onPrimary, onPrimary}, ranker: ranker, want: false},
		{name: "a less-preferred node exists", eligible: []db.Node{onPrimary, onLess}, ranker: ranker, want: true},
		{name: "an off-list node exists", eligible: []db.Node{onPrimary, offList}, ranker: ranker, want: true},
		{name: "nil ranker never reports a candidate", eligible: []db.Node{onLess, offList}, ranker: nil, want: false},
		{name: "no eligible nodes", eligible: nil, ranker: ranker, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, controlPlaneHasNodeBelowPrimaryRank(tt.eligible, tt.ranker))
		})
	}
}

func TestUpgradeInProgress(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()

	drifted := healthyCPNode(clusterID, "cp-drifted", time.Now())
	drifted.ControlPlaneConfigurationID = uuid.New()

	// A control plane with one node whose pinned snapshot drifts from desired.
	driftingCP := func() *controlPlane {
		return &controlPlane{
			nodes:                     []db.Node{drifted},
			clusterControlPlaneConfig: db.ControlPlaneConfig{ControlPlaneConfigSpec: db.ControlPlaneConfigSpec{KubernetesVersion: "v1.34.2"}},
			nodePinnedControlPlaneConfigs: map[uuid.UUID]db.ControlPlaneConfigSpec{
				drifted.ControlPlaneConfigurationID: {KubernetesVersion: "v1.33.6"},
			},
		}
	}
	steadyCP := &controlPlane{}

	tests := []struct {
		name    string
		cp      *controlPlane
		cluster db.Cluster
		want    bool
	}{
		{
			name:    "recycling marked in flight blocks",
			cp:      steadyCP,
			cluster: db.Cluster{Conditions: db.ClusterConditions{CPNodesRecycling: db.NewCondition(true, "Recycling")}},
			want:    true,
		},
		{
			name:    "recycling enabled with drift left to roll blocks",
			cp:      driftingCP(),
			cluster: db.Cluster{EnableCPNodeRecycling: true},
			want:    true,
		},
		{
			name:    "drift with recycling disabled does not block",
			cp:      driftingCP(),
			cluster: db.Cluster{EnableCPNodeRecycling: false},
			want:    false,
		},
		{
			name:    "no drift, not recycling does not block",
			cp:      steadyCP,
			cluster: db.Cluster{EnableCPNodeRecycling: true},
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.cp.upgradeInProgress(tt.cluster))
		})
	}
}

// TestReconcileControlPlaneScaleDown_NoOps covers the paths that must record no
// intent and write nothing: an upgrade in flight, and no surplus.
func TestReconcileControlPlaneScaleDown_NoOps(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	base := time.Now()
	threeHealthy := []db.Node{
		healthyCPNode(clusterID, "cp-a", base),
		healthyCPNode(clusterID, "cp-b", base),
		healthyCPNode(clusterID, "cp-c", base),
	}

	tests := []struct {
		name    string
		cluster db.Cluster
		cpNodes []db.Node
	}{
		{
			name:    "recycling in flight defers scale-down",
			cluster: db.Cluster{ID: clusterID, ControlPlaneConfig: db.ControlPlaneConfig{DesiredCount: 1}, Conditions: db.ClusterConditions{CPNodesRecycling: db.NewCondition(true, "Recycling")}},
			cpNodes: threeHealthy,
		},
		{
			name:    "at desired: no surplus, nothing to do",
			cluster: db.Cluster{ID: clusterID, ControlPlaneConfig: db.ControlPlaneConfig{DesiredCount: 3}},
			cpNodes: threeHealthy,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mockTx := &testutil.MockTx{}
			r := &reconciler{db: mockTx, logger: slog.Default()}
			cp := &controlPlane{nodes: tt.cpNodes, clusterControlPlaneConfig: tt.cluster.ControlPlaneConfig}

			err := r.reconcileControlPlaneScaleDown(context.Background(), tt.cluster, cp)
			require.NoError(t, err)

			mockTx.AssertNotCalled(t, "GetNodeForUpdate", mock.Anything, mock.Anything)
			mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
			mockTx.AssertNotCalled(t, "UpdateCluster", mock.Anything, mock.Anything)
		})
	}
}

// TestReconcileControlPlaneScaleDown_HeldByQuorumFloor: desired below the floor,
// so the shrink stops at the floor -- no victim selected, no delete recorded.
func TestReconcileControlPlaneScaleDown_HeldByQuorumFloor(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	base := time.Now()
	cluster := db.Cluster{ID: clusterID, Name: "test-cluster", ControlPlaneConfig: db.ControlPlaneConfig{DesiredCount: 1}}
	cpNodes := []db.Node{
		healthyCPNode(clusterID, "cp-a", base),
		healthyCPNode(clusterID, "cp-b", base),
		healthyCPNode(clusterID, "cp-c", base),
	}

	mockTx := &testutil.MockTx{}
	r := &reconciler{db: mockTx, logger: slog.Default()}
	cp := &controlPlane{nodes: cpNodes, clusterControlPlaneConfig: cluster.ControlPlaneConfig}

	err := r.reconcileControlPlaneScaleDown(context.Background(), cluster, cp)
	require.NoError(t, err)

	mockTx.AssertNotCalled(t, "GetNodeForUpdate", mock.Anything, mock.Anything)
	mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
	mockTx.AssertNotCalled(t, "CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// TestReconcileControlPlaneScaleDown_RecordsIntent: a real surplus above the
// floor records a queued delete on the oldest node and starts no teardown inline.
func TestReconcileControlPlaneScaleDown_RecordsIntent(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	base := time.Now()
	cluster := db.Cluster{ID: clusterID, Name: "test-cluster", ControlPlaneConfig: db.ControlPlaneConfig{DesiredCount: 3}}

	victim := healthyCPNode(clusterID, "cp-oldest", base.Add(-time.Hour))
	others := []db.Node{
		healthyCPNode(clusterID, "cp-b", base),
		healthyCPNode(clusterID, "cp-c", base),
		healthyCPNode(clusterID, "cp-d", base),
	}
	all := append([]db.Node{victim}, others...)

	mockTx := &testutil.MockTx{}
	mockTx.On("GetNodeForUpdate", mock.Anything, victim.ID).Return(victim, nil)
	// The record-time quorum floor lists CP pools and nodes to count survivors.
	mockTx.On("ListNodePools", mock.Anything, mock.Anything).Return([]db.NodePool{}, nil)
	mockTx.On("ListNodes", mock.Anything, mock.Anything).Return(all, nil)

	var written db.Node
	mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(n *db.Node) bool {
		written = *n
		return true
	})).Return(victim, nil)

	r := &reconciler{db: mockTx, logger: slog.Default()}
	cp := &controlPlane{nodes: all, clusterControlPlaneConfig: cluster.ControlPlaneConfig}

	err := r.reconcileControlPlaneScaleDown(context.Background(), cluster, cp)
	require.NoError(t, err)

	assert.Equal(t, victim.ID, written.ID, "the oldest eligible node is shed first")
	assert.True(t, nodes.IsDeleteRequested(written), "scale-down records intent")
	assert.False(t, nodes.IsDeleting(written), "the teardown waits for the serialized delete loop")
	mockTx.AssertNotCalled(t, "CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}
