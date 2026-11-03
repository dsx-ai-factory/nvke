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

	"github.com/dsx-ai-factory/nvke/controller/internal/clients"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/nodes"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

// rebalanceCluster builds a Nico cluster wired for rebalance: config pinned to
// configID with the ordered preference, a VIP, and the two flags. rebalanceEnabled
// maps to the inverted NodeRebalanceDisabled toggle; scaleDownDisabled sets the
// independent scale-down opt-out.
func rebalanceCluster(clusterID, configID uuid.UUID, preference []uuid.UUID, rebalanceEnabled, scaleDownDisabled bool) db.Cluster {
	var spec db.ControlPlaneConfigSpec
	spec.SetNico(db.NicoControlPlaneProvider{InstanceTypePreference: preference})
	return db.Cluster{
		ID:              clusterID,
		Name:            "test-cluster",
		ProviderType:    db.ProviderTypeNico,
		ControlPlaneVIP: "10.0.0.1",
		ControlPlaneConfig: db.ControlPlaneConfig{
			ConfigurationID:        configID,
			DesiredCount:           3,
			AutoScaleDownDisabled:  scaleDownDisabled,
			NodeRebalanceDisabled:  !rebalanceEnabled,
			ControlPlaneConfigSpec: spec,
		},
	}
}

// cpNodeOnType is a healthy, joined control-plane node placed on instanceType
// and pinned to configID.
func cpNodeOnType(clusterID, configID uuid.UUID, name string, instanceType uuid.UUID, createdAt time.Time) db.Node {
	node := healthyCPNode(clusterID, name, createdAt)
	node.ControlPlaneConfigurationID = configID
	node.SetNico(db.NicoNodeProvider{InstanceTypeID: instanceType})
	return node
}

func newRebalanceReconciler(tx *testutil.MockTx) *reconciler {
	return &reconciler{db: tx, logger: slog.Default()}
}

func newNodeProviderFor(t *testing.T, cluster db.Cluster) clients.NewNodeProvider {
	t.Helper()
	p, err := clients.GetNewNodeProvider(cluster)
	require.NoError(t, err)
	return p
}

// TestReconcileControlPlaneRebalance_SurgesMorePreferredReplacement pins the happy
// path: a joined node on a less-preferred type triggers a surge that is marked,
// pinned to a capped snapshot, and created one at a time. Scale-down is disabled
// here to show rebalance surges regardless of that toggle.
func TestReconcileControlPlaneRebalance_SurgesMorePreferredReplacement(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	configID := uuid.New()
	cappedID := uuid.New()
	base := time.Now()
	primaryType := uuid.New()
	worseType := uuid.New()
	cluster := rebalanceCluster(clusterID, configID, []uuid.UUID{primaryType, worseType}, true, true)

	nodesList := []db.Node{
		cpNodeOnType(clusterID, configID, "cp-a", primaryType, base),
		cpNodeOnType(clusterID, configID, "cp-b", primaryType, base),
		cpNodeOnType(clusterID, configID, "cp-old", worseType, base.Add(-time.Hour)),
	}

	mockTx := &testutil.MockTx{}
	mockTx.On("ListControlPlaneConfigurationsByCluster", mock.Anything, clusterID).
		Return([]db.ControlPlaneConfiguration{}, nil)
	// The capped snapshot keeps only the primary type (strictly better than the
	// worst node's rank 1).
	mockTx.On("CreateControlPlaneConfiguration", mock.Anything, mock.MatchedBy(func(row *db.ControlPlaneConfiguration) bool {
		nico, err := row.Config.NicoProvider()
		return err == nil &&
			len(nico.InstanceTypePreference) == 1 &&
			nico.InstanceTypePreference[0] == primaryType &&
			row.ChangeReason == rebalanceSnapshotReason
	})).Return(db.ControlPlaneConfiguration{ID: cappedID, ClusterID: clusterID}, nil)
	mockTx.On("ListClusterNodeNames", mock.Anything, clusterID).
		Return([]string{"cp-a", "cp-b", "cp-old"}, nil)

	var created db.Node
	mockTx.On("CreateNode", mock.Anything, mock.MatchedBy(func(node *db.Node) bool {
		created = *node
		return isRebalanceReplacement(*node) && node.ControlPlaneConfigurationID == cappedID
	})).Return(db.Node{}, nil)

	r := newRebalanceReconciler(mockTx)
	cp := &controlPlane{nodes: nodesList, clusterControlPlaneConfig: cluster.ControlPlaneConfig}

	acted, err := r.reconcileControlPlaneRebalance(context.Background(), cluster, cp, domainAllocator{}, newNodeProviderFor(t, cluster))
	require.NoError(t, err)
	assert.True(t, acted, "a surge changes membership, so the pass owns the reconcile and scale-down is skipped")

	mockTx.AssertNumberOfCalls(t, "CreateNode", 1)
	assert.True(t, isRebalanceReplacement(created), "the surge node carries the rebalance marker")
	assert.Equal(t, cappedID, created.ControlPlaneConfigurationID, "the surge is pinned to the capped snapshot")
	// A successful surge is progress, not a failure: it records no backoff, so the
	// next round (once the surge joins and sheds) follows without waiting.
	mockTx.AssertNotCalled(t, "UpdateCluster", mock.Anything, mock.Anything)
}

