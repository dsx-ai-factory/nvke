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
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/NVIDIA/nke/controller/internal/db"
)

func TestSelectNodeToRemediate(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name         string
		markedNodes  []db.Node
		wantNodeName string
	}{
		{
			name: "single node returns that node",
			markedNodes: []db.Node{
				{Name: "node1", CreatedAt: now},
			},
			wantNodeName: "node1",
		},
		{
			name: "selects oldest pending node first",
			markedNodes: []db.Node{
				{Name: "older-ready", CreatedAt: now.Add(-2 * time.Hour), Conditions: db.NodeConditions{
					Pending:         db.Condition{Status: db.ConditionStatusFalse},
					Provisioned:     db.Condition{Status: db.ConditionStatusTrue},
					KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
					NodeAgentReady:  db.Condition{Status: db.ConditionStatusTrue},
				}},
				{Name: "newer-pending", CreatedAt: now.Add(-1 * time.Hour), Conditions: db.NodeConditions{
					Pending:         db.Condition{Status: db.ConditionStatusTrue},
					KubernetesReady: db.Condition{Status: db.ConditionStatusFalse},
				}},
				{Name: "oldest-pending", CreatedAt: now.Add(-3 * time.Hour), Conditions: db.NodeConditions{
					Pending:         db.Condition{Status: db.ConditionStatusTrue},
					KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
				}},
				{Name: "oldest-kube-not-ready", CreatedAt: now.Add(-4 * time.Hour), Conditions: db.NodeConditions{
					Pending:         db.Condition{Status: db.ConditionStatusFalse},
					Provisioned:     db.Condition{Status: db.ConditionStatusTrue},
					KubernetesReady: db.Condition{Status: db.ConditionStatusFalse},
					NodeAgentReady:  db.Condition{Status: db.ConditionStatusTrue},
				}},
				{Name: "oldest-not-provisioned", CreatedAt: now.Add(-5 * time.Hour), Conditions: db.NodeConditions{
					Pending:         db.Condition{Status: db.ConditionStatusFalse},
					Provisioned:     db.Condition{Status: db.ConditionStatusFalse},
					KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
					NodeAgentReady:  db.Condition{Status: db.ConditionStatusTrue},
				}},
				{Name: "oldest-agent-not-ready", CreatedAt: now.Add(-5 * time.Hour), Conditions: db.NodeConditions{
					Pending:         db.Condition{Status: db.ConditionStatusFalse},
					Provisioned:     db.Condition{Status: db.ConditionStatusTrue},
					KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
					NodeAgentReady:  db.Condition{Status: db.ConditionStatusFalse},
				}},
			},
			wantNodeName: "oldest-pending",
		},
		{
			name: "selects oldest not-provisioned node when no pending nodes",
			markedNodes: []db.Node{
				{Name: "provisioned-oldest", CreatedAt: now.Add(-3 * time.Hour), Conditions: db.NodeConditions{
					Pending:         db.Condition{Status: db.ConditionStatusFalse},
					Provisioned:     db.Condition{Status: db.ConditionStatusTrue},
					KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
					NodeAgentReady:  db.Condition{Status: db.ConditionStatusTrue},
				}},
				{Name: "not-provisioned-newer", CreatedAt: now.Add(-1 * time.Hour), Conditions: db.NodeConditions{
					Pending:         db.Condition{Status: db.ConditionStatusFalse},
					Provisioned:     db.Condition{Status: db.ConditionStatusFalse},
					KubernetesReady: db.Condition{Status: db.ConditionStatusFalse},
					NodeAgentReady:  db.Condition{Status: db.ConditionStatusFalse},
				}},
				{Name: "not-provisioned-oldest", CreatedAt: now.Add(-2 * time.Hour), Conditions: db.NodeConditions{
					Pending:         db.Condition{Status: db.ConditionStatusFalse},
					Provisioned:     db.Condition{Status: db.ConditionStatusFalse},
					KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
					NodeAgentReady:  db.Condition{Status: db.ConditionStatusTrue},
				}},
				{Name: "oldest-not-kube-ready", CreatedAt: now.Add(-3 * time.Hour), Conditions: db.NodeConditions{
					Pending:         db.Condition{Status: db.ConditionStatusFalse},
					Provisioned:     db.Condition{Status: db.ConditionStatusTrue},
					KubernetesReady: db.Condition{Status: db.ConditionStatusFalse},
					NodeAgentReady:  db.Condition{Status: db.ConditionStatusTrue},
				}},
				{Name: "oldest-not-agent-ready", CreatedAt: now.Add(-4 * time.Hour), Conditions: db.NodeConditions{
					Pending:         db.Condition{Status: db.ConditionStatusFalse},
					Provisioned:     db.Condition{Status: db.ConditionStatusTrue},
					KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
					NodeAgentReady:  db.Condition{Status: db.ConditionStatusFalse},
				}},
			},
			wantNodeName: "not-provisioned-oldest",
		},
		{
			name: "selects oldest not-kubernetes-ready node when no pending nodes",
			markedNodes: []db.Node{
				{Name: "ready-oldest", CreatedAt: now.Add(-3 * time.Hour), Conditions: db.NodeConditions{
					Pending:         db.Condition{Status: db.ConditionStatusFalse},
					Provisioned:     db.Condition{Status: db.ConditionStatusTrue},
					KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
					NodeAgentReady:  db.Condition{Status: db.ConditionStatusTrue},
				}},
				{Name: "not-ready-newer", CreatedAt: now.Add(-1 * time.Hour), Conditions: db.NodeConditions{
					Pending:         db.Condition{Status: db.ConditionStatusFalse},
					Provisioned:     db.Condition{Status: db.ConditionStatusTrue},
					KubernetesReady: db.Condition{Status: db.ConditionStatusFalse},
					NodeAgentReady:  db.Condition{Status: db.ConditionStatusTrue},
				}},
				{Name: "not-ready-oldest", CreatedAt: now.Add(-2 * time.Hour), Conditions: db.NodeConditions{
					Pending:         db.Condition{Status: db.ConditionStatusFalse},
					Provisioned:     db.Condition{Status: db.ConditionStatusTrue},
					KubernetesReady: db.Condition{Status: db.ConditionStatusFalse},
					NodeAgentReady:  db.Condition{Status: db.ConditionStatusTrue},
				}},
				{Name: "oldest-not-agent-ready", CreatedAt: now.Add(-4 * time.Hour), Conditions: db.NodeConditions{
					Pending:         db.Condition{Status: db.ConditionStatusFalse},
					Provisioned:     db.Condition{Status: db.ConditionStatusTrue},
					KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
					NodeAgentReady:  db.Condition{Status: db.ConditionStatusFalse},
				}},
			},
			wantNodeName: "not-ready-oldest",
		},
		{
			name: "selects oldest not-NodeAgentReady node when all provisioned and KubernetesReady",
			markedNodes: []db.Node{
				{Name: "fully-ready-oldest", CreatedAt: now.Add(-3 * time.Hour), Conditions: db.NodeConditions{
					Pending:         db.Condition{Status: db.ConditionStatusFalse},
					Provisioned:     db.Condition{Status: db.ConditionStatusTrue},
					KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
					NodeAgentReady:  db.Condition{Status: db.ConditionStatusTrue},
				}},
				{Name: "not-agent-ready-newer", CreatedAt: now.Add(-1 * time.Hour), Conditions: db.NodeConditions{
					Pending:         db.Condition{Status: db.ConditionStatusFalse},
					Provisioned:     db.Condition{Status: db.ConditionStatusTrue},
					KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
					NodeAgentReady:  db.Condition{Status: db.ConditionStatusFalse},
				}},
				{Name: "not-agent-ready-oldest", CreatedAt: now.Add(-2 * time.Hour), Conditions: db.NodeConditions{
					Pending:         db.Condition{Status: db.ConditionStatusFalse},
					Provisioned:     db.Condition{Status: db.ConditionStatusTrue},
					KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
					NodeAgentReady:  db.Condition{Status: db.ConditionStatusFalse},
				}},
			},
			wantNodeName: "not-agent-ready-oldest",
		},
		{
			name: "selects oldest node when all nodes are fully ready",
			markedNodes: []db.Node{
				{Name: "ready-newer", CreatedAt: now.Add(-1 * time.Hour), Conditions: db.NodeConditions{
					Pending:         db.Condition{Status: db.ConditionStatusFalse},
					Provisioned:     db.Condition{Status: db.ConditionStatusTrue},
					KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
					NodeAgentReady:  db.Condition{Status: db.ConditionStatusTrue},
				}},
				{Name: "ready-oldest", CreatedAt: now.Add(-3 * time.Hour), Conditions: db.NodeConditions{
					Pending:         db.Condition{Status: db.ConditionStatusFalse},
					Provisioned:     db.Condition{Status: db.ConditionStatusTrue},
					KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
					NodeAgentReady:  db.Condition{Status: db.ConditionStatusTrue},
				}},
				{Name: "ready-middle", CreatedAt: now.Add(-2 * time.Hour), Conditions: db.NodeConditions{
					Pending:         db.Condition{Status: db.ConditionStatusFalse},
					Provisioned:     db.Condition{Status: db.ConditionStatusTrue},
					KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
					NodeAgentReady:  db.Condition{Status: db.ConditionStatusTrue},
				}},
			},
			wantNodeName: "ready-oldest",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := selectNodeToRemediate(tt.markedNodes)
			assert.Equal(t, tt.wantNodeName, result.Name)
		})
	}
}

