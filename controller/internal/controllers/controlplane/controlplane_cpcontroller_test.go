// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/testutil"
)

// cpControllerReconciler returns a reconciler with the control-plane-controller
// feature flag ON. The snapshot-based helpers are pure and read from cp.nodePinnedControlPlaneConfigs,
// so no DB is needed.
func cpControllerReconciler() *reconciler {
	return &reconciler{
		cfg:    config.Config{ProvisionControlPlaneWithControlPlaneController: true},
		logger: slog.New(slog.DiscardHandler),
	}
}

// makeCPControllerNode builds a healthy no-nodepool control-plane node pinned to
// the given configuration snapshot.
func makeCPControllerNode(name string, configID uuid.UUID) db.Node {
	return db.Node{
		ID:                          uuid.New(),
		Name:                        name,
		ControlPlane:                true,
		NodePoolID:                  uuid.Nil,
		ControlPlaneConfigurationID: configID,
		Conditions: db.NodeConditions{
			Pending:         db.Condition{Status: db.ConditionStatusFalse},
			KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
			Provisioned:     db.Condition{Status: db.ConditionStatusTrue},
			NodeAgentReady:  db.Condition{Status: db.ConditionStatusTrue},
			Deleting:        db.Condition{Status: db.ConditionStatusFalse},
		},
	}
}

// nicoCPSpec builds the control-plane snapshot a stored Nico cluster carries.
// The os and config profiles live in the nested nico payload, so a spec written
// as a bare literal with root-level profiles would compare as having none.
func nicoCPSpec(osProfile, configProfile, kubernetesVersion string) db.ControlPlaneConfigSpec {
	spec := db.ControlPlaneConfigSpec{KubernetesVersion: kubernetesVersion}
	spec.SetNico(db.NicoControlPlaneProvider{OSProfile: osProfile, ConfigProfile: configProfile})
	return spec
}

// staleCPSpec returns spec carrying an older OS profile, which is what makes a
// node's pinned snapshot stale relative to the cluster's current one.
func staleCPSpec(t *testing.T, spec db.ControlPlaneConfigSpec) db.ControlPlaneConfigSpec {
	t.Helper()
	nico, err := spec.NicoProvider()
	require.NoError(t, err)
	nico.OSProfile = "nke-1.33.6"
	spec.SetNico(nico)
	return spec
}

func clusterWithCurrentSnapshot(spec db.ControlPlaneConfigSpec, maxSurge int) db.Cluster {
	return db.Cluster{
		ID:                uuid.New(),
		KubernetesVersion: spec.KubernetesVersion,
		ControlPlaneConfig: db.ControlPlaneConfig{
			ConfigurationID:        uuid.New(),
			DesiredCount:           3,
			MaxSurge:               maxSurge,
			ControlPlaneConfigSpec: spec,
		},
	}
}

func TestReadControlPlane_PreloadsControlPlaneConfigs(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	configA := uuid.New()
	configB := uuid.New()
	specA := nicoCPSpec("nke-1.34.2", "", "1.34.2")
	specB := nicoCPSpec("nke-1.33.6", "", "1.33.6")

	// Two nodes share configA (dedup expected), one uses configB.
	nodes := []db.Node{
		makeCPControllerNode("cp-1", configA),
		makeCPControllerNode("cp-2", configA),
		makeCPControllerNode("cp-3", configB),
	}

	mockTx := &testutil.MockTx{}
	mockTx.On("ListNodes", mock.Anything, mock.MatchedBy(func(f db.NodeFilters) bool {
		return f.ClusterID != nil && *f.ClusterID == clusterID && f.ControlPlane != nil && *f.ControlPlane
	})).Return(nodes, nil).Once()
	mockTx.On("ListNodePools", mock.Anything, mock.Anything).Return([]db.NodePool{}, nil).Once()
	// Exactly one batch read, with the distinct (deduped) configuration IDs.
	mockTx.On("ListControlPlaneConfigurationsByIDs", mock.Anything, mock.MatchedBy(func(ids []uuid.UUID) bool {
		return len(ids) == 2 &&
			(ids[0] == configA || ids[1] == configA) &&
			(ids[0] == configB || ids[1] == configB)
	})).Return([]db.ControlPlaneConfiguration{
		{ID: configA, Config: specA},
		{ID: configB, Config: specB},
	}, nil).Once()

	cp, err := readControlPlane(context.Background(), mockTx, db.Cluster{ID: clusterID}, true)
	require.NoError(t, err)
	require.Len(t, cp.nodePinnedControlPlaneConfigs, 2)
	assert.Equal(t, specA, cp.nodePinnedControlPlaneConfigs[configA])
	assert.Equal(t, specB, cp.nodePinnedControlPlaneConfigs[configB])
	assert.True(t, cp.useController)
	mockTx.AssertExpectations(t)
}

