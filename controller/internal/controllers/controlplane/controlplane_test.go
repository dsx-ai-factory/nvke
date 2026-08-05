// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package controlplane

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/NVIDIA/nke/controller/internal/db"
)

func TestControlPlane_HasNodesMarkedForDeletion(t *testing.T) {
	tests := []struct {
		name  string
		nodes []db.Node
		want  bool
	}{
		{
			name:  "empty control plane",
			nodes: []db.Node{},
			want:  false,
		},
		{
			name: "no nodes marked for deletion",
			nodes: []db.Node{
				{Conditions: db.NodeConditions{Deleting: db.Condition{Status: db.ConditionStatusFalse}}},
				{Conditions: db.NodeConditions{Deleting: db.Condition{Status: db.ConditionStatusFalse}}},
				{Conditions: db.NodeConditions{Deleting: db.Condition{Status: db.ConditionStatusUnknown}}},
			},
			want: false,
		},
		{
			name: "one node marked for deletion",
			nodes: []db.Node{
				{Conditions: db.NodeConditions{Deleting: db.Condition{Status: db.ConditionStatusFalse}}},
				{Conditions: db.NodeConditions{Deleting: db.Condition{Status: db.ConditionStatusFalse}}},
				{Conditions: db.NodeConditions{Deleting: db.Condition{Status: db.ConditionStatusTrue}}},
			},
			want: true,
		},
		{
			name: "all nodes marked for deletion",
			nodes: []db.Node{
				{Conditions: db.NodeConditions{Deleting: db.Condition{Status: db.ConditionStatusTrue}}},
				{Conditions: db.NodeConditions{Deleting: db.Condition{Status: db.ConditionStatusTrue}}},
				{Conditions: db.NodeConditions{Deleting: db.Condition{Status: db.ConditionStatusTrue}}},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cp := &controlPlane{nodes: tt.nodes}
			assert.Equal(t, tt.want, cp.hasNodesMarkedForDeletion())
		})
	}
}

