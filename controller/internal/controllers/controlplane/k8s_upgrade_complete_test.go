// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// Right after a pool's version changes, the NodeSet controller has not yet made
// a new current NodeSet, so nothing counts as deprecated and every node is
// healthy: the old check read that as "upgrade complete" and started the
// add-ons upgrade on a node still running the previous kubeadm.
func TestIsControlPlaneUpgradeComplete_NodesStillOnOldVersion(t *testing.T) {
	healthy := db.Node{Conditions: db.NodeConditions{
		KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
		Provisioned:     db.Condition{Status: db.ConditionStatusTrue},
	}}
	nodeSetID := uuid.New()
	pool := func(version string) *controlPlane {
		return &controlPlane{
			nodes: []db.Node{healthy, healthy, healthy},
			nodePools: []nodePool{{
				NodePool: db.NodePool{DesiredCount: 3, CurrentNodeSetID: &nodeSetID},
				nodeSets: []nodeSet{{
					NodeSet: db.NodeSet{ID: nodeSetID, KubernetesVersion: version},
					nodes:   []db.Node{healthy, healthy, healthy},
				}},
			}},
		}
	}
	cluster := db.Cluster{KubernetesVersion: "1.35.3"}
	r := &reconciler{}

	assert.False(t, r.isControlPlaneUpgradeComplete(cluster, pool("1.34.2"), slog.Default()), "nodes on the old version")
	assert.True(t, r.isControlPlaneUpgradeComplete(cluster, pool("1.35.3"), slog.Default()), "nodes on the target version")
	assert.True(t, r.isControlPlaneUpgradeComplete(cluster, pool(""), slog.Default()), "unknown NodeSet version does not block")
}