// TestReconcileControlPlaneRebalance_NoOps covers the gates that must surge
// nothing and write nothing.
func TestReconcileControlPlaneRebalance_NoOps(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	configID := uuid.New()
	base := time.Now()
	primaryType := uuid.New()
	worseType := uuid.New()

	balanced := []db.Node{
		cpNodeOnType(clusterID, configID, "cp-a", primaryType, base),
		cpNodeOnType(clusterID, configID, "cp-b", primaryType, base),
		cpNodeOnType(clusterID, configID, "cp-c", primaryType, base),
	}
	unbalanced := []db.Node{
		cpNodeOnType(clusterID, configID, "cp-a", primaryType, base),
		cpNodeOnType(clusterID, configID, "cp-b", primaryType, base),
		cpNodeOnType(clusterID, configID, "cp-old", worseType, base),
	}

	// A CAPI cluster expresses no ordered preference, so the ranker is nil.
	// NodeRebalanceDisabled left false (the default): rebalancing is on, and the
	// nil ranker is what makes this a no-op, not the flag.
	capiCluster := db.Cluster{
		ID: clusterID, Name: "test-cluster", ProviderType: db.ProviderTypeCAPI,
		ControlPlaneVIP:    "10.0.0.1",
		ControlPlaneConfig: db.ControlPlaneConfig{ConfigurationID: configID, DesiredCount: 3},
	}

	// A less-preferred node exists, but a surge was abandoned for lack of capacity
	// within the backoff window, so the step waits rather than churning another.
	coolingDown := rebalanceCluster(clusterID, configID, []uuid.UUID{primaryType, worseType}, true, false)
	coolingDown.Conditions.ControlPlaneRebalanceBackoff = db.NewCondition(true, "recent abandoned surge")

	// A control plane whose desiredCount is below the fault-tolerant floor can
	// never shed the node a surge would add -- the quorum floor refuses the shed --
	// so it must not surge in the first place, even with a less-preferred node.
	subFloor := rebalanceCluster(clusterID, configID, []uuid.UUID{primaryType, worseType}, true, false)
	subFloor.ControlPlaneConfig.DesiredCount = nodes.MinFaultTolerantControlPlaneNodes - 1
	subFloorNodes := []db.Node{
		cpNodeOnType(clusterID, configID, "cp-a", primaryType, base),
		cpNodeOnType(clusterID, configID, "cp-old", worseType, base),
	}

	tests := []struct {
		name    string
		cluster db.Cluster
		nodes   []db.Node
	}{
		{name: "flag off", cluster: rebalanceCluster(clusterID, configID, []uuid.UUID{primaryType, worseType}, false, false), nodes: unbalanced},
		{name: "provider has no ranker", cluster: capiCluster, nodes: unbalanced},
		{name: "already balanced on the primary type", cluster: rebalanceCluster(clusterID, configID, []uuid.UUID{primaryType, worseType}, true, false), nodes: balanced},
		{name: "backing off after an abandoned surge", cluster: coolingDown, nodes: unbalanced},
		{name: "desiredCount below the fault-tolerant floor", cluster: subFloor, nodes: subFloorNodes},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mockTx := &testutil.MockTx{}
			r := newRebalanceReconciler(mockTx)
			cp := &controlPlane{nodes: tt.nodes, clusterControlPlaneConfig: tt.cluster.ControlPlaneConfig}

			acted, err := r.reconcileControlPlaneRebalance(context.Background(), tt.cluster, cp, domainAllocator{}, nil)
			require.NoError(t, err)
			assert.False(t, acted, "a no-op pass must not short-circuit scale-down")

			mockTx.AssertNotCalled(t, "CreateControlPlaneConfiguration", mock.Anything, mock.Anything)
			mockTx.AssertNotCalled(t, "CreateNode", mock.Anything, mock.Anything)
			mockTx.AssertNotCalled(t, "GetNodeForUpdate", mock.Anything, mock.Anything)
			mockTx.AssertNotCalled(t, "UpdateCluster", mock.Anything, mock.Anything)
		})
	}
}

// TestRebalanceCoolingDown pins the re-probe backoff: a surge abandoned for lack
// of capacity within rebalanceBackoffInterval blocks another, and the window
// reopens once it elapses.
func TestRebalanceCoolingDown(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		last time.Time
		want bool
	}{
		{name: "never abandoned", last: time.Time{}, want: false},
		{name: "just abandoned", last: time.Now(), want: true},
		{name: "within the interval", last: time.Now().Add(-rebalanceBackoffInterval + time.Minute), want: true},
		{name: "interval elapsed", last: time.Now().Add(-rebalanceBackoffInterval - time.Minute), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var cluster db.Cluster
			if !tt.last.IsZero() {
				cluster.Conditions.ControlPlaneRebalanceBackoff = db.Condition{
					Status:             db.ConditionStatusTrue,
					LastTransitionTime: tt.last,
				}
			}

			assert.Equal(t, tt.want, rebalanceCoolingDown(cluster))
		})
	}
}