func TestControlPlane_HasHealthyNodesProvisioning(t *testing.T) {
	tests := []struct {
		name  string
		nodes []db.Node
		want  bool
	}{
		{
			name:  "empty control plane",
			nodes: []db.Node{},
			want:  false,
		},
		{
			name: "all nodes kubernetes ready and provisioned",
			nodes: []db.Node{
				{Conditions: db.NodeConditions{
					KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
					Provisioned:     db.Condition{Status: db.ConditionStatusTrue},
				}},
				{Conditions: db.NodeConditions{
					KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
					Provisioned:     db.Condition{Status: db.ConditionStatusTrue},
				}},
				{Conditions: db.NodeConditions{
					KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
					Provisioned:     db.Condition{Status: db.ConditionStatusTrue},
				}},
			},
			want: false,
		},
		{
			name: "one node not provisioned and not marked for recycling or deletion",
			nodes: []db.Node{
				{Conditions: db.NodeConditions{
					KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
					Provisioned:     db.Condition{Status: db.ConditionStatusTrue},
				}},
				{Conditions: db.NodeConditions{
					KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
					Provisioned:     db.Condition{Status: db.ConditionStatusTrue},
				}},
				{Conditions: db.NodeConditions{
					KubernetesReady:    db.Condition{Status: db.ConditionStatusTrue},
					Provisioned:        db.Condition{Status: db.ConditionStatusFalse},
					MarkedForRecycling: db.Condition{Status: db.ConditionStatusFalse},
					Deleting:           db.Condition{Status: db.ConditionStatusFalse},
				}},
			},
			want: true,
		},
		{
			name: "one node not kubernetes ready and not marked for recycling or deletion",
			nodes: []db.Node{
				{Conditions: db.NodeConditions{
					KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
					Provisioned:     db.Condition{Status: db.ConditionStatusTrue},
				}},
				{Conditions: db.NodeConditions{
					KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
					Provisioned:     db.Condition{Status: db.ConditionStatusTrue},
				}},
				{Conditions: db.NodeConditions{
					KubernetesReady:    db.Condition{Status: db.ConditionStatusFalse},
					Provisioned:        db.Condition{Status: db.ConditionStatusFalse},
					MarkedForRecycling: db.Condition{Status: db.ConditionStatusFalse},
					Deleting:           db.Condition{Status: db.ConditionStatusFalse},
				}},
			},
			want: true,
		},
		{
			name: "not provisioned node is marked for recycling",
			nodes: []db.Node{
				{Conditions: db.NodeConditions{
					KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
					Provisioned:     db.Condition{Status: db.ConditionStatusTrue},
				}},
				{Conditions: db.NodeConditions{
					KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
					Provisioned:     db.Condition{Status: db.ConditionStatusTrue},
				}},
				{Conditions: db.NodeConditions{
					Provisioned:        db.Condition{Status: db.ConditionStatusFalse},
					MarkedForRecycling: db.Condition{Status: db.ConditionStatusTrue},
				}},
			},
			want: false,
		},
		{
			name: "not provisioned node is marked for deletion",
			nodes: []db.Node{
				{Conditions: db.NodeConditions{
					KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
					Provisioned:     db.Condition{Status: db.ConditionStatusTrue},
				}},
				{Conditions: db.NodeConditions{
					KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
					Provisioned:     db.Condition{Status: db.ConditionStatusTrue},
				}},
				{Conditions: db.NodeConditions{
					Provisioned: db.Condition{Status: db.ConditionStatusFalse},
					Deleting:    db.Condition{Status: db.ConditionStatusTrue},
				}},
			},
			want: false,
		},
		{
			name: "not kubernetes ready node is marked for recycling",
			nodes: []db.Node{
				{Conditions: db.NodeConditions{
					KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
					Provisioned:     db.Condition{Status: db.ConditionStatusTrue},
				}},
				{Conditions: db.NodeConditions{
					KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
					Provisioned:     db.Condition{Status: db.ConditionStatusTrue},
				}},
				{Conditions: db.NodeConditions{
					KubernetesReady:    db.Condition{Status: db.ConditionStatusFalse},
					MarkedForRecycling: db.Condition{Status: db.ConditionStatusTrue},
				}},
			},
			want: false,
		},
		{
			name: "five node control plane with one healthy provisioning",
			nodes: []db.Node{
				// kubernetes ready and provisioned
				{Conditions: db.NodeConditions{
					KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
					Provisioned:     db.Condition{Status: db.ConditionStatusTrue},
				}},
				// kubernetes ready and provisioned
				{Conditions: db.NodeConditions{
					KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
					Provisioned:     db.Condition{Status: db.ConditionStatusTrue},
				}},
				// not provisioned but marked for recycling
				{Conditions: db.NodeConditions{
					Provisioned:        db.Condition{Status: db.ConditionStatusFalse},
					MarkedForRecycling: db.Condition{Status: db.ConditionStatusTrue},
				}},
				// kubernetes ready and provisioned
				{Conditions: db.NodeConditions{
					KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
					Provisioned:     db.Condition{Status: db.ConditionStatusTrue},
				}},
				// not provisioned and healthy (still provisioning)
				{Conditions: db.NodeConditions{
					Provisioned:        db.Condition{Status: db.ConditionStatusFalse},
					MarkedForRecycling: db.Condition{Status: db.ConditionStatusFalse},
					Deleting:           db.Condition{Status: db.ConditionStatusFalse},
				}},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cp := &controlPlane{nodes: tt.nodes}
			assert.Equal(t, tt.want, cp.hasHealthyNodesProvisioning())
		})
	}
}