func TestReadControlPlane_SkipsConfigLoadWithoutCPControllerNodes(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	// A node-pool-managed node (NodePoolID set) is not CP-controller managed.
	poolNode := db.Node{ID: uuid.New(), ClusterID: clusterID, ControlPlane: true, NodePoolID: uuid.New()}

	mockTx := &testutil.MockTx{}
	mockTx.On("ListNodes", mock.Anything, mock.Anything).Return([]db.Node{poolNode}, nil).Once()
	mockTx.On("ListNodePools", mock.Anything, mock.Anything).Return([]db.NodePool{}, nil).Once()

	cp, err := readControlPlane(context.Background(), mockTx, db.Cluster{ID: clusterID}, false)
	require.NoError(t, err)
	assert.Empty(t, cp.nodePinnedControlPlaneConfigs)
	mockTx.AssertNotCalled(t, "ListControlPlaneConfigurationsByIDs", mock.Anything, mock.Anything)
	mockTx.AssertExpectations(t)
}

func TestIsK8sVersionUpgradeNeeded_CPController(t *testing.T) {
	t.Parallel()

	oldConfigID := uuid.New()
	currentConfigID := uuid.New()

	currentSpec := nicoCPSpec("nke-1.34.2", "cfg", "1.34.2")
	oldSpec := nicoCPSpec("nke-1.33.6", "cfg", "1.33.6")
	configs := map[uuid.UUID]db.ControlPlaneConfigSpec{oldConfigID: oldSpec, currentConfigID: currentSpec}

	tests := []struct {
		name       string
		nodeConfig uuid.UUID
		want       bool
	}{
		{name: "node pinned to older k8s version needs upgrade", nodeConfig: oldConfigID, want: true},
		{name: "node pinned to current k8s version does not", nodeConfig: currentConfigID, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := cpControllerReconciler()
			cluster := clusterWithCurrentSnapshot(currentSpec, 0)
			cp := &controlPlane{
				nodes:                         []db.Node{makeCPControllerNode("cp-1", tt.nodeConfig)},
				nodePinnedControlPlaneConfigs: configs,
			}

			assert.Equal(t, tt.want, r.isK8sVersionUpgradeNeeded(cluster, cp, slog.New(slog.DiscardHandler)))
		})
	}
}

func TestNodesNeedingRollout_TemplateVsLabelOnly(t *testing.T) {
	t.Parallel()

	currentSpec := nicoCPSpec("nke-1.34.2", "cfg", "1.34.2")
	// Same replacing template as current, but a non-replacing (label-only) delta.
	labelOnlySpec := currentSpec
	labelOnlySpec.NodeLabels = map[string]string{"role": "cp"}
	// Replacing delta: different OS profile.
	replacingSpec := staleCPSpec(t, currentSpec)

	replacingID := uuid.New()
	currentID := uuid.New()
	labelOnlyID := uuid.New()

	cp := &controlPlane{
		nodes: []db.Node{
			makeCPControllerNode("cp-replacing", replacingID),
			makeCPControllerNode("cp-current", currentID),
			makeCPControllerNode("cp-label-only", labelOnlyID),
		},
		nodePinnedControlPlaneConfigs: map[uuid.UUID]db.ControlPlaneConfigSpec{
			replacingID: replacingSpec,
			currentID:   currentSpec,
			labelOnlyID: labelOnlySpec,
		},
	}

	nodesNeedingRollout := cp.nodesNeedingRollout(currentSpec)
	require.Len(t, nodesNeedingRollout, 1)
	assert.Equal(t, "cp-replacing", nodesNeedingRollout[0].Name, "only the replacing-template node should need rollout")
}