// TestReconcileControlPlaneRebalance_DefersWhileRolloutOwed pins that a pending
// rollout (drift) blocks a new surge even with recycling disabled, so rebalance
// never muddies an operator's pending config change.
func TestReconcileControlPlaneRebalance_DefersWhileRolloutOwed(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	configID := uuid.New()
	base := time.Now()
	primaryType := uuid.New()
	worseType := uuid.New()
	cluster := rebalanceCluster(clusterID, configID, []uuid.UUID{primaryType, worseType}, true, false)
	cluster.ControlPlaneConfig.KubernetesVersion = "v1.34.2"

	nodesList := []db.Node{
		cpNodeOnType(clusterID, configID, "cp-a", primaryType, base),
		cpNodeOnType(clusterID, configID, "cp-b", primaryType, base),
		cpNodeOnType(clusterID, configID, "cp-old", worseType, base),
	}

	mockTx := &testutil.MockTx{}
	r := newRebalanceReconciler(mockTx)
	cp := &controlPlane{
		nodes:                     nodesList,
		clusterControlPlaneConfig: cluster.ControlPlaneConfig,
		// Every node is pinned to an older version snapshot, so a rollout is owed.
		nodePinnedControlPlaneConfigs: map[uuid.UUID]db.ControlPlaneConfigSpec{
			configID: {KubernetesVersion: "v1.33.6"},
		},
	}
	require.Positive(t, cp.countNodesNeedingRollout(), "the scenario must have a rollout owed")

	acted, err := r.reconcileControlPlaneRebalance(context.Background(), cluster, cp, domainAllocator{}, nil)
	require.NoError(t, err)
	assert.False(t, acted, "deferring while a rollout is owed is a no-op; scale-down still runs")

	mockTx.AssertNotCalled(t, "CreateControlPlaneConfiguration", mock.Anything, mock.Anything)
	mockTx.AssertNotCalled(t, "CreateNode", mock.Anything, mock.Anything)
}

// TestReconcileControlPlaneRebalance_WaitsWhileSurgeComingUp pins that an
// in-flight, not-yet-joined surge is left to come up: no new surge, no teardown.
func TestReconcileControlPlaneRebalance_WaitsWhileSurgeComingUp(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	configID := uuid.New()
	base := time.Now()
	primaryType := uuid.New()
	worseType := uuid.New()
	cluster := rebalanceCluster(clusterID, configID, []uuid.UUID{primaryType, worseType}, true, false)

	surge := healthyCPNode(clusterID, "cp-surge", base)
	surge.Conditions.Joined = db.NewCondition(false, "Not joined")
	surge.Conditions.Pending = db.NewCondition(true, "Pending")
	surge.Conditions.ControlPlanePreferredReplacement = db.NewCondition(true, "Rebalance surge")
	nodesList := []db.Node{
		cpNodeOnType(clusterID, configID, "cp-a", primaryType, base),
		cpNodeOnType(clusterID, configID, "cp-b", primaryType, base),
		cpNodeOnType(clusterID, configID, "cp-old", worseType, base),
		surge,
	}

	mockTx := &testutil.MockTx{}
	r := newRebalanceReconciler(mockTx)
	cp := &controlPlane{nodes: nodesList, clusterControlPlaneConfig: cluster.ControlPlaneConfig}

	acted, err := r.reconcileControlPlaneRebalance(context.Background(), cluster, cp, domainAllocator{}, nil)
	require.NoError(t, err)
	assert.False(t, acted, "waiting for a surge to come up is a no-op; scale-down still runs")

	mockTx.AssertNotCalled(t, "CreateNode", mock.Anything, mock.Anything)
	mockTx.AssertNotCalled(t, "GetNodeForUpdate", mock.Anything, mock.Anything)
	mockTx.AssertNotCalled(t, "CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// TestReconcileControlPlaneRebalance_FinishesJoinedSurge pins the completion step:
// once the surge joins, rebalance sheds the old node through the delete path, then
// clears the marker while leaving the surge pinned to its capped snapshot (no
// re-pin). Scale-down is disabled here to show rebalance does the shed itself.
func TestReconcileControlPlaneRebalance_FinishesJoinedSurge(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	configID := uuid.New()
	cappedID := uuid.New()
	base := time.Now()
	primaryType := uuid.New()
	worseType := uuid.New()
	cluster := rebalanceCluster(clusterID, configID, []uuid.UUID{primaryType, worseType}, true, true)

	surge := cpNodeOnType(clusterID, cappedID, "cp-surge", primaryType, base)
	surge.Conditions.ControlPlanePreferredReplacement = db.NewCondition(true, "Rebalance surge")
	old := cpNodeOnType(clusterID, configID, "cp-old", worseType, base.Add(-time.Hour))
	// Four nodes at desired 3 (the surge is the +1): shedding the old node leaves
	// three joined members, so the quorum floor permits the shed.
	nodesList := []db.Node{
		cpNodeOnType(clusterID, configID, "cp-a", primaryType, base),
		cpNodeOnType(clusterID, configID, "cp-b", primaryType, base),
		old,
		surge,
	}

	mockTx := &testutil.MockTx{}
	// Self-shed of the old node through the shared CP delete path.
	mockTx.On("GetNode", mock.Anything, old.ID).Return(old, nil)
	mockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(cluster, nil)
	mockTx.On("GetNodeForUpdate", mock.Anything, old.ID).Return(old, nil)
	mockTx.On("ListNodePools", mock.Anything, mock.Anything).Return([]db.NodePool{}, nil)
	mockTx.On("ListNodes", mock.Anything, mock.Anything).Return(nodesList, nil)
	// Clearing the marker locks and re-writes the surge node.
	mockTx.On("GetNodeForUpdate", mock.Anything, surge.ID).Return(surge, nil)

	var writes []db.Node
	mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(node *db.Node) bool {
		writes = append(writes, *node)
		return true
	})).Return(surge, nil)

	r := newRebalanceReconciler(mockTx)
	cp := &controlPlane{nodes: nodesList, clusterControlPlaneConfig: cluster.ControlPlaneConfig}

	acted, err := r.reconcileControlPlaneRebalance(context.Background(), cluster, cp, domainAllocator{}, newNodeProviderFor(t, cluster))
	require.NoError(t, err)
	assert.True(t, acted, "shedding the old node changes membership, so the pass owns the reconcile")

	require.Len(t, writes, 2, "one write records the old node's delete, one clears the surge marker")

	var oldWrite, surgeWrite db.Node
	for _, w := range writes {
		switch w.ID {
		case old.ID:
			oldWrite = w
		case surge.ID:
			surgeWrite = w
		}
	}

	assert.True(t, nodes.IsDeleteRequested(oldWrite), "the old less-preferred node is shed through the delete path")
	assert.False(t, isRebalanceReplacement(surgeWrite), "the marker is cleared once the surge joins")
	assert.Equal(t, cappedID, surgeWrite.ControlPlaneConfigurationID, "the joined surge keeps its capped snapshot pin (no re-pin)")
	mockTx.AssertNotCalled(t, "CreateNode", mock.Anything, mock.Anything)
}