func TestControlPlane_Size(t *testing.T) {
	tests := []struct {
		name  string
		nodes []db.Node
		want  int
	}{
		{
			name:  "empty control plane",
			nodes: []db.Node{},
			want:  0,
		},
		{
			name:  "single node",
			nodes: []db.Node{{}},
			want:  1,
		},
		{
			name:  "three nodes",
			nodes: []db.Node{{}, {}, {}},
			want:  3,
		},
		{
			name:  "five nodes",
			nodes: []db.Node{{}, {}, {}, {}, {}},
			want:  5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cp := &controlPlane{nodes: tt.nodes}
			assert.Equal(t, tt.want, cp.size())
		})
	}
}

func TestControlPlane_NodesForRemediation(t *testing.T) {
	nodeMarked1 := db.Node{Name: "node1", Conditions: db.NodeConditions{MarkedForRecycling: db.Condition{Status: db.ConditionStatusTrue}}}
	nodeHealthy1 := db.Node{Name: "node2", Conditions: db.NodeConditions{MarkedForRecycling: db.Condition{Status: db.ConditionStatusFalse}}}
	nodeMarked2 := db.Node{Name: "node3", Conditions: db.NodeConditions{MarkedForRecycling: db.Condition{Status: db.ConditionStatusTrue}}}
	nodeHealthy2 := db.Node{Name: "node4", Conditions: db.NodeConditions{MarkedForRecycling: db.Condition{Status: db.ConditionStatusUnknown}}}
	nodeHealthy3 := db.Node{Name: "node5", Conditions: db.NodeConditions{MarkedForRecycling: db.Condition{Status: db.ConditionStatusFalse}}}
	nodeMarked3 := db.Node{Name: "node6", Conditions: db.NodeConditions{MarkedForRecycling: db.Condition{Status: db.ConditionStatusTrue}}}

	tests := []struct {
		name  string
		nodes []db.Node
		want  []db.Node
	}{
		{
			name:  "empty control plane",
			nodes: []db.Node{},
			want:  nil,
		},
		{
			name:  "no nodes marked for recycling in three node control plane",
			nodes: []db.Node{nodeHealthy1, nodeHealthy2, nodeHealthy3},
			want:  nil,
		},
		{
			name:  "one node marked for recycling in three node control plane",
			nodes: []db.Node{nodeMarked1, nodeHealthy1, nodeHealthy2},
			want:  []db.Node{nodeMarked1},
		},
		{
			name:  "multiple nodes marked for recycling in five node control plane",
			nodes: []db.Node{nodeMarked1, nodeHealthy1, nodeMarked2, nodeHealthy2, nodeHealthy3},
			want:  []db.Node{nodeMarked1, nodeMarked2},
		},
		{
			name:  "all nodes marked for recycling in three node control plane",
			nodes: []db.Node{nodeMarked1, nodeMarked2, nodeMarked3},
			want:  []db.Node{nodeMarked1, nodeMarked2, nodeMarked3},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cp := &controlPlane{nodes: tt.nodes}
			assert.Equal(t, tt.want, cp.nodesForRemediation())
		})
	}
}

func TestIsKubernetesReady(t *testing.T) {
	tests := []struct {
		name string
		node db.Node
		want bool
	}{
		{
			name: "kubernetes ready is true",
			node: db.Node{Conditions: db.NodeConditions{KubernetesReady: db.Condition{Status: db.ConditionStatusTrue}}},
			want: true,
		},
		{
			name: "kubernetes ready is false",
			node: db.Node{Conditions: db.NodeConditions{KubernetesReady: db.Condition{Status: db.ConditionStatusFalse}}},
			want: false,
		},
		{
			name: "kubernetes ready is unknown",
			node: db.Node{Conditions: db.NodeConditions{KubernetesReady: db.Condition{Status: db.ConditionStatusUnknown}}},
			want: false,
		},
		{
			name: "kubernetes ready is empty",
			node: db.Node{Conditions: db.NodeConditions{KubernetesReady: db.Condition{Status: ""}}},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isKubernetesReady(tt.node))
		})
	}
}