func TestCanStartRemediation(t *testing.T) {
	log := slog.New(slog.DiscardHandler)

	currentNodeSetID := uuid.New()
	deprecatedNodeSetID := uuid.New()
	nodeToRemove := makeNodeToRemove(currentNodeSetID)

	tests := []struct {
		name           string
		controlPlane   *controlPlane
		nodeToRemove   db.Node
		expectedResult bool
	}{
		{
			name: "cannot remediate with less than 2 control plane nodes",
			controlPlane: func() *controlPlane {
				nodes := []db.Node{nodeToRemove}
				return &controlPlane{nodes: nodes, nodePools: makeNodePool(currentNodeSetID, 1, nodes)}
			}(),
			nodeToRemove:   nodeToRemove,
			expectedResult: false,
		},
		{
			name: "cannot remediate when current size is less than expected count",
			controlPlane: func() *controlPlane {
				nodes := []db.Node{nodeToRemove, makeHealthyNode("healthy-node", currentNodeSetID)}
				return &controlPlane{nodes: nodes, nodePools: makeNodePool(currentNodeSetID, 3, nodes)} // expected 3 but only have 2
			}(),
			nodeToRemove:   nodeToRemove,
			expectedResult: false,
		},
		{
			name: "cannot remediate when healthy nodes are provisioning",
			controlPlane: func() *controlPlane {
				nodes := []db.Node{
					nodeToRemove,
					{Name: "provisioning-node", NodeSetID: currentNodeSetID, Conditions: db.NodeConditions{
						Pending:            db.Condition{Status: db.ConditionStatusTrue},
						MarkedForRecycling: db.Condition{Status: db.ConditionStatusFalse},
						Deleting:           db.Condition{Status: db.ConditionStatusFalse},
					}},
				}
				return &controlPlane{nodes: nodes, nodePools: makeNodePool(currentNodeSetID, 2, nodes)}
			}(),
			nodeToRemove:   nodeToRemove,
			expectedResult: false,
		},
		{
			name: "cannot remediate when nodes are marked for deletion",
			controlPlane: func() *controlPlane {
				nodes := []db.Node{
					nodeToRemove,
					{Name: "deleting-node", NodeSetID: currentNodeSetID, Conditions: db.NodeConditions{
						Deleting:        db.Condition{Status: db.ConditionStatusTrue},
						KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
					}},
				}
				return &controlPlane{nodes: nodes, nodePools: makeNodePool(currentNodeSetID, 2, nodes)}
			}(),
			nodeToRemove:   nodeToRemove,
			expectedResult: false,
		},
		{
			name: "cannot remediate when it would lose etcd quorum - 2 nodes with 1 unhealthy",
			controlPlane: func() *controlPlane {
				nodes := []db.Node{
					nodeToRemove,
					{Name: "unhealthy-node", NodeSetID: currentNodeSetID, Conditions: db.NodeConditions{
						Pending:         db.Condition{Status: db.ConditionStatusFalse},
						KubernetesReady: db.Condition{Status: db.ConditionStatusFalse},
						Deleting:        db.Condition{Status: db.ConditionStatusFalse},
					}},
				}
				return &controlPlane{nodes: nodes, nodePools: makeNodePool(currentNodeSetID, 2, nodes)}
			}(),
			nodeToRemove:   nodeToRemove,
			expectedResult: false,
		},
		{
			name: "cannot remediate when it would lose etcd quorum - 3 nodes, 2 unhealthy after removal",
			controlPlane: func() *controlPlane {
				nodes := []db.Node{
					nodeToRemove,
					makeHealthyNode("healthy-node", currentNodeSetID),
					{Name: "unhealthy-node", NodeSetID: currentNodeSetID, Conditions: db.NodeConditions{
						Pending:         db.Condition{Status: db.ConditionStatusFalse},
						KubernetesReady: db.Condition{Status: db.ConditionStatusFalse},
						Deleting:        db.Condition{Status: db.ConditionStatusFalse},
					}},
				}
				return &controlPlane{nodes: nodes, nodePools: makeNodePool(currentNodeSetID, 3, nodes)}
			}(),
			nodeToRemove:   nodeToRemove,
			expectedResult: false,
		},
		{
			name: "can remediate with 2 healthy nodes",
			controlPlane: func() *controlPlane {
				nodes := []db.Node{nodeToRemove, makeHealthyNode("healthy-node", currentNodeSetID)}
				return &controlPlane{nodes: nodes, nodePools: makeNodePool(currentNodeSetID, 2, nodes)}
			}(),
			nodeToRemove:   nodeToRemove,
			expectedResult: true,
		},
		{
			name: "can remediate with 3 nodes when 2 remain healthy",
			controlPlane: func() *controlPlane {
				nodes := []db.Node{
					nodeToRemove,
					makeHealthyNode("healthy-node-1", currentNodeSetID),
					makeHealthyNode("healthy-node-2", currentNodeSetID),
				}
				return &controlPlane{nodes: nodes, nodePools: makeNodePool(currentNodeSetID, 3, nodes)}
			}(),
			nodeToRemove:   nodeToRemove,
			expectedResult: true,
		},
		{
			name: "cannot remediate with 5 nodes when quorum would be lost - 2 healthy after removal",
			controlPlane: func() *controlPlane {
				nodes := []db.Node{
					nodeToRemove,
					makeHealthyNode("healthy-node-1", currentNodeSetID),
					makeHealthyNode("healthy-node-2", currentNodeSetID),
					// Unhealthy nodes are marked for recycling so they don't block hasHealthyNodesProvisioning
					{Name: "unhealthy-node-1", NodeSetID: currentNodeSetID, Conditions: db.NodeConditions{
						Pending:            db.Condition{Status: db.ConditionStatusFalse},
						KubernetesReady:    db.Condition{Status: db.ConditionStatusFalse},
						Deleting:           db.Condition{Status: db.ConditionStatusFalse},
						MarkedForRecycling: db.Condition{Status: db.ConditionStatusTrue},
					}},
					{Name: "unhealthy-node-2", NodeSetID: currentNodeSetID, Conditions: db.NodeConditions{
						Pending:            db.Condition{Status: db.ConditionStatusFalse},
						KubernetesReady:    db.Condition{Status: db.ConditionStatusFalse},
						Deleting:           db.Condition{Status: db.ConditionStatusFalse},
						MarkedForRecycling: db.Condition{Status: db.ConditionStatusTrue},
					}},
				}
				return &controlPlane{nodes: nodes, nodePools: makeNodePool(currentNodeSetID, 5, nodes)}
			}(),
			nodeToRemove:   nodeToRemove,
			expectedResult: false, // 4 members, quorum=3, only 2 healthy = cannot remove
		},
		{
			name: "can remediate with 5 nodes when quorum maintained - 3 healthy after removal",
			controlPlane: func() *controlPlane {
				nodes := []db.Node{
					nodeToRemove,
					makeHealthyNode("healthy-node-1", currentNodeSetID),
					makeHealthyNode("healthy-node-2", currentNodeSetID),
					makeHealthyNode("healthy-node-3", currentNodeSetID),
					// This unhealthy node is also marked for recycling (known problematic)
					// so it doesn't block hasHealthyNodesProvisioning check
					{Name: "unhealthy-node", NodeSetID: currentNodeSetID, Conditions: db.NodeConditions{
						Pending:            db.Condition{Status: db.ConditionStatusFalse},
						KubernetesReady:    db.Condition{Status: db.ConditionStatusFalse},
						Deleting:           db.Condition{Status: db.ConditionStatusFalse},
						MarkedForRecycling: db.Condition{Status: db.ConditionStatusTrue},
					}},
				}
				return &controlPlane{nodes: nodes, nodePools: makeNodePool(currentNodeSetID, 5, nodes)}
			}(),
			nodeToRemove:   nodeToRemove,
			expectedResult: true, // 4 members, quorum=3, 3 healthy = can remove
		},
		{
			name: "can remediate with 3 nodes when 1 is pending if the pending node is marked for recycling",
			controlPlane: func() *controlPlane {
				nodes := []db.Node{
					nodeToRemove,
					makeHealthyNode("healthy-node", currentNodeSetID),
					{Name: "pending-node", NodeSetID: currentNodeSetID, Conditions: db.NodeConditions{
						Pending:         db.Condition{Status: db.ConditionStatusTrue},
						KubernetesReady: db.Condition{Status: db.ConditionStatusFalse},
						Deleting:        db.Condition{Status: db.ConditionStatusFalse},
						// This node is marked for recycling so it won't block hasHealthyNodesProvisioning
						MarkedForRecycling: db.Condition{Status: db.ConditionStatusTrue},
					}},
				}
				return &controlPlane{nodes: nodes, nodePools: makeNodePool(currentNodeSetID, 3, nodes)}
			}(),
			nodeToRemove:   nodeToRemove,
			expectedResult: true, // pending node not counted, 1 healthy member remaining, quorum=1
		},
		{
			name: "node to remove not counted in quorum calculation",
			controlPlane: func() *controlPlane {
				nodes := []db.Node{nodeToRemove, makeHealthyNode("healthy-node", currentNodeSetID)}
				return &controlPlane{nodes: nodes, nodePools: makeNodePool(currentNodeSetID, 2, nodes)}
			}(),
			nodeToRemove:   nodeToRemove,
			expectedResult: true, // node to remove excluded, 1 healthy member, quorum=1
		},
		// Sequential execution tests - verify size >= expectedCount before remediation
		{
			name: "cannot remediate when waiting for replacement node to be created",
			controlPlane: func() *controlPlane {
				nodes := []db.Node{
					nodeToRemove,
					makeHealthyNode("healthy-1", currentNodeSetID),
				}
				return &controlPlane{
					nodes: nodes,
					nodePools: []nodePool{{
						NodePool: db.NodePool{
							DesiredCount:     3,
							MaxSurge:         0,
							CurrentNodeSetID: &currentNodeSetID,
						},
						nodeSets: []nodeSet{{NodeSet: db.NodeSet{ID: currentNodeSetID}, nodes: nodes}},
					}},
				}
			}(),
			nodeToRemove:   nodeToRemove,
			expectedResult: false, // 2 nodes but expected 3
		},
		{
			name: "can remediate when size equals expected count",
			controlPlane: func() *controlPlane {
				nodes := []db.Node{
					nodeToRemove,
					makeHealthyNode("healthy-1", currentNodeSetID),
					makeHealthyNode("healthy-2", currentNodeSetID),
				}
				return &controlPlane{
					nodes: nodes,
					nodePools: []nodePool{{
						NodePool: db.NodePool{
							DesiredCount:     3,
							MaxSurge:         0,
							CurrentNodeSetID: &currentNodeSetID,
						},
						nodeSets: []nodeSet{{NodeSet: db.NodeSet{ID: currentNodeSetID}, nodes: nodes}},
					}},
				}
			}(),
			nodeToRemove:   nodeToRemove,
			expectedResult: true, // 3 nodes and expected 3
		},
		{
			name: "can remediate when size exceeds expected count",
			controlPlane: func() *controlPlane {
				nodes := []db.Node{
					nodeToRemove,
					makeHealthyNode("healthy-1", currentNodeSetID),
					makeHealthyNode("healthy-2", currentNodeSetID),
					makeHealthyNode("healthy-3", currentNodeSetID),
				}
				return &controlPlane{
					nodes: nodes,
					nodePools: []nodePool{{
						NodePool: db.NodePool{
							DesiredCount:     3,
							MaxSurge:         0,
							CurrentNodeSetID: &currentNodeSetID,
						},
						nodeSets: []nodeSet{{NodeSet: db.NodeSet{ID: currentNodeSetID}, nodes: nodes}},
					}},
				}
			}(),
			nodeToRemove:   nodeToRemove,
			expectedResult: true, // 4 nodes but expected 3
		},
		{
			name: "accounts for surge when deprecated nodes exist",
			controlPlane: func() *controlPlane {
				//nolint:prealloc // exact-capacity literal is deliberate: the slice is also appended to below, and spare capacity would alias the shared backing array.
				currentNodes := []db.Node{nodeToRemove, makeHealthyNode("healthy-1", currentNodeSetID)}
				deprecatedNodes := []db.Node{makeHealthyNode("deprecated-1", deprecatedNodeSetID)}
				return &controlPlane{
					nodes: append(currentNodes, deprecatedNodes...),
					nodePools: []nodePool{{
						NodePool: db.NodePool{
							DesiredCount:     3,
							MaxSurge:         1,
							CurrentNodeSetID: &currentNodeSetID,
						},
						nodeSets: []nodeSet{
							{NodeSet: db.NodeSet{ID: currentNodeSetID}, nodes: currentNodes},
							{NodeSet: db.NodeSet{ID: deprecatedNodeSetID}, nodes: deprecatedNodes},
						},
					}},
				}
			}(),
			nodeToRemove:   nodeToRemove,
			expectedResult: false, // 3 nodes but expected 4 (3 + 1 surge)
		},
		{
			name: "can remediate during upgrade when size equals expected with surge",
			controlPlane: func() *controlPlane {
				//nolint:prealloc // exact-capacity literal is deliberate: the slice is also appended to below, and spare capacity would alias the shared backing array.
				currentNodes := []db.Node{nodeToRemove, makeHealthyNode("healthy-1", currentNodeSetID), makeHealthyNode("healthy-2", currentNodeSetID)}
				deprecatedNodes := []db.Node{makeHealthyNode("deprecated-1", deprecatedNodeSetID)}
				return &controlPlane{
					nodes: append(currentNodes, deprecatedNodes...),
					nodePools: []nodePool{{
						NodePool: db.NodePool{
							DesiredCount:     3,
							MaxSurge:         1,
							CurrentNodeSetID: &currentNodeSetID,
						},
						nodeSets: []nodeSet{
							{NodeSet: db.NodeSet{ID: currentNodeSetID}, nodes: currentNodes},
							{NodeSet: db.NodeSet{ID: deprecatedNodeSetID}, nodes: deprecatedNodes},
						},
					}},
				}
			}(),
			nodeToRemove:   nodeToRemove,
			expectedResult: true, // 4 nodes and expected 4 (3 + 1 surge)
		},
		{
			name: "multiple node pools - both must satisfy expected count",
			controlPlane: func() *controlPlane {
				//nolint:prealloc // exact-capacity literal is deliberate: the slice is also appended to below, and spare capacity would alias the shared backing array.
				pool1Nodes := []db.Node{nodeToRemove, makeHealthyNode("pool1-healthy-1", currentNodeSetID)}
				pool2Nodes := []db.Node{makeHealthyNode("pool2-healthy-1", deprecatedNodeSetID)}
				return &controlPlane{
					nodes: append(pool1Nodes, pool2Nodes...),
					nodePools: []nodePool{
						{
							NodePool: db.NodePool{
								DesiredCount:     2,
								MaxSurge:         0,
								CurrentNodeSetID: &currentNodeSetID,
							},
							nodeSets: []nodeSet{{NodeSet: db.NodeSet{ID: currentNodeSetID}, nodes: pool1Nodes}},
						},
						{
							NodePool: db.NodePool{
								DesiredCount:     2,
								MaxSurge:         0,
								CurrentNodeSetID: &deprecatedNodeSetID,
							},
							nodeSets: []nodeSet{{NodeSet: db.NodeSet{ID: deprecatedNodeSetID}, nodes: pool2Nodes}},
						},
					},
				}
			}(),
			nodeToRemove:   nodeToRemove,
			expectedResult: false, // 3 nodes but expected 4 (2 + 2)
		},
		// Pre-condition 5: Enough healthy nodes to bootstrap another node
		// A healthy node must be KubernetesReady, Provisioned, and NodeAgentReady
		{
			name: "cannot remediate when no additional nodes have NodeAgentReady",
			controlPlane: func() *controlPlane {
				nodes := []db.Node{
					nodeToRemove,
					{Name: "kube-ready-but-no-agent", NodeSetID: currentNodeSetID, Conditions: db.NodeConditions{
						Pending:         db.Condition{Status: db.ConditionStatusFalse},
						KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
						Provisioned:     db.Condition{Status: db.ConditionStatusTrue},
						NodeAgentReady:  db.Condition{Status: db.ConditionStatusFalse}, // NOT ready
						Deleting:        db.Condition{Status: db.ConditionStatusFalse},
					}},
				}
				return &controlPlane{nodes: nodes, nodePools: makeNodePool(currentNodeSetID, 2, nodes)}
			}(),
			nodeToRemove:   nodeToRemove,
			expectedResult: false, // node is not fully healthy (missing NodeAgentReady)
		},
		{
			name: "cannot remediate when additional node is not Provisioned",
			controlPlane: func() *controlPlane {
				nodes := []db.Node{
					nodeToRemove,
					{Name: "kube-ready-agent-ready-not-provisioned", NodeSetID: currentNodeSetID, Conditions: db.NodeConditions{
						Pending:         db.Condition{Status: db.ConditionStatusFalse},
						KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
						Provisioned:     db.Condition{Status: db.ConditionStatusFalse}, // NOT provisioned
						NodeAgentReady:  db.Condition{Status: db.ConditionStatusTrue},
						Deleting:        db.Condition{Status: db.ConditionStatusFalse},
					}},
				}
				return &controlPlane{nodes: nodes, nodePools: makeNodePool(currentNodeSetID, 2, nodes)}
			}(),
			nodeToRemove:   nodeToRemove,
			expectedResult: false, // node is not fully healthy (missing Provisioned)
		},
		{
			name: "cannot remediate when additional node is not KubernetesReady",
			controlPlane: func() *controlPlane {
				nodes := []db.Node{
					nodeToRemove,
					{Name: "provisioned-agent-ready-not-kube-ready", NodeSetID: currentNodeSetID, Conditions: db.NodeConditions{
						Pending:            db.Condition{Status: db.ConditionStatusFalse},
						KubernetesReady:    db.Condition{Status: db.ConditionStatusFalse}, // NOT ready
						Provisioned:        db.Condition{Status: db.ConditionStatusTrue},
						NodeAgentReady:     db.Condition{Status: db.ConditionStatusTrue},
						Deleting:           db.Condition{Status: db.ConditionStatusFalse},
						MarkedForRecycling: db.Condition{Status: db.ConditionStatusTrue}, // marked for recycling to pass hasHealthyNodesProvisioning
					}},
				}
				return &controlPlane{nodes: nodes, nodePools: makeNodePool(currentNodeSetID, 2, nodes)}
			}(),
			nodeToRemove:   nodeToRemove,
			expectedResult: false, // node is not fully healthy (missing KubernetesReady)
		},
		{
			name: "can remediate when at least one node has all three conditions",
			controlPlane: func() *controlPlane {
				nodes := []db.Node{
					nodeToRemove,
					// One fully healthy node
					makeHealthyNode("fully-healthy", currentNodeSetID),
					// One node missing NodeAgentReady
					{Name: "missing-agent", NodeSetID: currentNodeSetID, Conditions: db.NodeConditions{
						Pending:            db.Condition{Status: db.ConditionStatusFalse},
						KubernetesReady:    db.Condition{Status: db.ConditionStatusTrue},
						Provisioned:        db.Condition{Status: db.ConditionStatusTrue},
						NodeAgentReady:     db.Condition{Status: db.ConditionStatusFalse},
						Deleting:           db.Condition{Status: db.ConditionStatusFalse},
						MarkedForRecycling: db.Condition{Status: db.ConditionStatusTrue},
					}},
				}
				return &controlPlane{nodes: nodes, nodePools: makeNodePool(currentNodeSetID, 3, nodes)}
			}(),
			nodeToRemove:   nodeToRemove,
			expectedResult: true, // one fully healthy node is enough
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := canStartRemediation(log, tt.controlPlane, tt.nodeToRemove)
			assert.Equal(t, tt.expectedResult, result)
		})
	}
}