// TestReconcileControlPlaneRebalance_KeepsMarkerWhileUnrelatedDeleteInFlight pins
// that a joined surge does not drop its marker while some other control-plane
// delete is in flight. The in-flight node here is not yet joined, even though it
// sits on the less-preferred type, so it is not the shed. Clearing the marker
// would leave the surge in place above desiredCount, and with automatic
// scale-down disabled nothing else would remove it.
func TestReconcileControlPlaneRebalance_KeepsMarkerWhileUnrelatedDeleteInFlight(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	configID := uuid.New()
	cappedID := uuid.New()
	base := time.Now()
	primaryType := uuid.New()
	worseType := uuid.New()
	cluster := rebalanceCluster(clusterID, configID, []uuid.UUID{primaryType, worseType}, true, true)

	surge := cpNodeOnType(clusterID, cappedID, "cp-surge", primaryType, base)
	surge.Conditions.ControlPlanePreferredReplacement = db.NewCondition(true, "Rebalance surge")
	pending := cpNodeOnType(clusterID, configID, "cp-pending", worseType, base)
	pending.Conditions.Joined = db.NewCondition(false, "Not joined")
	pending.Conditions.Pending = db.NewCondition(true, "Pending")
	pending.Conditions.DeleteRequested = db.NewCondition(true, "Delete accepted")
	nodesList := []db.Node{
		cpNodeOnType(clusterID, configID, "cp-a", primaryType, base),
		cpNodeOnType(clusterID, configID, "cp-old", worseType, base.Add(-time.Hour)),
		pending,
		surge,
	}

	mockTx := &testutil.MockTx{}
	r := newRebalanceReconciler(mockTx)
	cp := &controlPlane{nodes: nodesList, clusterControlPlaneConfig: cluster.ControlPlaneConfig}

	acted, err := r.reconcileControlPlaneRebalance(context.Background(), cluster, cp, domainAllocator{}, nil)
	require.NoError(t, err)
	assert.False(t, acted, "an unrelated delete is not the shed, so this pass changes nothing")

	mockTx.AssertNotCalled(t, "GetNodeForUpdate", mock.Anything, mock.Anything)
	mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
	mockTx.AssertNotCalled(t, "GetNode", mock.Anything, mock.Anything)
}

// TestReconcileControlPlaneRebalance_ClearsMarkerWhenShedAlreadyInFlight pins the
// retry: a shed recorded on an earlier pass, whose marker clear did not land, is
// already in flight. The marker is cleared and a second victim is not selected.
func TestReconcileControlPlaneRebalance_ClearsMarkerWhenShedAlreadyInFlight(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	configID := uuid.New()
	cappedID := uuid.New()
	base := time.Now()
	primaryType := uuid.New()
	worseType := uuid.New()
	cluster := rebalanceCluster(clusterID, configID, []uuid.UUID{primaryType, worseType}, true, true)

	surge := cpNodeOnType(clusterID, cappedID, "cp-surge", primaryType, base)
	surge.Conditions.ControlPlanePreferredReplacement = db.NewCondition(true, "Rebalance surge")
	old := cpNodeOnType(clusterID, configID, "cp-old", worseType, base.Add(-time.Hour))
	old.Conditions.DeleteRequested = db.NewCondition(true, "Delete accepted")
	nodesList := []db.Node{
		cpNodeOnType(clusterID, configID, "cp-a", primaryType, base),
		cpNodeOnType(clusterID, configID, "cp-b", primaryType, base),
		old,
		surge,
	}

	mockTx := &testutil.MockTx{}
	mockTx.On("GetNodeForUpdate", mock.Anything, surge.ID).Return(surge, nil)
	var written db.Node
	mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(node *db.Node) bool {
		written = *node
		return true
	})).Return(surge, nil)

	r := newRebalanceReconciler(mockTx)
	cp := &controlPlane{nodes: nodesList, clusterControlPlaneConfig: cluster.ControlPlaneConfig}

	acted, err := r.reconcileControlPlaneRebalance(context.Background(), cluster, cp, domainAllocator{}, nil)
	require.NoError(t, err)
	assert.True(t, acted, "clearing the marker after the shed was recorded finishes the surge")

	assert.False(t, isRebalanceReplacement(written), "the marker is cleared once the shed is in flight")
	assert.Equal(t, surge.ID, written.ID)
	mockTx.AssertNotCalled(t, "GetNode", mock.Anything, mock.Anything)
	mockTx.AssertNumberOfCalls(t, "UpdateNode", 1)
}