func TestIsPending(t *testing.T) {
	tests := []struct {
		name string
		node db.Node
		want bool
	}{
		{
			name: "pending is true",
			node: db.Node{Conditions: db.NodeConditions{Pending: db.Condition{Status: db.ConditionStatusTrue}}},
			want: true,
		},
		{
			name: "pending is false",
			node: db.Node{Conditions: db.NodeConditions{Pending: db.Condition{Status: db.ConditionStatusFalse}}},
			want: false,
		},
		{
			name: "pending is unknown",
			node: db.Node{Conditions: db.NodeConditions{Pending: db.Condition{Status: db.ConditionStatusUnknown}}},
			want: false,
		},
		{
			name: "pending is empty",
			node: db.Node{Conditions: db.NodeConditions{Pending: db.Condition{Status: ""}}},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isPending(tt.node))
		})
	}
}

func TestIsMarkedForRecycling(t *testing.T) {
	tests := []struct {
		name string
		node db.Node
		want bool
	}{
		{
			name: "marked for recycling is true",
			node: db.Node{Conditions: db.NodeConditions{MarkedForRecycling: db.Condition{Status: db.ConditionStatusTrue}}},
			want: true,
		},
		{
			name: "marked for recycling is false",
			node: db.Node{Conditions: db.NodeConditions{MarkedForRecycling: db.Condition{Status: db.ConditionStatusFalse}}},
			want: false,
		},
		{
			name: "marked for recycling is unknown",
			node: db.Node{Conditions: db.NodeConditions{MarkedForRecycling: db.Condition{Status: db.ConditionStatusUnknown}}},
			want: false,
		},
		{
			name: "marked for recycling is empty",
			node: db.Node{Conditions: db.NodeConditions{MarkedForRecycling: db.Condition{Status: ""}}},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isMarkedForRecycling(tt.node))
		})
	}
}

func TestIsMarkedForDeletion(t *testing.T) {
	tests := []struct {
		name string
		node db.Node
		want bool
	}{
		{
			name: "deleting is true",
			node: db.Node{Conditions: db.NodeConditions{Deleting: db.Condition{Status: db.ConditionStatusTrue}}},
			want: true,
		},
		{
			name: "deleting is false",
			node: db.Node{Conditions: db.NodeConditions{Deleting: db.Condition{Status: db.ConditionStatusFalse}}},
			want: false,
		},
		{
			name: "deleting is unknown",
			node: db.Node{Conditions: db.NodeConditions{Deleting: db.Condition{Status: db.ConditionStatusUnknown}}},
			want: false,
		},
		{
			name: "deleting is empty",
			node: db.Node{Conditions: db.NodeConditions{Deleting: db.Condition{Status: ""}}},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isMarkedForDeletion(tt.node))
		})
	}
}

func TestIsProvisioned(t *testing.T) {
	tests := []struct {
		name string
		node db.Node
		want bool
	}{
		{
			name: "provisioned is true",
			node: db.Node{Conditions: db.NodeConditions{Provisioned: db.Condition{Status: db.ConditionStatusTrue}}},
			want: true,
		},
		{
			name: "provisioned is false",
			node: db.Node{Conditions: db.NodeConditions{Provisioned: db.Condition{Status: db.ConditionStatusFalse}}},
			want: false,
		},
		{
			name: "provisioned is unknown",
			node: db.Node{Conditions: db.NodeConditions{Provisioned: db.Condition{Status: db.ConditionStatusUnknown}}},
			want: false,
		},
		{
			name: "provisioned is empty",
			node: db.Node{Conditions: db.NodeConditions{Provisioned: db.Condition{Status: ""}}},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isProvisioned(tt.node))
		})
	}
}

