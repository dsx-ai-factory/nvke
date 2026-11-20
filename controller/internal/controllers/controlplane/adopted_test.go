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
	node.Conditions.AdoptedSeed = db.NewCondition(true, "Adopted")
	return node
}

// TestAdoptedControlPlaneIsNeverReplaced pins every reconcile path that could
// build a provider control-plane node in place of an adopted one -- scale-up,
// rollout, rebalance and remediation -- as a no-op that writes nothing.
func TestAdoptedControlPlaneIsNeverReplaced(t *testing.T) {
	t.Parallel()
	clusterID, configID, oldConfigID := uuid.New(), uuid.New(), uuid.New()
	base := time.Now()
	preferred := uuid.New()

	cluster := rebalanceCluster(clusterID, configID, []uuid.UUID{preferred}, true, false)
	cluster.EnableCPNodeRecycling = true
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
	marked := adopted(configID)
	marked[0].Conditions.MarkedForRecycling = db.NewCondition(true, "NeedsRecycle")

	tests := []struct {
		name string
		run  func(r *reconciler) (bool, error)
	}{
		{
			name: "scale-up does not fill a desiredCount gap",
			run: func(r *reconciler) (bool, error) {
				short := cluster
				short.ControlPlaneConfig.DesiredCount = 5
				return false, r.reconcileControlPlaneScaleUp(context.Background(), short, controlPlaneOf(adopted(configID)), domainAllocator{}, newNodeProviderFor(t, cluster))
			},
		},
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
			name: "remediation leaves a marked adopted node for the operator",
			run: func(r *reconciler) (bool, error) {
				return r.remediateMachines(context.Background(), cluster, controlPlaneOf(marked))
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

func TestScaleDownEligibleSkipsAdoptedNodes(t *testing.T) {
	t.Parallel()
	clusterID, configID := uuid.New(), uuid.New()
	built := cpNodeOnType(clusterID, configID, "cp-built", uuid.New(), time.Now())
	cp := &controlPlane{nodes: []db.Node{adoptedCPNode(clusterID, configID, "cp-adopted", time.Now()), built}}

	eligible := cp.scaleDownEligible()

	require.Len(t, eligible, 1)
	assert.Equal(t, built.ID, eligible[0].ID)
	assert.True(t, cp.adopted())
	assert.False(t, (&controlPlane{nodes: []db.Node{built}}).adopted())
}
