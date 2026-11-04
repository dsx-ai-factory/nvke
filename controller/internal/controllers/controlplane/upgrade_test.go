// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/nodes"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestIsControlPlaneHealthy_WithSurgeThreshold(t *testing.T) {
	t.Parallel()

	currentSpec := nicoCPSpec("nvke-1.34.2", "", "1.34.2")
	currentID, staleID := uuid.New(), uuid.New()
	configs := map[uuid.UUID]db.ControlPlaneConfigSpec{currentID: currentSpec, staleID: staleCPSpec(t, currentSpec)}

	tests := []struct {
		name     string
		maxSurge int
		nodes    []db.Node
		want     bool
	}{
		{
			name:     "rolling with surge: three healthy is below desired plus surge",
			maxSurge: 1,
			nodes: []db.Node{
				makeCPControllerNode("cp-1", currentID),
				makeCPControllerNode("cp-2", currentID),
				makeCPControllerNode("cp-3", staleID),
			},
			want: false,
		},
		{
			name:     "rolling with surge: four healthy meets desired plus surge",
			maxSurge: 1,
			nodes: []db.Node{
				makeCPControllerNode("cp-1", currentID),
				makeCPControllerNode("cp-2", currentID),
				makeCPControllerNode("cp-3", currentID),
				makeCPControllerNode("cp-4", staleID),
			},
			want: true,
		},
		{
			name:     "no rollout: three healthy meets desired",
			maxSurge: 1,
			nodes: []db.Node{
				makeCPControllerNode("cp-1", currentID),
				makeCPControllerNode("cp-2", currentID),
				makeCPControllerNode("cp-3", currentID),
			},
			want: true,
		},
		{
			name:     "single healthy node is never sufficient",
			maxSurge: 0,
			nodes:    []db.Node{makeCPControllerNode("cp-1", currentID)},
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cluster := clusterWithCurrentSnapshot(currentSpec, tt.maxSurge)
			mockTx := &testutil.MockTx{}
			mockTx.On("ListNodes", mock.Anything, mock.MatchedBy(func(f db.NodeFilters) bool {
				return f.ClusterID != nil && *f.ClusterID == cluster.ID && f.ControlPlane != nil && *f.ControlPlane
			})).Return(tt.nodes, nil)

			r := &reconciler{db: mockTx, logger: slog.Default()}
			cp := &controlPlane{nodes: tt.nodes, nodePinnedControlPlaneConfigs: configs, clusterControlPlaneConfig: cluster.ControlPlaneConfig}
			got, err := r.isControlPlaneHealthy(context.Background(), cluster, cp, slog.Default())
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			mockTx.AssertExpectations(t)
		})
	}
}