func TestNodesNeedingRollout_IncludesInFlightSkipsMissingSnapshot(t *testing.T) {
	t.Parallel()

	currentSpec := nicoCPSpec("nke-1.34.2", "", "1.34.2")
	replacingSpec := staleCPSpec(t, currentSpec)
	replacingID := uuid.New()

	pendingNode := makeCPControllerNode("cp-pending", replacingID)
	pendingNode.Conditions.Pending = db.Condition{Status: db.ConditionStatusTrue}
	deletingNode := makeCPControllerNode("cp-deleting", replacingID)
	deletingNode.Conditions.Deleting = db.Condition{Status: db.ConditionStatusTrue}
	activeNode := makeCPControllerNode("cp-active", replacingID)
	// Node whose pinned snapshot is not preloaded (e.g. mid-pass surge node) is skipped.
	missingSnapshotNode := makeCPControllerNode("cp-missing", uuid.New())

	cp := &controlPlane{
		nodes:                         []db.Node{pendingNode, deletingNode, activeNode, missingSnapshotNode},
		nodePinnedControlPlaneConfigs: map[uuid.UUID]db.ControlPlaneConfigSpec{replacingID: replacingSpec},
	}

	// In-flight (Pending/Deleting) nodes are NOT filtered, matching the node-pool
	// path; only the node with a snapshot absent from the preloaded map is skipped.
	nodesNeedingRollout := cp.nodesNeedingRollout(currentSpec)
	require.Len(t, nodesNeedingRollout, 3)
	names := []string{nodesNeedingRollout[0].Name, nodesNeedingRollout[1].Name, nodesNeedingRollout[2].Name}
	assert.ElementsMatch(t, []string{"cp-pending", "cp-deleting", "cp-active"}, names)
}

func TestIsControlPlaneUpgradeComplete_CPController(t *testing.T) {
	t.Parallel()

	currentSpec := nicoCPSpec("nke-1.34.2", "", "1.34.2")
	replacingSpec := staleCPSpec(t, currentSpec)
	replacingID := uuid.New()
	currentID := uuid.New()
	configs := map[uuid.UUID]db.ControlPlaneConfigSpec{replacingID: replacingSpec, currentID: currentSpec}

	tests := []struct {
		name      string
		recycling bool
		nodes     []db.Node
		want      bool
	}{
		{
			name: "not complete while deprecated nodes remain",
			nodes: []db.Node{
				makeCPControllerNode("cp-1", currentID),
				makeCPControllerNode("cp-2", currentID),
				makeCPControllerNode("cp-3", replacingID),
			},
			want: false,
		},
		{
			name: "not complete while a surge node is still present",
			nodes: []db.Node{
				makeCPControllerNode("cp-1", currentID),
				makeCPControllerNode("cp-2", currentID),
				makeCPControllerNode("cp-3", currentID),
				makeCPControllerNode("cp-surge", currentID),
			},
			want: false, // 4 healthy != desired 3
		},
		{
			name:      "not complete while recycling condition set",
			recycling: true,
			nodes: []db.Node{
				makeCPControllerNode("cp-1", currentID),
				makeCPControllerNode("cp-2", currentID),
				makeCPControllerNode("cp-3", currentID),
			},
			want: false,
		},
		{
			name: "complete when no deprecated nodes and healthy equals desired",
			nodes: []db.Node{
				makeCPControllerNode("cp-1", currentID),
				makeCPControllerNode("cp-2", currentID),
				makeCPControllerNode("cp-3", currentID),
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := cpControllerReconciler()
			cluster := clusterWithCurrentSnapshot(currentSpec, 0)
			if tt.recycling {
				cluster.Conditions.CPNodesRecycling = db.NewCondition(true, "recycling")
			}
			cp := &controlPlane{nodes: tt.nodes, nodePinnedControlPlaneConfigs: configs, clusterControlPlaneConfig: cluster.ControlPlaneConfig, useController: true}

			assert.Equal(t, tt.want, r.isControlPlaneUpgradeComplete(cluster, cp, slog.New(slog.DiscardHandler)))
		})
	}
}