func TestNodePool_ExpectedCount(t *testing.T) {
	currentNodeSetID := uuid.New()
	deprecatedNodeSetID := uuid.New()

	tests := []struct {
		name     string
		nodePool nodePool
		want     int
	}{
		{
			name: "normal operation - no deprecated nodes, returns DesiredCount",
			nodePool: nodePool{
				NodePool: db.NodePool{
					DesiredCount:     3,
					MaxSurge:         1,
					CurrentNodeSetID: &currentNodeSetID,
				},
				nodeSets: []nodeSet{
					{NodeSet: db.NodeSet{ID: currentNodeSetID}, nodes: []db.Node{{}, {}, {}}},
				},
			},
			want: 3,
		},
		{
			name: "during upgrade - has deprecated nodes, returns DesiredCount + MaxSurge",
			nodePool: nodePool{
				NodePool: db.NodePool{
					DesiredCount:     3,
					MaxSurge:         1,
					CurrentNodeSetID: &currentNodeSetID,
				},
				nodeSets: []nodeSet{
					{NodeSet: db.NodeSet{ID: currentNodeSetID}, nodes: []db.Node{{}, {}}},
					{NodeSet: db.NodeSet{ID: deprecatedNodeSetID}, nodes: []db.Node{{}}}, // 1 deprecated node
				},
			},
			want: 4, // 3 + 1 surge
		},
		{
			name: "MaxSurge is 0, no extra nodes expected even with deprecated nodes",
			nodePool: nodePool{
				NodePool: db.NodePool{
					DesiredCount:     3,
					MaxSurge:         0,
					CurrentNodeSetID: &currentNodeSetID,
				},
				nodeSets: []nodeSet{
					{NodeSet: db.NodeSet{ID: currentNodeSetID}, nodes: []db.Node{{}}},
					{NodeSet: db.NodeSet{ID: deprecatedNodeSetID}, nodes: []db.Node{{}}},
				},
			},
			want: 3, // no surge because MaxSurge is 0
		},
		{
			name: "empty deprecated node set does not trigger surge",
			nodePool: nodePool{
				NodePool: db.NodePool{
					DesiredCount:     3,
					MaxSurge:         1,
					CurrentNodeSetID: &currentNodeSetID,
				},
				nodeSets: []nodeSet{
					{NodeSet: db.NodeSet{ID: currentNodeSetID}, nodes: []db.Node{{}, {}, {}}},
					{NodeSet: db.NodeSet{ID: deprecatedNodeSetID}, nodes: nil}, // exists but has no nodes
				},
			},
			want: 3, // no surge because deprecated node set is empty
		},
		{
			name: "MaxSurge larger than deprecated count - surge capped to deprecated count",
			nodePool: nodePool{
				NodePool: db.NodePool{
					DesiredCount:     3,
					MaxSurge:         2,
					CurrentNodeSetID: &currentNodeSetID,
				},
				nodeSets: []nodeSet{
					{NodeSet: db.NodeSet{ID: currentNodeSetID}, nodes: []db.Node{{}, {}}},
					{NodeSet: db.NodeSet{ID: deprecatedNodeSetID}, nodes: []db.Node{{}}}, // only 1 deprecated node
				},
			},
			want: 4, // 3 + min(2, 1) = 4, not 5
		},
		{
			name: "MaxSurge smaller than deprecated count - surge equals MaxSurge",
			nodePool: nodePool{
				NodePool: db.NodePool{
					DesiredCount:     3,
					MaxSurge:         1,
					CurrentNodeSetID: &currentNodeSetID,
				},
				nodeSets: []nodeSet{
					{NodeSet: db.NodeSet{ID: currentNodeSetID}, nodes: []db.Node{{}}},
					{NodeSet: db.NodeSet{ID: deprecatedNodeSetID}, nodes: []db.Node{{}, {}}}, // 2 deprecated nodes
				},
			},
			want: 4, // DesiredCount(3) + min(MaxSurge(1), deprecated(2))
		},
		{
			name: "MaxSurge equals deprecated count",
			nodePool: nodePool{
				NodePool: db.NodePool{
					DesiredCount:     3,
					MaxSurge:         2,
					CurrentNodeSetID: &currentNodeSetID,
				},
				nodeSets: []nodeSet{
					{NodeSet: db.NodeSet{ID: currentNodeSetID}, nodes: []db.Node{{}}},
					{NodeSet: db.NodeSet{ID: deprecatedNodeSetID}, nodes: []db.Node{{}, {}}}, // 2 deprecated nodes
				},
			},
			want: 5, // DesiredCount(3) + min(MaxSurge(2), deprecated(2))
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.nodePool.expectedCount())
		})
	}
}

