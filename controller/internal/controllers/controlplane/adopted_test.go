// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

// adoptedCPNode is a healthy adopted control-plane node: no provider payload,
// so ScaleDownRank ranks it below every preferred instance type.
func adoptedCPNode(clusterID, configID uuid.UUID, name string, createdAt time.Time) db.Node {
	node := healthyCPNode(clusterID, name, createdAt)
	node.ControlPlaneConfigurationID = configID
	node.Conditions.Adopted = db.NewCondition(true, "Adopted")
	return node
}

// TestAdoptedControlPlaneIsNeverReplaced pins the settings POST /clusters/adopt
// records -- recycling, rebalancing and automatic scale-down off -- as keeping
// every reconcile path that could replace or shed a control-plane node from
// acting on an adopted control plane, with no adopted-specific check.
func TestAdoptedControlPlaneIsNeverReplaced(t *testing.T) {
	t.Parallel()
	clusterID, configID, oldConfigID := uuid.New(), uuid.New(), uuid.New()
	base := time.Now()

	cluster := rebalanceCluster(clusterID, configID, []uuid.UUID{uuid.New()}, false, true)
	cluster.EnableCPNodeRecycling = false
	cluster.ControlPlaneConfig.MaxSurge = 1
	cluster.ControlPlaneConfig.ControlPlaneConfigSpec.KubernetesVersion = "1.36.6"
	cluster.KubernetesVersion = "1.36.6"
	pinned := cluster.ControlPlaneConfig.ControlPlaneConfigSpec
	pinned.KubernetesVersion = "1.36.4"

	adopted := func(configID uuid.UUID) []db.Node {
		return []db.Node{
			adoptedCPNode(clusterID, configID, "cp-a", base),
			adoptedCPNode(clusterID, configID, "cp-b", base),
			adoptedCPNode(clusterID, configID, "cp-c", base),
		}
	}
	controlPlaneOf := func(nodes []db.Node) *controlPlane {
		return &controlPlane{
			nodes:                         nodes,
			clusterControlPlaneConfig:     cluster.ControlPlaneConfig,
			nodePinnedControlPlaneConfigs: map[uuid.UUID]db.ControlPlaneConfigSpec{configID: cluster.ControlPlaneConfig.ControlPlaneConfigSpec, oldConfigID: pinned},
		}
	}

	tests := []struct {
		name string
		run  func(r *reconciler) (bool, error)
	}{
		{
			name: "rollout neither surges nor deletes drifted adopted nodes",
			run: func(r *reconciler) (bool, error) {
				cp := controlPlaneOf(adopted(oldConfigID))
				require.Len(t, cp.nodesNeedingRollout(cluster.ControlPlaneConfig.ControlPlaneConfigSpec), 3, "drifted adopted nodes still count as needing rollout")
				return r.reconcileNodesNeedingRollout(context.Background(), cluster, cp, domainAllocator{}, newNodeProviderFor(t, cluster))
			},
		},
		{
			name: "rebalance does not surge past adopted nodes on no instance type",
			run: func(r *reconciler) (bool, error) {
				return r.reconcileControlPlaneRebalance(context.Background(), cluster, controlPlaneOf(adopted(configID)), domainAllocator{}, newNodeProviderFor(t, cluster))
			},
		},
		{
			name: "scale-down does not shed an adopted node above desiredCount",
			run: func(r *reconciler) (bool, error) {
				shrunk := cluster
				shrunk.ControlPlaneConfig.DesiredCount = 2
				return false, r.reconcileControlPlaneScaleDown(context.Background(), shrunk, controlPlaneOf(adopted(configID)))
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mockTx := &testutil.MockTx{}
			acted, err := tc.run(newRebalanceReconciler(mockTx))
			require.NoError(t, err)
			assert.False(t, acted)
			mockTx.AssertNotCalled(t, "ListClusterNodeNames", mock.Anything, mock.Anything)
			mockTx.AssertNotCalled(t, "CreateNode", mock.Anything, mock.Anything)
			mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
			mockTx.AssertNotCalled(t, "GetNodeForUpdate", mock.Anything, mock.Anything)
		})
	}
}
