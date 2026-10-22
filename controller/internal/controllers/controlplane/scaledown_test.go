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
	// DeleteWithTx reads the node unlocked and takes the CP mutation lock before
	// the FOR UPDATE read; both must succeed for the record path to proceed.
	mockTx.On("GetNode", mock.Anything, victim.ID).Return(victim, nil)
	mockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(cluster, nil)
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

// TestReconcileControlPlaneScaleDown_RefusedByFloorAtRecordTime covers the race
// the in-memory pre-check cannot: the snapshot shows enough eligible nodes to
// shed one, but the live count the record-time quorum floor reads is tighter, so
// DeleteWithTx returns a ControlPlaneQuorumFloorError. The reconciler must treat
// that as the guard working -- log and move on -- not as a reconcile failure.
func TestReconcileControlPlaneScaleDown_RefusedByFloorAtRecordTime(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	base := time.Now()
	cluster := db.Cluster{ID: clusterID, Name: "test-cluster", ControlPlaneConfig: db.ControlPlaneConfig{DesiredCount: 3}}

	// Snapshot: four healthy nodes, so the pre-check (4-1 >= 3) lets a victim through.
	victim := healthyCPNode(clusterID, "cp-oldest", base.Add(-time.Hour))
	b := healthyCPNode(clusterID, "cp-b", base)
	c := healthyCPNode(clusterID, "cp-d", base)
	d := healthyCPNode(clusterID, "cp-e", base)
	snapshot := []db.Node{victim, b, c, d}

	mockTx := &testutil.MockTx{}
	// DeleteWithTx reads the node unlocked and takes the CP mutation lock before
	// the FOR UPDATE read; both must succeed for the record path to reach the floor.
	mockTx.On("GetNode", mock.Anything, victim.ID).Return(victim, nil)
	mockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(cluster, nil)
	mockTx.On("GetNodeForUpdate", mock.Anything, victim.ID).Return(victim, nil)
	mockTx.On("ListNodePools", mock.Anything, mock.Anything).Return([]db.NodePool{}, nil)
	// Live reality the floor reads is tighter: only victim + two survivors, so
	// removing the victim would leave 2 < MinFaultTolerantControlPlaneNodes.
	mockTx.On("ListNodes", mock.Anything, mock.Anything).Return([]db.Node{victim, b, c}, nil)

	r := &reconciler{db: mockTx, logger: slog.Default()}
	cp := &controlPlane{nodes: snapshot, clusterControlPlaneConfig: cluster.ControlPlaneConfig}

	err := r.reconcileControlPlaneScaleDown(context.Background(), cluster, cp)
	require.NoError(t, err, "a record-time floor refusal is the guard working, not a reconcile error")

	// The refusal happens before the row is written, so nothing is recorded.
	mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
}

// TestReconcileControlPlaneScaleDown_RecordsOneAtATime: a surplus greater than
// one still records exactly one delete per reconcile. The recorded node drops
// out of the eligible set next pass, so the loop paces the shrink one node at a
// time rather than tearing several down at once.
func TestReconcileControlPlaneScaleDown_RecordsOneAtATime(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	base := time.Now()
	cluster := db.Cluster{ID: clusterID, Name: "test-cluster", ControlPlaneConfig: db.ControlPlaneConfig{DesiredCount: 3}}

	// Five healthy nodes, desired 3: surplus is 2, but only one may be recorded.
	victim := healthyCPNode(clusterID, "cp-oldest", base.Add(-time.Hour))
	all := []db.Node{
		victim,
		healthyCPNode(clusterID, "cp-b", base),
		healthyCPNode(clusterID, "cp-c", base),
		healthyCPNode(clusterID, "cp-d", base),
		healthyCPNode(clusterID, "cp-e", base),
	}

	mockTx := &testutil.MockTx{}
	// DeleteWithTx reads the node unlocked and takes the CP mutation lock before
	// the FOR UPDATE read; both must succeed for the record path to proceed.
	mockTx.On("GetNode", mock.Anything, victim.ID).Return(victim, nil)
	mockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(cluster, nil)
	mockTx.On("GetNodeForUpdate", mock.Anything, victim.ID).Return(victim, nil)
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
	mockTx.AssertNumberOfCalls(t, "UpdateNode", 1)
}