func TestNodePool_CountDeprecatedNodes(t *testing.T) {
	currentNodeSetID := uuid.New()
	deprecatedNodeSetID1 := uuid.New()
	deprecatedNodeSetID2 := uuid.New()

	tests := []struct {
		name     string
		nodePool nodePool
		want     int
	}{
		{
			name: "all nodes in current node set",
			nodePool: nodePool{
				NodePool: db.NodePool{CurrentNodeSetID: &currentNodeSetID},
				nodeSets: []nodeSet{
					{NodeSet: db.NodeSet{ID: currentNodeSetID}, nodes: []db.Node{{}, {}}},
				},
			},
			want: 0,
		},
		{
			name: "one node in deprecated node set",
			nodePool: nodePool{
				NodePool: db.NodePool{CurrentNodeSetID: &currentNodeSetID},
				nodeSets: []nodeSet{
					{NodeSet: db.NodeSet{ID: currentNodeSetID}, nodes: []db.Node{{}}},
					{NodeSet: db.NodeSet{ID: deprecatedNodeSetID1}, nodes: []db.Node{{}}},
				},
			},
			want: 1,
		},
		{
			name: "multiple deprecated node sets with nodes",
			nodePool: nodePool{
				NodePool: db.NodePool{CurrentNodeSetID: &currentNodeSetID},
				nodeSets: []nodeSet{
					{NodeSet: db.NodeSet{ID: currentNodeSetID}, nodes: []db.Node{{}}},
					{NodeSet: db.NodeSet{ID: deprecatedNodeSetID1}, nodes: []db.Node{{}}},
					{NodeSet: db.NodeSet{ID: deprecatedNodeSetID2}, nodes: []db.Node{{}}},
				},
			},
			want: 2,
		},
		{
			name: "deprecated node set exists but has no nodes",
			nodePool: nodePool{
				NodePool: db.NodePool{CurrentNodeSetID: &currentNodeSetID},
				nodeSets: []nodeSet{
					{NodeSet: db.NodeSet{ID: currentNodeSetID}, nodes: []db.Node{{}, {}}},
					{NodeSet: db.NodeSet{ID: deprecatedNodeSetID1}, nodes: nil},
				},
			},
			want: 0,
		},
		{
			name: "no node sets",
			nodePool: nodePool{
				NodePool: db.NodePool{CurrentNodeSetID: &currentNodeSetID},
				nodeSets: []nodeSet{},
			},
			want: 0,
		},
		{
			name: "deprecated node set is counted",
			nodePool: nodePool{
				NodePool: db.NodePool{CurrentNodeSetID: &currentNodeSetID},
				nodeSets: []nodeSet{
					{NodeSet: db.NodeSet{ID: currentNodeSetID}, nodes: []db.Node{{}}},
					{NodeSet: db.NodeSet{ID: deprecatedNodeSetID1}, nodes: []db.Node{{}}},
				},
			},
			want: 1,
		},
		{
			name: "current node set is not counted",
			nodePool: nodePool{
				NodePool: db.NodePool{CurrentNodeSetID: &currentNodeSetID},
				nodeSets: []nodeSet{
					{NodeSet: db.NodeSet{ID: currentNodeSetID}, nodes: []db.Node{{}}},
				},
			},
			want: 0,
		},
		{
			name: "multiple nodes in same deprecated node set",
			nodePool: nodePool{
				NodePool: db.NodePool{CurrentNodeSetID: &currentNodeSetID},
				nodeSets: []nodeSet{
					{NodeSet: db.NodeSet{ID: currentNodeSetID}, nodes: []db.Node{{}}},
					{NodeSet: db.NodeSet{ID: deprecatedNodeSetID1}, nodes: []db.Node{{}, {}, {}}},
				},
			},
			want: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.nodePool.countDeprecatedNodes())
		})
	}
}