// TestReconcileControlPlaneRebalance_RemovesSurgeThatIsNotAnImprovement pins that
// a joined surge which is not strictly better than any other member is removed
// by rebalance itself when the control plane is still above desiredCount.
// Automatic scale-down is disabled, so leaving the surge and clearing the marker
// would strand it.
func TestReconcileControlPlaneRebalance_RemovesSurgeThatIsNotAnImprovement(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	configID := uuid.New()
	cappedID := uuid.New()
	base := time.Now()
	primaryType := uuid.New()
	cluster := rebalanceCluster(clusterID, configID, []uuid.UUID{primaryType}, true, true)

	surge := cpNodeOnType(clusterID, cappedID, "cp-surge", primaryType, base)
	surge.Conditions.ControlPlanePreferredReplacement = db.NewCondition(true, "Rebalance surge")
	nodesList := []db.Node{
		cpNodeOnType(clusterID, configID, "cp-a", primaryType, base),
		cpNodeOnType(clusterID, configID, "cp-b", primaryType, base),
		cpNodeOnType(clusterID, configID, "cp-c", primaryType, base.Add(-time.Hour)),
		surge,
	}

	mockTx := &testutil.MockTx{}
	mockTx.On("GetNode", mock.Anything, surge.ID).Return(surge, nil)
	mockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(cluster, nil)
	mockTx.On("GetNodeForUpdate", mock.Anything, surge.ID).Return(surge, nil)
	mockTx.On("ListNodePools", mock.Anything, mock.Anything).Return([]db.NodePool{}, nil)
	mockTx.On("ListNodes", mock.Anything, mock.Anything).Return(nodesList, nil)
	var written db.Node
	mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(node *db.Node) bool {
		written = *node
		return true
	})).Return(surge, nil)

	r := newRebalanceReconciler(mockTx)
	cp := &controlPlane{nodes: nodesList, clusterControlPlaneConfig: cluster.ControlPlaneConfig}

	acted, err := r.reconcileControlPlaneRebalance(context.Background(), cluster, cp, domainAllocator{}, nil)
	require.NoError(t, err)
	assert.True(t, acted, "removing the surge that is not an improvement changes membership")

	assert.Equal(t, surge.ID, written.ID)
	assert.True(t, nodes.IsDeleteRequested(written), "the surge is removed through the control-plane delete path")
	assert.True(t, isRebalanceReplacement(written), "the marker stays until the surge is gone, so a second surge cannot start")
	mockTx.AssertNumberOfCalls(t, "UpdateNode", 1)
}

// TestReconcileControlPlaneRebalance_ClearsMarkerWhenSurgeFillsDesiredCount pins
// that a joined surge with nothing worse to shed becomes an ordinary member once
// the control plane is already at desiredCount. A departure while it was coming
// up consumed the extra slot, so there is no surplus to strand.
func TestReconcileControlPlaneRebalance_ClearsMarkerWhenSurgeFillsDesiredCount(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	configID := uuid.New()
	cappedID := uuid.New()
	base := time.Now()
	primaryType := uuid.New()
	cluster := rebalanceCluster(clusterID, configID, []uuid.UUID{primaryType}, true, true)

	surge := cpNodeOnType(clusterID, cappedID, "cp-surge", primaryType, base)
	surge.Conditions.ControlPlanePreferredReplacement = db.NewCondition(true, "Rebalance surge")
	nodesList := []db.Node{
		cpNodeOnType(clusterID, configID, "cp-a", primaryType, base),
		cpNodeOnType(clusterID, configID, "cp-b", primaryType, base),
		surge,
	}

	mockTx := &testutil.MockTx{}
	mockTx.On("GetNodeForUpdate", mock.Anything, surge.ID).Return(surge, nil)
	var written db.Node
	mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(node *db.Node) bool {
		written = *node
		return true
	})).Return(surge, nil)

	r := newRebalanceReconciler(mockTx)
	cp := &controlPlane{nodes: nodesList, clusterControlPlaneConfig: cluster.ControlPlaneConfig}

	acted, err := r.reconcileControlPlaneRebalance(context.Background(), cluster, cp, domainAllocator{}, nil)
	require.NoError(t, err)
	assert.True(t, acted, "the surge has become an ordinary member")

	assert.False(t, isRebalanceReplacement(written), "the marker is cleared once the control plane is at desired count")
	assert.False(t, nodes.IsDeleteRequested(written), "a surge that filled a departure is not removed")
	mockTx.AssertNotCalled(t, "GetNode", mock.Anything, mock.Anything)
}

// TestReconcileControlPlaneRebalance_KeepsMarkerWhenFloorRefusesShed pins that a
// quorum-floor refusal above desiredCount leaves the marker in place. Clearing it
// would strand the extra node, and with automatic scale-down disabled the shed
// would never be retried.
func TestReconcileControlPlaneRebalance_KeepsMarkerWhenFloorRefusesShed(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	configID := uuid.New()
	cappedID := uuid.New()
	base := time.Now()
	primaryType := uuid.New()
	worseType := uuid.New()
	cluster := rebalanceCluster(clusterID, configID, []uuid.UUID{primaryType, worseType}, true, true)

	surge := cpNodeOnType(clusterID, cappedID, "cp-surge", primaryType, base)
	surge.Conditions.ControlPlanePreferredReplacement = db.NewCondition(true, "Rebalance surge")
	old := cpNodeOnType(clusterID, configID, "cp-old", worseType, base.Add(-time.Hour))
	// Three joined members plus a not-yet-joined node: shedding the old joined
	// member would leave two, which the quorum floor refuses, while the control
	// plane is still above desiredCount.
	pending := cpNodeOnType(clusterID, configID, "cp-pending", primaryType, base)
	pending.Conditions.Joined = db.NewCondition(false, "Not joined")
	pending.Conditions.Pending = db.NewCondition(true, "Pending")
	nodesList := []db.Node{
		cpNodeOnType(clusterID, configID, "cp-a", primaryType, base),
		old,
		pending,
		surge,
	}

	mockTx := &testutil.MockTx{}
	mockTx.On("GetNode", mock.Anything, old.ID).Return(old, nil)
	mockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(cluster, nil)
	mockTx.On("GetNodeForUpdate", mock.Anything, old.ID).Return(old, nil)
	mockTx.On("ListNodePools", mock.Anything, mock.Anything).Return([]db.NodePool{}, nil)
	mockTx.On("ListNodes", mock.Anything, mock.Anything).Return(nodesList, nil)

	r := newRebalanceReconciler(mockTx)
	cp := &controlPlane{nodes: nodesList, clusterControlPlaneConfig: cluster.ControlPlaneConfig}

	acted, err := r.reconcileControlPlaneRebalance(context.Background(), cluster, cp, domainAllocator{}, nil)
	require.NoError(t, err)
	assert.False(t, acted, "a floor refusal above desiredCount retries the shed and changes nothing this pass")

	mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
}

