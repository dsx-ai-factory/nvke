// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/nodes"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
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
				return &controlPlane{nodes: nodes, clusterControlPlaneConfig: db.ControlPlaneConfig{DesiredCount: 1}}
			}(),
			nodeToRemove:   nodeToRemove,
			expectedResult: false,
		},
		{
			name: "cannot remediate when current size is less than expected count",
			controlPlane: func() *controlPlane {
				nodes := []db.Node{nodeToRemove, makeHealthyNode("healthy-node", currentNodeSetID)}
				return &controlPlane{nodes: nodes, clusterControlPlaneConfig: db.ControlPlaneConfig{DesiredCount: 3}} // expected 3 but only have 2
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
				return &controlPlane{nodes: nodes, clusterControlPlaneConfig: db.ControlPlaneConfig{DesiredCount: 2}}
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
				return &controlPlane{nodes: nodes, clusterControlPlaneConfig: db.ControlPlaneConfig{DesiredCount: 2}}
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
				return &controlPlane{nodes: nodes, clusterControlPlaneConfig: db.ControlPlaneConfig{DesiredCount: 2}}
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
				return &controlPlane{nodes: nodes, clusterControlPlaneConfig: db.ControlPlaneConfig{DesiredCount: 3}}
			}(),
			nodeToRemove:   nodeToRemove,
			expectedResult: false,
		},
		{
			name: "can remediate with 2 healthy nodes",
			controlPlane: func() *controlPlane {
				nodes := []db.Node{nodeToRemove, makeHealthyNode("healthy-node", currentNodeSetID)}
				return &controlPlane{nodes: nodes, clusterControlPlaneConfig: db.ControlPlaneConfig{DesiredCount: 2}}
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
				return &controlPlane{nodes: nodes, clusterControlPlaneConfig: db.ControlPlaneConfig{DesiredCount: 3}}
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
				return &controlPlane{nodes: nodes, clusterControlPlaneConfig: db.ControlPlaneConfig{DesiredCount: 5}}
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
				return &controlPlane{nodes: nodes, clusterControlPlaneConfig: db.ControlPlaneConfig{DesiredCount: 5}}
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
				return &controlPlane{nodes: nodes, clusterControlPlaneConfig: db.ControlPlaneConfig{DesiredCount: 3}}
			}(),
			nodeToRemove:   nodeToRemove,
			expectedResult: true, // pending node not counted, 1 healthy member remaining, quorum=1
		},
		{
			name: "node to remove not counted in quorum calculation",
			controlPlane: func() *controlPlane {
				nodes := []db.Node{nodeToRemove, makeHealthyNode("healthy-node", currentNodeSetID)}
				return &controlPlane{nodes: nodes, clusterControlPlaneConfig: db.ControlPlaneConfig{DesiredCount: 2}}
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
					nodes:                     nodes,
					clusterControlPlaneConfig: db.ControlPlaneConfig{DesiredCount: 3},
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
					nodes:                     nodes,
					clusterControlPlaneConfig: db.ControlPlaneConfig{DesiredCount: 3},
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
					nodes:                     nodes,
					clusterControlPlaneConfig: db.ControlPlaneConfig{DesiredCount: 3},
				}
			}(),
			nodeToRemove:   nodeToRemove,
			expectedResult: true, // 4 nodes but expected 3
		},
		{
			name: "accounts for surge when nodes need rollout",
			controlPlane: func() *controlPlane {
				staleID, currentID := uuid.New(), uuid.New()
				currentSpec := nicoCPSpec("nvke-1.34.2", "", "1.34.2")
				return &controlPlane{
					nodes: []db.Node{
						nodeToRemove,
						makeCPControllerNode("current-1", currentID),
						makeCPControllerNode("stale-1", staleID),
					},
					nodePinnedControlPlaneConfigs: map[uuid.UUID]db.ControlPlaneConfigSpec{staleID: staleCPSpec(t, currentSpec), currentID: currentSpec},
					clusterControlPlaneConfig:     clusterWithCurrentSnapshot(currentSpec, 1).ControlPlaneConfig,
				}
			}(),
			nodeToRemove:   nodeToRemove,
			expectedResult: false, // 3 nodes but expected 4 (3 + 1 surge)
		},
		{
			name: "can remediate during rollout when size equals expected with surge",
			controlPlane: func() *controlPlane {
				staleID, currentID := uuid.New(), uuid.New()
				currentSpec := nicoCPSpec("nvke-1.34.2", "", "1.34.2")
				return &controlPlane{
					nodes: []db.Node{
						nodeToRemove,
						makeCPControllerNode("current-1", currentID),
						makeCPControllerNode("current-2", currentID),
						makeCPControllerNode("stale-1", staleID),
					},
					nodePinnedControlPlaneConfigs: map[uuid.UUID]db.ControlPlaneConfigSpec{staleID: staleCPSpec(t, currentSpec), currentID: currentSpec},
					clusterControlPlaneConfig:     clusterWithCurrentSnapshot(currentSpec, 1).ControlPlaneConfig,
				}
			}(),
			nodeToRemove:   nodeToRemove,
			expectedResult: true, // 4 nodes and expected 4 (3 + 1 surge)
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
				return &controlPlane{nodes: nodes, clusterControlPlaneConfig: db.ControlPlaneConfig{DesiredCount: 2}}
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
				return &controlPlane{nodes: nodes, clusterControlPlaneConfig: db.ControlPlaneConfig{DesiredCount: 2}}
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
				return &controlPlane{nodes: nodes, clusterControlPlaneConfig: db.ControlPlaneConfig{DesiredCount: 2}}
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
				return &controlPlane{nodes: nodes, clusterControlPlaneConfig: db.ControlPlaneConfig{DesiredCount: 3}}
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

// TestRemediateMachinesRecordsIntentUnderTheController pins the path remediation
// takes: it records the delete the same way a user-initiated one does and leaves
// reconcileRequestedDeletes to start the teardown, so both kinds queue in one
// place.
func TestRemediateMachinesRecordsIntentUnderTheController(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	cluster := db.Cluster{ID: clusterID, Name: "test-cluster", ControlPlaneVIP: "10.0.0.100"}

	// Three healthy nodes, one of them marked: enough for canStartRemediation,
	// and deliberately at the quorum floor so the exemption is exercised.
	healthy := func(name string) db.Node {
		return db.Node{
			ID: uuid.New(), ClusterID: clusterID, ControlPlane: true, Name: name,
			Conditions: db.NodeConditions{
				Joined:          db.NewCondition(true, "Joined"),
				Provisioned:     db.NewCondition(true, "Provisioned"),
				KubernetesReady: db.NewCondition(true, "Ready"),
				NodeAgentReady:  db.NewCondition(true, "Ready"),
			},
		}
	}
	a, b := healthy("cp-a"), healthy("cp-b")
	marked := healthy("cp-marked")
	marked.Conditions.MarkedForRecycling = db.NewCondition(true, "Marked")

	mockTx := &testutil.MockTx{}
	mockTx.On("GetNodeForUpdate", mock.Anything, marked.ID).Return(marked, nil)
	mockTx.On("ListNodes", mock.Anything, mock.Anything).Return([]db.Node{a, b, marked}, nil).Maybe()

	var written db.Node
	mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(n *db.Node) bool {
		written = *n
		return true
	})).Return(marked, nil)

	r := &reconciler{cfg: config.Config{}, db: mockTx, logger: slog.Default()}
	cp := &controlPlane{
		nodes:                     []db.Node{a, b, marked},
		clusterControlPlaneConfig: db.ControlPlaneConfig{DesiredCount: 3},
	}

	requeue, err := r.remediateMachines(context.Background(), cluster, cp)
	require.NoError(t, err, "the quorum floor must not refuse a replacement at three nodes")
	assert.True(t, requeue)
	assert.True(t, nodes.IsDeleteRequested(written), "remediation should record intent")
	assert.False(t, nodes.IsDeleting(written), "the teardown must wait for the serialized loop")
	mockTx.AssertNotCalled(t, "CreateWorkflowOutboxEntry",
		mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestRemediateMachinesWithoutControlPlaneVIPDoesNotReplaceNode(t *testing.T) {
	t.Parallel()

	cluster := db.Cluster{ID: uuid.New(), Name: "legacy-no-vip"}
	healthy := func(name string) db.Node {
		return db.Node{
			ID: uuid.New(), ClusterID: cluster.ID, ControlPlane: true, Name: name,
			Conditions: db.NodeConditions{
				Joined:          db.NewCondition(true, "Joined"),
				Provisioned:     db.NewCondition(true, "Provisioned"),
				KubernetesReady: db.NewCondition(true, "Ready"),
				NodeAgentReady:  db.NewCondition(true, "Ready"),
			},
		}
	}
	marked := healthy("cp-0")
	marked.Conditions.MarkedForRecycling = db.NewCondition(true, "Marked")
	cp := &controlPlane{
		nodes:                     []db.Node{marked, healthy("cp-1"), healthy("cp-2")},
		clusterControlPlaneConfig: db.ControlPlaneConfig{DesiredCount: 3},
	}
	mockTx := &testutil.MockTx{}
	r := &reconciler{db: mockTx, logger: slog.Default()}

	requeue, err := r.remediateMachines(context.Background(), cluster, cp)

	require.NoError(t, err)
	assert.False(t, requeue)
	mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
	mockTx.AssertNotCalled(t, "CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}