func TestReconcileNodesNeedingRolloutWithoutControlPlaneVIP(t *testing.T) {
	t.Parallel()
	for _, allow := range []bool{false, true} {
		t.Run(fmt.Sprintf("override=%t", allow), func(t *testing.T) {
			t.Parallel()
			clusterID := uuid.New()
			oldConfigID := uuid.New()
			desiredSpec := db.ControlPlaneConfigSpec{KubernetesVersion: "1.31.0"}
			desiredSpec.SetNico(db.NicoControlPlaneProvider{Release: "new-os"})
			pinnedSpec := db.ControlPlaneConfigSpec{KubernetesVersion: "1.31.0"}
			pinnedSpec.SetNico(db.NicoControlPlaneProvider{Release: "old-os"})
			cluster := db.Cluster{
				ID:                    clusterID,
				Name:                  "single-no-vip",
				EnableCPNodeRecycling: true,
				KubernetesVersion:     "1.31.0",
				ControlPlaneConfig: db.ControlPlaneConfig{
					ControlPlaneConfigSpec: desiredSpec,
					DesiredCount:           1,
					MaxSurge:               1,
				},
			}
			node := db.Node{
				ID:                          uuid.New(),
				ClusterID:                   clusterID,
				ControlPlane:                true,
				ControlPlaneConfigurationID: oldConfigID,
				Name:                        "cp-0",
			}
			cp := &controlPlane{
				nodes:                     []db.Node{node},
				clusterControlPlaneConfig: cluster.ControlPlaneConfig,
				nodePinnedControlPlaneConfigs: map[uuid.UUID]db.ControlPlaneConfigSpec{
					oldConfigID: pinnedSpec,
				},
			}
			mockTx := &testutil.MockTx{}
			surgeError := errors.New("surge database failure")
			if allow {
				mockTx.On("ListClusterNodeNames", mock.Anything, clusterID).Return([]string{}, surgeError).Once()
			}
			r := &reconciler{cfg: config.Config{AllowControlPlaneWithoutVIP: allow}, db: mockTx, logger: slog.Default()}

			_, err := r.reconcileNodesNeedingRollout(context.Background(), cluster, cp, domainAllocator{}, nil)

			if allow {
				require.ErrorIs(t, err, surgeError)
			} else {
				require.NoError(t, err)
			}
			mockTx.AssertExpectations(t)
			mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
			mockTx.AssertNotCalled(t, "CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
		})
	}
}

func TestReconcileNodesNeedingRolloutRecordsIntentAfterSurgeExists(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	oldConfigID := uuid.New()
	currentConfigID := uuid.New()
	desiredSpec := db.ControlPlaneConfigSpec{KubernetesVersion: "1.31.0"}
	desiredSpec.SetNico(db.NicoControlPlaneProvider{Release: "new-os"})
	pinnedSpec := db.ControlPlaneConfigSpec{KubernetesVersion: "1.31.0"}
	pinnedSpec.SetNico(db.NicoControlPlaneProvider{Release: "old-os"})
	cluster := db.Cluster{
		ID:                    clusterID,
		Name:                  "single-no-vip",
		EnableCPNodeRecycling: true,
		ControlPlaneConfig: db.ControlPlaneConfig{
			ControlPlaneConfigSpec: desiredSpec,
			DesiredCount:           1,
			MaxSurge:               1,
		},
		Conditions: db.ClusterConditions{
			CPNodesRecycling: db.Condition{Status: db.ConditionStatusTrue},
		},
	}
	oldNode := makeHealthyNode("cp-old", uuid.Nil)
	oldNode.ID = uuid.New()
	oldNode.ClusterID = clusterID
	oldNode.ControlPlane = true
	oldNode.ControlPlaneConfigurationID = oldConfigID
	newNode := makeHealthyNode("cp-new", uuid.Nil)
	newNode.ID = uuid.New()
	newNode.ClusterID = clusterID
	newNode.ControlPlane = true
	newNode.ControlPlaneConfigurationID = currentConfigID
	cp := &controlPlane{
		nodes:                     []db.Node{oldNode, newNode},
		clusterControlPlaneConfig: cluster.ControlPlaneConfig,
		nodePinnedControlPlaneConfigs: map[uuid.UUID]db.ControlPlaneConfigSpec{
			oldConfigID:     pinnedSpec,
			currentConfigID: desiredSpec,
		},
	}
	mockTx := &testutil.MockTx{}
	cpTrue := true
	mockTx.On("ListNodes", mock.Anything, db.NodeFilters{
		ClusterID:    &clusterID,
		ControlPlane: &cpTrue,
	}).Return([]db.Node{oldNode, newNode}, nil).Once()
	// The rollout now records delete intent through nodes.DeleteWithTx (the same
	// path as a user delete and remediation) rather than tearing the node down
	// inline, so it reads the node, locks the cluster for the mutation fence, and
	// flips DeleteRequested -- it does not set Deleting or queue the teardown.
	mockTx.On("GetNode", mock.Anything, oldNode.ID).Return(oldNode, nil)
	mockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(cluster, nil)
	mockTx.On("GetNodeForUpdate", mock.Anything, oldNode.ID).Return(oldNode, nil)
	var written db.Node
	mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(node *db.Node) bool {
		return node.ID == oldNode.ID && nodes.IsDeleteRequested(*node)
	})).Run(func(args mock.Arguments) {
		written = *args.Get(1).(*db.Node)
	}).Return(oldNode, nil)
	r := &reconciler{cfg: config.Config{}, db: mockTx, logger: slog.Default()}

	recorded, err := r.reconcileNodesNeedingRollout(context.Background(), cluster, cp, domainAllocator{}, nil)

	require.NoError(t, err)
	assert.True(t, recorded, "reconcileNodesNeedingRollout should report that it recorded a delete")
	assert.True(t, nodes.IsDeleteRequested(written), "rollout should record delete intent")
	assert.False(t, nodes.IsDeleting(written), "the teardown must wait for the serialized delete loop")
	mockTx.AssertNotCalled(t, "CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	mockTx.AssertExpectations(t)
}

