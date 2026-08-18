// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cwe_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/rollout"
	"github.com/NVIDIA/nke/controller/internal/rollout/adapters/cwe"
)

func TestNodeAdapter_RoleFromControlPlaneFlag(t *testing.T) {
	cp := cwe.NewNode(db.Node{ID: uuid.New(), Name: "cp-0", ControlPlane: true}, "15005")
	worker := cwe.NewNode(db.Node{ID: uuid.New(), Name: "worker-0", ControlPlane: false}, "15005")

	assert.Equal(t, rollout.NodeRoleControlPlane, cp.Role())
	assert.Equal(t, rollout.NodeRoleWorker, worker.Role())
}

func TestNodeAdapter_AgentEndpoint(t *testing.T) {
	id := uuid.New()
	n := cwe.NewNode(db.Node{ID: id, Name: "cp-0", IP: "10.0.0.10"}, "15005")

	assert.Equal(t, id, n.ID())
	assert.Equal(t, "cp-0", n.Name())
	assert.Equal(t, "10.0.0.10:15005", n.AgentEndpoint())
}

func TestClusterAdapter_MappingAndDeterministicOrder(t *testing.T) {
	clusterID := uuid.New()
	cpA := db.Node{ID: uuid.New(), Name: "cp-a", IP: "10.0.0.11", ControlPlane: true, ClusterID: clusterID}
	cpB := db.Node{ID: uuid.New(), Name: "cp-b", IP: "10.0.0.12", ControlPlane: true, ClusterID: clusterID}
	worker := db.Node{ID: uuid.New(), Name: "worker-0", IP: "10.0.0.20", ControlPlane: false, ClusterID: clusterID}

	// Intentionally out of name order to prove the adapter sorts.
	all := []db.Node{worker, cpB, cpA}
	cps := []db.Node{cpB, cpA}

	cluster := cwe.NewCluster(db.Cluster{
		ID:                clusterID,
		Name:              "test",
		KubernetesVersion: "v1.30.0",
		ControlPlaneVIP:   "10.0.0.5",
	}, all, cps, "15005")

	assert.Equal(t, clusterID, cluster.ID())
	assert.Equal(t, "test", cluster.Name())
	assert.Equal(t, "v1.30.0", cluster.KubernetesVersion())
	assert.Equal(t, "10.0.0.5", cluster.ControlPlaneVIP())

	gotCP := cluster.ControlPlaneNodes()
	assert.Len(t, gotCP, 2)
	assert.Equal(t, "cp-a", gotCP[0].Name(), "control-plane nodes must be name-sorted")
	assert.Equal(t, "cp-b", gotCP[1].Name())
	assert.Equal(t, rollout.NodeRoleControlPlane, gotCP[0].Role())
	assert.Equal(t, "10.0.0.11:15005", gotCP[0].AgentEndpoint())

	gotAll := cluster.Nodes()
	assert.Len(t, gotAll, 3)
	assert.Equal(t, []string{"cp-a", "cp-b", "worker-0"},
		[]string{gotAll[0].Name(), gotAll[1].Name(), gotAll[2].Name()},
		"all nodes must be name-sorted")
	assert.Equal(t, rollout.NodeRoleWorker, gotAll[2].Role())
}