// TestReconcileControlPlaneScaleDown_SkippedWhenAutoScaleDownDisabled pins the
// operator opt-out: with AutoScaleDownDisabled set, a real surplus of healthy
// nodes is left in place and scale-down records nothing, not even reaching the
// delete path.
func TestReconcileControlPlaneScaleDown_SkippedWhenAutoScaleDownDisabled(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	base := time.Now()
	cluster := db.Cluster{
		ID:   clusterID,
		Name: "test-cluster",
		ControlPlaneConfig: db.ControlPlaneConfig{
			DesiredCount:          3,
			AutoScaleDownDisabled: true,
		},
	}

	// Five healthy nodes, desired 3: a surplus of 2 that would otherwise be shed.
	all := []db.Node{
		healthyCPNode(clusterID, "cp-a", base.Add(-time.Hour)),
		healthyCPNode(clusterID, "cp-b", base),
		healthyCPNode(clusterID, "cp-c", base),
		healthyCPNode(clusterID, "cp-d", base),
		healthyCPNode(clusterID, "cp-e", base),
	}

	mockTx := &testutil.MockTx{}
	r := &reconciler{db: mockTx, logger: slog.Default()}
	cp := &controlPlane{nodes: all, clusterControlPlaneConfig: cluster.ControlPlaneConfig}

	err := r.reconcileControlPlaneScaleDown(context.Background(), cluster, cp)
	require.NoError(t, err)

	mockTx.AssertNotCalled(t, "GetNodeForUpdate", mock.Anything, mock.Anything)
	mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
}

// TestReconcileControlPlaneScaleDown_DefersDuringSurgeBeforeRecyclingCondition
// pins the surge window: a rollout has created a fresh (un-joined) surge node but
// the CPNodesRecycling condition is not set True yet. The drift clause of
// upgradeInProgress (EnableCPNodeRecycling && countNodesNeedingRollout > 0) must
// still defer scale-down, even though there is a nominal surplus above the floor,
// so the shrink never races the rollout's own teardown.
func TestReconcileControlPlaneScaleDown_DefersDuringSurgeBeforeRecyclingCondition(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	base := time.Now()

	// Recycling enabled, drift present, but the condition has not flipped yet.
	cluster := db.Cluster{
		ID:                    clusterID,
		Name:                  "test-cluster",
		EnableCPNodeRecycling: true,
		ControlPlaneConfig: db.ControlPlaneConfig{
			DesiredCount:           3,
			ControlPlaneConfigSpec: db.ControlPlaneConfigSpec{KubernetesVersion: "v1.34.2"},
		},
		// Conditions.CPNodesRecycling left unset: this is the pre-condition window.
	}

	// Four healthy, joined nodes still pinned to the old version (drifted), so a
	// rollout is owed. Desired is 3, so there is a nominal surplus of 1.
	driftConfig := uuid.New()
	drifted := func(name string, t time.Time) db.Node {
		n := healthyCPNode(clusterID, name, t)
		n.ControlPlaneConfigurationID = driftConfig
		return n
	}
	// A freshly-created surge node: not joined, not ready, so not scale-down eligible.
	surge := db.Node{
		ID:           uuid.New(),
		ClusterID:    clusterID,
		ControlPlane: true,
		Name:         "cp-surge",
		CreatedAt:    base,
		Conditions:   db.NodeConditions{Pending: db.NewCondition(true, "Pending")},
	}
	allNodes := []db.Node{
		drifted("cp-a", base.Add(-time.Hour)),
		drifted("cp-b", base),
		drifted("cp-c", base),
		drifted("cp-d", base),
		surge,
	}

	mockTx := &testutil.MockTx{}
	r := &reconciler{db: mockTx, logger: slog.Default()}
	cp := &controlPlane{
		nodes:                     allNodes,
		clusterControlPlaneConfig: cluster.ControlPlaneConfig,
		nodePinnedControlPlaneConfigs: map[uuid.UUID]db.ControlPlaneConfigSpec{
			driftConfig: {KubernetesVersion: "v1.33.6"},
		},
	}

	// Guard the scenario's own assumptions: the drift clause is active even with
	// the condition unset, and there really is a surplus the guard is suppressing.
	require.True(t, cp.upgradeInProgress(cluster), "the drift clause must defer while recycling is enabled with a roll owed")
	require.Len(t, cp.scaleDownEligible(), 4, "the un-joined surge node is not eligible; the four joined nodes are")

	err := r.reconcileControlPlaneScaleDown(context.Background(), cluster, cp)
	require.NoError(t, err)

	// Nothing recorded: the surge-before-recycling-condition window does not shrink.
	mockTx.AssertNotCalled(t, "GetNodeForUpdate", mock.Anything, mock.Anything)
	mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
}