// TestReconcileControlPlaneRebalance_AbandonsSurgeOnUpgrade pins the yield rule:
// a not-yet-joined surge is torn down when an upgrade starts, so it never blocks
// the upgrade.
func TestReconcileControlPlaneRebalance_AbandonsSurgeOnUpgrade(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	configID := uuid.New()
	cappedID := uuid.New()
	base := time.Now()
	primaryType := uuid.New()
	worseType := uuid.New()
	cluster := rebalanceCluster(clusterID, configID, []uuid.UUID{primaryType, worseType}, true, false)
	cluster.Conditions.ControlPlaneUpgrading = db.NewCondition(true, "Upgrading")

	surge := healthyCPNode(clusterID, "cp-surge", base)
	surge.ControlPlaneConfigurationID = cappedID
	surge.Conditions.Joined = db.NewCondition(false, "Not joined")
	surge.Conditions.Pending = db.NewCondition(true, "Pending")
	surge.Conditions.ControlPlanePreferredReplacement = db.NewCondition(true, "Rebalance surge")
	nodesList := []db.Node{
		cpNodeOnType(clusterID, configID, "cp-a", primaryType, base),
		cpNodeOnType(clusterID, configID, "cp-b", primaryType, base),
		cpNodeOnType(clusterID, configID, "cp-c", primaryType, base),
		surge,
	}

	mockTx := &testutil.MockTx{}
	mockTx.On("GetNodeForUpdate", mock.Anything, surge.ID).Return(surge, nil)
	mockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(cluster, nil)
	var written db.Node
	mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(node *db.Node) bool {
		written = *node
		return true
	})).Return(surge, nil)
	mockTx.On("CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(db.WorkflowOutbox{}, nil)

	r := newRebalanceReconciler(mockTx)
	cp := &controlPlane{nodes: nodesList, clusterControlPlaneConfig: cluster.ControlPlaneConfig}

	acted, err := r.reconcileControlPlaneRebalance(context.Background(), cluster, cp, domainAllocator{}, nil)
	require.NoError(t, err)
	assert.True(t, acted, "abandoning the surge changes membership, so the pass owns the reconcile")

	assert.True(t, nodes.IsDeleting(written), "the surge is torn down to yield to the upgrade")
	mockTx.AssertNumberOfCalls(t, "CreateWorkflowOutboxEntry", 1)
	mockTx.AssertNotCalled(t, "CreateNode", mock.Anything, mock.Anything)
	// Yielding to an upgrade is a priority preemption, not a capacity failure, so
	// it records no backoff: rebalance resumes as soon as the upgrade clears.
	mockTx.AssertNotCalled(t, "UpdateCluster", mock.Anything, mock.Anything)
}

// TestReconcileControlPlaneRebalance_BacksOffWhenSurgeHasNoCapacity pins the
// backoff rule: a surge that never joins within the timeout is abandoned for lack
// of capacity and records a backoff, so the next round re-probes slowly instead of
// churning a surge every timeout.
func TestReconcileControlPlaneRebalance_BacksOffWhenSurgeHasNoCapacity(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	configID := uuid.New()
	cappedID := uuid.New()
	base := time.Now()
	primaryType := uuid.New()
	worseType := uuid.New()
	cluster := rebalanceCluster(clusterID, configID, []uuid.UUID{primaryType, worseType}, true, false)

	// A surge that has been coming up longer than the timeout: no capacity.
	surge := healthyCPNode(clusterID, "cp-surge", base)
	surge.ControlPlaneConfigurationID = cappedID
	surge.Conditions.Joined = db.NewCondition(false, "Not joined")
	surge.Conditions.Pending = db.Condition{
		Status:             db.ConditionStatusTrue,
		Message:            "Pending",
		LastTransitionTime: time.Now().Add(-defaultRebalanceSurgeTimeout - time.Minute),
	}
	surge.Conditions.ControlPlanePreferredReplacement = db.NewCondition(true, "Rebalance surge")
	nodesList := []db.Node{
		cpNodeOnType(clusterID, configID, "cp-a", primaryType, base),
		cpNodeOnType(clusterID, configID, "cp-b", primaryType, base),
		cpNodeOnType(clusterID, configID, "cp-c", primaryType, base),
		surge,
	}

	mockTx := &testutil.MockTx{}
	mockTx.On("GetNodeForUpdate", mock.Anything, surge.ID).Return(surge, nil)
	mockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(cluster, nil)
	var written db.Node
	mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(node *db.Node) bool {
		written = *node
		return true
	})).Return(surge, nil)
	mockTx.On("CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(db.WorkflowOutbox{}, nil)
	// The abandonment records the backoff on the cluster.
	mockTx.On("GetCluster", mock.Anything, clusterID).Return(cluster, nil)
	var recorded db.Cluster
	mockTx.On("UpdateCluster", mock.Anything, mock.MatchedBy(func(c *db.Cluster) bool {
		recorded = *c
		return true
	})).Return(db.Cluster{}, nil)

	r := newRebalanceReconciler(mockTx)
	cp := &controlPlane{nodes: nodesList, clusterControlPlaneConfig: cluster.ControlPlaneConfig}

	acted, err := r.reconcileControlPlaneRebalance(context.Background(), cluster, cp, domainAllocator{}, nil)
	require.NoError(t, err)
	assert.True(t, acted, "abandoning the stuck surge changes membership, so the pass owns the reconcile")

	assert.True(t, nodes.IsDeleting(written), "the stuck surge is torn down")
	assert.Equal(t, db.ConditionStatusTrue, recorded.Conditions.ControlPlaneRebalanceBackoff.Status, "a no-capacity abandonment records the backoff")
	assert.False(t, recorded.Conditions.ControlPlaneRebalanceBackoff.LastTransitionTime.IsZero(), "the backoff carries a timestamp")
	mockTx.AssertNotCalled(t, "CreateNode", mock.Anything, mock.Anything)
}