func TestControlPlane_ExpectedCount(t *testing.T) {
	currentNodeSetID1 := uuid.New()
	currentNodeSetID2 := uuid.New()
	deprecatedNodeSetID := uuid.New()

	tests := []struct {
		name         string
		controlPlane *controlPlane
		want         int
	}{
		{
			name: "single node pool, no deprecated nodes",
			controlPlane: &controlPlane{
				nodes: []db.Node{{}, {}, {}},
				nodePools: []nodePool{
					{
						NodePool: db.NodePool{
							DesiredCount:     3,
							MaxSurge:         1,
							CurrentNodeSetID: &currentNodeSetID1,
						},
						nodeSets: []nodeSet{
							{NodeSet: db.NodeSet{ID: currentNodeSetID1}, nodes: []db.Node{{}, {}, {}}},
						},
					},
				},
			},
			want: 3,
		},
		{
			name: "single node pool with deprecated nodes",
			controlPlane: &controlPlane{
				nodes: []db.Node{{}, {}, {}},
				nodePools: []nodePool{
					{
						NodePool: db.NodePool{
							DesiredCount:     3,
							MaxSurge:         1,
							CurrentNodeSetID: &currentNodeSetID1,
						},
						nodeSets: []nodeSet{
							{NodeSet: db.NodeSet{ID: currentNodeSetID1}, nodes: []db.Node{{}, {}}},
							{NodeSet: db.NodeSet{ID: deprecatedNodeSetID}, nodes: []db.Node{{}}},
						},
					},
				},
			},
			want: 4, // 3 + 1 surge
		},
		{
			name: "multiple node pools, no deprecated nodes",
			controlPlane: &controlPlane{
				nodes: []db.Node{{}, {}, {}, {}, {}},
				nodePools: []nodePool{
					{
						NodePool: db.NodePool{
							DesiredCount:     3,
							MaxSurge:         1,
							CurrentNodeSetID: &currentNodeSetID1,
						},
						nodeSets: []nodeSet{
							{NodeSet: db.NodeSet{ID: currentNodeSetID1}, nodes: []db.Node{{}, {}, {}}},
						},
					},
					{
						NodePool: db.NodePool{
							DesiredCount:     2,
							MaxSurge:         1,
							CurrentNodeSetID: &currentNodeSetID2,
						},
						nodeSets: []nodeSet{
							{NodeSet: db.NodeSet{ID: currentNodeSetID2}, nodes: []db.Node{{}, {}}},
						},
					},
				},
			},
			want: 5, // 3 + 2
		},
		{
			name: "multiple node pools, one with deprecated nodes",
			controlPlane: &controlPlane{
				nodes: []db.Node{{}, {}, {}, {}},
				nodePools: []nodePool{
					{
						NodePool: db.NodePool{
							DesiredCount:     3,
							MaxSurge:         1,
							CurrentNodeSetID: &currentNodeSetID1,
						},
						nodeSets: []nodeSet{
							{NodeSet: db.NodeSet{ID: currentNodeSetID1}, nodes: []db.Node{{}}},
							{NodeSet: db.NodeSet{ID: deprecatedNodeSetID}, nodes: []db.Node{{}}}, // 1 deprecated
						},
					},
					{
						NodePool: db.NodePool{
							DesiredCount:     2,
							MaxSurge:         1,
							CurrentNodeSetID: &currentNodeSetID2,
						},
						nodeSets: []nodeSet{
							{NodeSet: db.NodeSet{ID: currentNodeSetID2}, nodes: []db.Node{{}, {}}},
						},
					},
				},
			},
			want: 6, // (3 + 1 surge) + 2
		},
		{
			name:         "no node pools",
			controlPlane: &controlPlane{nodes: []db.Node{}, nodePools: []nodePool{}},
			want:         0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.controlPlane.expectedCount())
		})
	}
}