func TestExpectedControlPlaneCount_CPController(t *testing.T) {
	t.Parallel()

	currentSpec := nicoCPSpec("nke-1.34.2", "", "1.34.2")
	replacingSpec := staleCPSpec(t, currentSpec)
	replacingID := uuid.New()
	currentID := uuid.New()
	configs := map[uuid.UUID]db.ControlPlaneConfigSpec{replacingID: replacingSpec, currentID: currentSpec}

	t.Run("no surge configured -> desired count", func(t *testing.T) {
		t.Parallel()
		cp := &controlPlane{
			nodes: []db.Node{
				makeCPControllerNode("cp-1", replacingID),
				makeCPControllerNode("cp-2", replacingID),
				makeCPControllerNode("cp-3", replacingID),
			},
			nodePinnedControlPlaneConfigs: configs,
			clusterControlPlaneConfig:     clusterWithCurrentSnapshot(currentSpec, 0).ControlPlaneConfig,
			useController:                 true,
		}

		assert.Equal(t, 3, cp.expectedCount())
	})

	t.Run("surge configured with deprecated nodes -> desired + 1", func(t *testing.T) {
		t.Parallel()
		cp := &controlPlane{
			nodes: []db.Node{
				makeCPControllerNode("cp-1", replacingID),
				makeCPControllerNode("cp-2", currentID),
				makeCPControllerNode("cp-3", currentID),
			},
			nodePinnedControlPlaneConfigs: configs,
			clusterControlPlaneConfig:     clusterWithCurrentSnapshot(currentSpec, 1).ControlPlaneConfig,
			useController:                 true,
		}

		assert.Equal(t, 4, cp.expectedCount(), "desired 3 + min(maxSurge 1, deprecated 1)")
	})
}

func TestCanStartRemediation_CPControllerWaitsForReplacement(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.DiscardHandler)
	nodeToRemove := db.Node{
		ID:           uuid.New(),
		Name:         "cp-recycle",
		ControlPlane: true,
		Conditions: db.NodeConditions{
			Pending:            db.Condition{Status: db.ConditionStatusFalse},
			KubernetesReady:    db.Condition{Status: db.ConditionStatusTrue},
			Provisioned:        db.Condition{Status: db.ConditionStatusTrue},
			NodeAgentReady:     db.Condition{Status: db.ConditionStatusTrue},
			MarkedForRecycling: db.Condition{Status: db.ConditionStatusTrue},
			Deleting:           db.Condition{Status: db.ConditionStatusFalse},
		},
	}
	healthy := func(name string) db.Node { return makeCPControllerNode(name, uuid.New()) }
	// expectedCount() resolves to DesiredCount (3) since no node is deprecated.
	desired3 := db.ControlPlaneConfig{DesiredCount: 3}

	// desired=3 with only 2 nodes present: replacement not yet created -> wait.
	cpWaiting := &controlPlane{nodes: []db.Node{nodeToRemove, healthy("cp-2")}, clusterControlPlaneConfig: desired3, useController: true}
	assert.False(t, canStartRemediation(log, cpWaiting, nodeToRemove))

	// desired=3 with 3 nodes present: replacement created -> can proceed.
	cpReady := &controlPlane{nodes: []db.Node{nodeToRemove, healthy("cp-2"), healthy("cp-3")}, clusterControlPlaneConfig: desired3, useController: true}
	assert.True(t, canStartRemediation(log, cpReady, nodeToRemove))
}
