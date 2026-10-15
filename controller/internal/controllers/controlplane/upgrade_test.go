// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
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

func TestReconcileNodesNeedingRolloutWithoutControlPlaneVIPDoesNotReplaceNodes(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	oldConfigID := uuid.New()
	desiredSpec := db.ControlPlaneConfigSpec{KubernetesVersion: "1.31.0"}
	desiredSpec.SetNico(db.NicoControlPlaneProvider{OSProfile: "new-os"})
	pinnedSpec := db.ControlPlaneConfigSpec{KubernetesVersion: "1.31.0"}
	pinnedSpec.SetNico(db.NicoControlPlaneProvider{OSProfile: "old-os"})
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
	r := &reconciler{cfg: config.Config{}, db: mockTx, logger: slog.Default()}

	err := r.reconcileNodesNeedingRollout(context.Background(), cluster, cp, domainAllocator{}, nil)

	require.NoError(t, err)
	mockTx.AssertExpectations(t)
	mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
	mockTx.AssertNotCalled(t, "CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestReconcileNodesNeedingRolloutWithoutControlPlaneVIPDeletesAfterSurgeExists(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	oldConfigID := uuid.New()
	currentConfigID := uuid.New()
	desiredSpec := db.ControlPlaneConfigSpec{KubernetesVersion: "1.31.0"}
	desiredSpec.SetNico(db.NicoControlPlaneProvider{OSProfile: "new-os"})
	pinnedSpec := db.ControlPlaneConfigSpec{KubernetesVersion: "1.31.0"}
	pinnedSpec.SetNico(db.NicoControlPlaneProvider{OSProfile: "old-os"})
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
	mockTx.On("GetNodeForUpdate", mock.Anything, oldNode.ID).Return(oldNode, nil)
	mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(node *db.Node) bool {
		return node.ID == oldNode.ID && node.Conditions.Deleting.Status == db.ConditionStatusTrue
	})).Return(oldNode, nil)
	mockTx.On("CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(db.WorkflowOutbox{}, nil)
	r := &reconciler{cfg: config.Config{}, db: mockTx, logger: slog.Default()}

	err := r.reconcileNodesNeedingRollout(context.Background(), cluster, cp, domainAllocator{}, nil)

	require.NoError(t, err)
	mockTx.AssertExpectations(t)
}