// TestReconcileControlPlaneRebalance_KeepsAllocatedSurgePastTimeout pins that the
// timeout abandons a surge only when the provider never allocated an instance. A
// surge that is past the timeout but has a backing Forge instance found capacity
// and is coming up, so it is left alone -- not torn down, and no capacity backoff
// recorded.
func TestReconcileControlPlaneRebalance_KeepsAllocatedSurgePastTimeout(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	configID := uuid.New()
	cappedID := uuid.New()
	base := time.Now()
	primaryType := uuid.New()
	worseType := uuid.New()
	cluster := rebalanceCluster(clusterID, configID, []uuid.UUID{primaryType, worseType}, true, false)

	// Past the timeout, but the provider allocated a Forge instance: capacity was
	// found, so this is not a capacity failure.
	surge := healthyCPNode(clusterID, "cp-surge", base)
	surge.ControlPlaneConfigurationID = cappedID
	surge.SetNico(db.NicoNodeProvider{InstanceTypeID: primaryType, ForgeInstanceID: "i-allocated"})
	surge.Conditions.Joined = db.NewCondition(false, "Not joined")
	surge.Conditions.Pending = db.Condition{
		Status:             db.ConditionStatusTrue,
		Message:            "Pending",
		LastTransitionTime: time.Now().Add(-defaultRebalanceSurgeTimeout - time.Minute),
	}
	surge.Conditions.ControlPlanePreferredReplacement = db.NewCondition(true, "Rebalance surge")
	nodesList := []db.Node{
		cpNodeOnType(clusterID, configID, "cp-a", primaryType, base),
		cpNodeOnType(clusterID, configID, "cp-b", primaryType, base),
		cpNodeOnType(clusterID, configID, "cp-c", primaryType, base),
		surge,
	}

	mockTx := &testutil.MockTx{}
	r := newRebalanceReconciler(mockTx)
	cp := &controlPlane{nodes: nodesList, clusterControlPlaneConfig: cluster.ControlPlaneConfig}

	acted, err := r.reconcileControlPlaneRebalance(context.Background(), cluster, cp, domainAllocator{}, nil)
	require.NoError(t, err)
	assert.False(t, acted, "an allocated surge is left to come up, so this pass changes nothing")

	mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
	mockTx.AssertNotCalled(t, "UpdateCluster", mock.Anything, mock.Anything)
	mockTx.AssertNotCalled(t, "CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// TestReconcileControlPlaneRebalance_CappedSurgeRolloutDrift pins the correctness
// the no-re-pin rule buys. A joined surge is left pinned to its capped snapshot;
// because we never re-pin it to the cluster's current config, the pin stays
// honest, so rollout drift is computed against what the surge was actually built
// on. The cap itself (a truncated preference) is a non-rollout field, so it never
// looks like drift on its own; but a config change in a rollout field while the
// surge was provisioning is still visible, so the stale node gets recycled rather
// than silently stranded.
func TestReconcileControlPlaneRebalance_CappedSurgeRolloutDrift(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	currentID := uuid.New()
	cappedID := uuid.New()
	base := time.Now()
	primaryType := uuid.New()
	worseType := uuid.New()

	cluster := rebalanceCluster(clusterID, currentID, []uuid.UUID{primaryType, worseType}, true, false)
	cluster.ControlPlaneConfig.KubernetesVersion = "v1.34.2"

	surge := cpNodeOnType(clusterID, cappedID, "cp-surge", primaryType, base)

	cappedSameVersion := db.ControlPlaneConfigSpec{KubernetesVersion: "v1.34.2"}
	cappedSameVersion.SetNico(db.NicoControlPlaneProvider{InstanceTypePreference: []uuid.UUID{primaryType}})

	cappedStaleVersion := db.ControlPlaneConfigSpec{KubernetesVersion: "v1.33.6"}
	cappedStaleVersion.SetNico(db.NicoControlPlaneProvider{InstanceTypePreference: []uuid.UUID{primaryType}})

	tests := []struct {
		name       string
		cappedSpec db.ControlPlaneConfigSpec
		wantCount  int
	}{
		{
			name:       "capped preference alone is not rollout drift",
			cappedSpec: cappedSameVersion,
			wantCount:  0,
		},
		{
			name:       "stale capped snapshot after a config change is still drift",
			cappedSpec: cappedStaleVersion,
			wantCount:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cp := &controlPlane{
				nodes:                     []db.Node{surge},
				clusterControlPlaneConfig: cluster.ControlPlaneConfig,
				nodePinnedControlPlaneConfigs: map[uuid.UUID]db.ControlPlaneConfigSpec{
					cappedID: tt.cappedSpec,
				},
			}

			assert.Equal(t, tt.wantCount, cp.countNodesNeedingRollout())
		})
	}
}

// TestReconcileControlPlaneRebalance_ResolvesInFlightSurgeWhenDisabled pins that
// flipping NodeRebalanceDisabled to true mid-surge still resolves the in-flight
// surge: a joined surge sheds its less-preferred node rather than being stranded
// one node above the desired count. The opt-out gates only new surges.
func TestReconcileControlPlaneRebalance_ResolvesInFlightSurgeWhenDisabled(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	configID := uuid.New()
	cappedID := uuid.New()
	base := time.Now()
	primaryType := uuid.New()
	worseType := uuid.New()
	// Rebalancing disabled: a new surge would never start, but an existing one
	// must still be driven to resolution.
	cluster := rebalanceCluster(clusterID, configID, []uuid.UUID{primaryType, worseType}, false, true)

	surge := cpNodeOnType(clusterID, cappedID, "cp-surge", primaryType, base)
	surge.Conditions.ControlPlanePreferredReplacement = db.NewCondition(true, "Rebalance surge")
	old := cpNodeOnType(clusterID, configID, "cp-old", worseType, base.Add(-time.Hour))
	nodesList := []db.Node{
		cpNodeOnType(clusterID, configID, "cp-a", primaryType, base),
		cpNodeOnType(clusterID, configID, "cp-b", primaryType, base),
		old,
		surge,
	}

	mockTx := &testutil.MockTx{}
	mockTx.On("GetNode", mock.Anything, old.ID).Return(old, nil)
	mockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(cluster, nil)
	mockTx.On("GetNodeForUpdate", mock.Anything, old.ID).Return(old, nil)
	mockTx.On("ListNodePools", mock.Anything, mock.Anything).Return([]db.NodePool{}, nil)
	mockTx.On("ListNodes", mock.Anything, mock.Anything).Return(nodesList, nil)
	mockTx.On("GetNodeForUpdate", mock.Anything, surge.ID).Return(surge, nil)

	var writes []db.Node
	mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(node *db.Node) bool {
		writes = append(writes, *node)
		return true
	})).Return(surge, nil)

	r := newRebalanceReconciler(mockTx)
	cp := &controlPlane{nodes: nodesList, clusterControlPlaneConfig: cluster.ControlPlaneConfig}

	acted, err := r.reconcileControlPlaneRebalance(context.Background(), cluster, cp, domainAllocator{}, newNodeProviderFor(t, cluster))
	require.NoError(t, err)
	assert.True(t, acted, "an in-flight surge is resolved even while rebalancing is disabled")

	var oldWrite db.Node
	for _, w := range writes {
		if w.ID == old.ID {
			oldWrite = w
		}
	}
	assert.True(t, nodes.IsDeleteRequested(oldWrite), "the less-preferred node is still shed when the opt-out is flipped mid-surge")
	mockTx.AssertNotCalled(t, "CreateNode", mock.Anything, mock.Anything)
}