func makeNodePool(currentNodeSetID uuid.UUID, desiredCount int, nodes []db.Node) []nodePool {
	return []nodePool{{
		NodePool: db.NodePool{
			DesiredCount:     desiredCount,
			MaxSurge:         0,
			CurrentNodeSetID: &currentNodeSetID,
		},
		nodeSets: []nodeSet{{NodeSet: db.NodeSet{ID: currentNodeSetID}, nodes: nodes}},
	}}
}

func makeHealthyNode(name string, nodeSetID uuid.UUID) db.Node {
	return db.Node{
		Name:      name,
		NodeSetID: nodeSetID,
		Conditions: db.NodeConditions{
			Pending:         db.Condition{Status: db.ConditionStatusFalse},
			KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
			Provisioned:     db.Condition{Status: db.ConditionStatusTrue},
			NodeAgentReady:  db.Condition{Status: db.ConditionStatusTrue},
			Deleting:        db.Condition{Status: db.ConditionStatusFalse},
		},
	}
}

func makeNodeToRemove(nodeSetID uuid.UUID) db.Node {
	return db.Node{
		ID:        uuid.New(),
		Name:      "node-to-remove",
		NodeSetID: nodeSetID,
		Conditions: db.NodeConditions{
			Pending:            db.Condition{Status: db.ConditionStatusFalse},
			KubernetesReady:    db.Condition{Status: db.ConditionStatusTrue},
			Provisioned:        db.Condition{Status: db.ConditionStatusTrue},
			NodeAgentReady:     db.Condition{Status: db.ConditionStatusTrue},
			MarkedForRecycling: db.Condition{Status: db.ConditionStatusTrue},
			Deleting:           db.Condition{Status: db.ConditionStatusFalse},
		},
	}
}