// TestReconcileNodesNeedingRolloutSkipsWhenReplacementNotJoined pins the
// joined-survivor precondition: the surge-inclusive health check proves the
// replacement is Ready, but until it has Joined etcd, removing the old node would
// drop the joined members below quorum -- and this path skips the live-delete
// guard, so nothing else catches it. The rollout must record nothing and wait for
// the replacement to join.
func TestReconcileNodesNeedingRolloutSkipsWhenReplacementNotJoined(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	oldConfigID := uuid.New()
	currentConfigID := uuid.New()
	desiredSpec := db.ControlPlaneConfigSpec{KubernetesVersion: "1.31.0"}
	desiredSpec.SetNico(db.NicoControlPlaneProvider{Release: "new-os"})
	pinnedSpec := db.ControlPlaneConfigSpec{KubernetesVersion: "1.31.0"}
	pinnedSpec.SetNico(db.NicoControlPlaneProvider{Release: "old-os"})
	cluster := db.Cluster{
		ID:                    clusterID,
		Name:                  "single-no-vip",
		EnableCPNodeRecycling: true,
		ControlPlaneConfig: db.ControlPlaneConfig{
			ControlPlaneConfigSpec: desiredSpec,
			DesiredCount:           1,
			MaxSurge:               1,
		},
		Conditions: db.ClusterConditions{
			CPNodesRecycling: db.Condition{Status: db.ConditionStatusTrue},
		},
	}
	oldNode := makeHealthyNode("cp-old", uuid.Nil)
	oldNode.ID = uuid.New()
	oldNode.ClusterID = clusterID
	oldNode.ControlPlane = true
	oldNode.ControlPlaneConfigurationID = oldConfigID
	newNode := makeHealthyNode("cp-new", uuid.Nil)
	newNode.ID = uuid.New()
	newNode.ClusterID = clusterID
	newNode.ControlPlane = true
	newNode.ControlPlaneConfigurationID = currentConfigID
	// The replacement is Ready (so the surge-inclusive health check passes) but has
	// not joined etcd yet, so it is not a live member that can keep quorum.
	newNode.Conditions.Joined = db.Condition{Status: db.ConditionStatusFalse}
	cp := &controlPlane{
		nodes:                     []db.Node{oldNode, newNode},
		clusterControlPlaneConfig: cluster.ControlPlaneConfig,
		nodePinnedControlPlaneConfigs: map[uuid.UUID]db.ControlPlaneConfigSpec{
			oldConfigID:     pinnedSpec,
			currentConfigID: desiredSpec,
		},
	}
	mockTx := &testutil.MockTx{}
	cpTrue := true
	mockTx.On("ListNodes", mock.Anything, db.NodeFilters{
		ClusterID:    &clusterID,
		ControlPlane: &cpTrue,
	}).Return([]db.Node{oldNode, newNode}, nil)
	r := &reconciler{cfg: config.Config{}, db: mockTx, logger: slog.Default()}

	recorded, err := r.reconcileNodesNeedingRollout(context.Background(), cluster, cp, domainAllocator{}, nil)

	require.NoError(t, err)
	assert.False(t, recorded, "reconcileNodesNeedingRollout must not report a recorded delete when the replacement has not joined")
	mockTx.AssertNotCalled(t, "GetClusterForUpdate", mock.Anything, mock.Anything)
	mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
	mockTx.AssertNotCalled(t, "CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// TestReconcileNodesNeedingRolloutSkipsWhenDeleteInFlight pins the serialization:
// when any control-plane node already has an accepted delete -- queued behind the
// serialized loop or already tearing down, as a concurrent remediation or user
// delete would leave it -- the rollout records nothing and waits, so two
// teardowns never overlap on one cluster.
func TestReconcileNodesNeedingRolloutSkipsWhenDeleteInFlight(t *testing.T) {
	t.Parallel()

	currentSpec := nicoCPSpec("nvke-1.34.2", "", "1.34.2")
	currentID, staleID := uuid.New(), uuid.New()
	configs := map[uuid.UUID]db.ControlPlaneConfigSpec{currentID: currentSpec, staleID: staleCPSpec(t, currentSpec)}

	tests := []struct {
		name         string
		markInFlight func(*db.Node)
	}{
		{
			name:         "another node already tearing down",
			markInFlight: func(n *db.Node) { n.Conditions.Deleting = db.NewCondition(true, "Node deletion in progress") },
		},
		{
			name:         "another node delete queued behind the serialized loop",
			markInFlight: func(n *db.Node) { n.Conditions.DeleteRequested = db.NewCondition(true, "Delete accepted") },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			clusterID := uuid.New()
			cluster := clusterWithCurrentSnapshot(currentSpec, 0)
			cluster.ID = clusterID
			cluster.EnableCPNodeRecycling = true
			cluster.Conditions.CPNodesRecycling = db.NewCondition(true, "Control plane nodes recycling in progress")

			cp1 := makeCPControllerNode("cp-1", currentID)
			cp2 := makeCPControllerNode("cp-2", currentID)
			cp3 := makeCPControllerNode("cp-3", currentID)
			stale := makeCPControllerNode("cp-stale", staleID)
			inFlight := makeCPControllerNode("cp-inflight", currentID)
			tt.markInFlight(&inFlight)
			for _, n := range []*db.Node{&cp1, &cp2, &cp3, &stale, &inFlight} {
				n.ClusterID = clusterID
			}
			allNodes := []db.Node{cp1, cp2, cp3, stale, inFlight}

			mockTx := &testutil.MockTx{}
			cpTrue := true
			mockTx.On("ListNodes", mock.Anything, db.NodeFilters{
				ClusterID:    &clusterID,
				ControlPlane: &cpTrue,
			}).Return(allNodes, nil)

			r := &reconciler{cfg: config.Config{}, db: mockTx, logger: slog.Default()}
			cp := &controlPlane{
				nodes:                         allNodes,
				clusterControlPlaneConfig:     cluster.ControlPlaneConfig,
				nodePinnedControlPlaneConfigs: configs,
			}

			recorded, err := r.reconcileNodesNeedingRollout(context.Background(), cluster, cp, domainAllocator{}, nil)

			require.NoError(t, err)
			assert.False(t, recorded, "reconcileNodesNeedingRollout must not report a recorded delete when a delete is already in flight")
			mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
			mockTx.AssertNotCalled(t, "CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
		})
	}
}