// TestReconcileControlPlaneRebalance_NoBackoffWhenAbandonDeleteInFlight pins that
// when a surge past the capacity timeout already has a delete in flight, the no-op
// teardown does not short-circuit the reconcile or re-stamp the backoff:
// abandonRebalanceSurge reports it did not start teardown, so the pass reports no
// change and reconcileRequestedDeletes can promote the recorded delete.
func TestReconcileControlPlaneRebalance_NoBackoffWhenAbandonDeleteInFlight(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	configID := uuid.New()
	cappedID := uuid.New()
	base := time.Now()
	primaryType := uuid.New()
	worseType := uuid.New()
	cluster := rebalanceCluster(clusterID, configID, []uuid.UUID{primaryType, worseType}, true, false)

	// Past the timeout, no instance allocated -- a capacity failure -- but a delete
	// is already in flight for the surge, so teardown is a no-op this pass.
	surge := healthyCPNode(clusterID, "cp-surge", base)
	surge.ControlPlaneConfigurationID = cappedID
	surge.Conditions.Joined = db.NewCondition(false, "Not joined")
	surge.Conditions.Pending = db.Condition{
		Status:             db.ConditionStatusTrue,
		Message:            "Pending",
		LastTransitionTime: time.Now().Add(-defaultRebalanceSurgeTimeout - time.Minute),
	}
	surge.Conditions.ControlPlanePreferredReplacement = db.NewCondition(true, "Rebalance surge")
	surge.Conditions.DeleteRequested = db.NewCondition(true, "Delete accepted")
	nodesList := []db.Node{
		cpNodeOnType(clusterID, configID, "cp-a", primaryType, base),
		cpNodeOnType(clusterID, configID, "cp-b", primaryType, base),
		cpNodeOnType(clusterID, configID, "cp-c", primaryType, base),
		surge,
	}

	mockTx := &testutil.MockTx{}
	r := newRebalanceReconciler(mockTx)
	cp := &controlPlane{nodes: nodesList, clusterControlPlaneConfig: cluster.ControlPlaneConfig}

	acted, err := r.reconcileControlPlaneRebalance(context.Background(), cluster, cp, domainAllocator{}, nil)
	require.NoError(t, err)
	assert.False(t, acted, "a no-op teardown does not own the reconcile, so requested deletes can still be promoted")

	mockTx.AssertNotCalled(t, "GetCluster", mock.Anything, mock.Anything)
	mockTx.AssertNotCalled(t, "UpdateCluster", mock.Anything, mock.Anything)
	mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
	mockTx.AssertNotCalled(t, "CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}