// TestReconcileControlPlaneScaleDown_DefersDuringControlPlaneUpgrade pins the
// Kubernetes-upgrade window: the ControlPlaneUpgrading condition is True while a
// version upgrade is mid-flight, even on a cluster with recycling disabled (so
// neither the CPNodesRecycling clause nor the drift clause of upgradeInProgress
// fires). Scale-down must still defer, so a shrink never races the upgrade.
func TestReconcileControlPlaneScaleDown_DefersDuringControlPlaneUpgrade(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	base := time.Now()

	// Recycling disabled, no drift: only the ControlPlaneUpgrading condition gates.
	cluster := db.Cluster{
		ID:   clusterID,
		Name: "test-cluster",
		ControlPlaneConfig: db.ControlPlaneConfig{
			DesiredCount: 3,
		},
		Conditions: db.ClusterConditions{
			ControlPlaneUpgrading: db.NewCondition(true, "Upgrading to v1.34.2"),
		},
	}

	// Five healthy nodes, desired 3: a surplus of 2 that would otherwise be shed.
	all := []db.Node{
		healthyCPNode(clusterID, "cp-a", base.Add(-time.Hour)),
		healthyCPNode(clusterID, "cp-b", base),
		healthyCPNode(clusterID, "cp-c", base),
		healthyCPNode(clusterID, "cp-d", base),
		healthyCPNode(clusterID, "cp-e", base),
	}

	mockTx := &testutil.MockTx{}
	r := &reconciler{db: mockTx, logger: slog.Default()}
	cp := &controlPlane{nodes: all, clusterControlPlaneConfig: cluster.ControlPlaneConfig}

	// Guard the scenario: only the upgrade condition defers here; the other two
	// clauses are inert (recycling off, no drift), and a real surplus exists.
	require.True(t, cp.upgradeInProgress(cluster), "the ControlPlaneUpgrading condition must defer scale-down")
	require.Len(t, cp.scaleDownEligible(), 5, "all five healthy nodes are eligible")

	err := r.reconcileControlPlaneScaleDown(context.Background(), cluster, cp)
	require.NoError(t, err)

	// Nothing recorded: the control-plane upgrade window does not shrink.
	mockTx.AssertNotCalled(t, "GetNodeForUpdate", mock.Anything, mock.Anything)
	mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
}

// TestReconcileControlPlaneScaleDown_ShedsNotYetJoinedSurplusFirst pins the
// eligibility asymmetry fix: scale-up counts all nodes toward desiredCount, so a
// surplus can be a node that has not joined yet. That node must be shed before
// any healthy, joined member -- it is pure surplus, disrupts nothing, and is not
// an etcd member, so the quorum floor is not even consulted for it.
func TestReconcileControlPlaneScaleDown_ShedsNotYetJoinedSurplusFirst(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	base := time.Now()
	cluster := db.Cluster{ID: clusterID, Name: "test-cluster", ControlPlaneConfig: db.ControlPlaneConfig{DesiredCount: 3}}

	// Three joined members at desiredCount, plus one not-yet-joined surplus node.
	joinedA := healthyCPNode(clusterID, "cp-a", base.Add(-2*time.Hour))
	joinedB := healthyCPNode(clusterID, "cp-b", base.Add(-time.Hour))
	joinedC := healthyCPNode(clusterID, "cp-c", base.Add(-30*time.Minute))
	pending := healthyCPNode(clusterID, "cp-pending", base)
	pending.Conditions.Joined = db.NewCondition(false, "Not joined")
	pending.Conditions.Pending = db.NewCondition(true, "Pending")
	all := []db.Node{joinedA, joinedB, joinedC, pending}

	mockTx := &testutil.MockTx{}
	// DeleteWithTx reads the node unlocked and takes the CP mutation lock before
	// the FOR UPDATE read; both must succeed for the record path to proceed.
	mockTx.On("GetNode", mock.Anything, pending.ID).Return(pending, nil)
	mockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(cluster, nil)
	mockTx.On("GetNodeForUpdate", mock.Anything, pending.ID).Return(pending, nil)
	var written db.Node
	mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(n *db.Node) bool {
		written = *n
		return true
	})).Return(pending, nil)

	r := &reconciler{db: mockTx, logger: slog.Default()}
	cp := &controlPlane{nodes: all, clusterControlPlaneConfig: cluster.ControlPlaneConfig}

	err := r.reconcileControlPlaneScaleDown(context.Background(), cluster, cp)
	require.NoError(t, err)

	assert.Equal(t, pending.ID, written.ID, "the not-yet-joined surplus node is shed before any joined member")
	assert.True(t, nodes.IsDeleteRequested(written), "scale-down records intent")
	// The quorum floor exempts a node that never joined, so its census is never read.
	mockTx.AssertNotCalled(t, "ListNodes", mock.Anything, mock.Anything)
	mockTx.AssertNotCalled(t, "ListNodePools", mock.Anything, mock.Anything)
	// Only the pending node's row is touched; the joined members are left alone.
	mockTx.AssertNumberOfCalls(t, "UpdateNode", 1)
}
