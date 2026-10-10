// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package controller adapts internal/db inventory so it satisfies rollout.Cluster
// and rollout.Node. It is the sole bridge from the rollout framework into
// the controller's own cluster/node model, letting the ported engine drive rollouts on
// controller-managed clusters.
package controller

import (
	"net"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/dsx-ai-factory/nke/controller/internal/controllerconfig"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/rollout"
)

var (
	_ rollout.Node    = (*NodeAdapter)(nil)
	_ rollout.Cluster = (*ClusterAdapter)(nil)
)

// NodeAdapter wraps a db.Node. nodeAgentPort is the port the agent
// listens on (config.Config.NodeAgent.Port); it is combined with the node IP
// to form the gRPC endpoint, exactly as internal/agent/client.go does.
type NodeAdapter struct {
	node          db.Node
	nodeAgentPort string
}

// NewNode wraps a db.Node with the agent port used for its endpoint.
func NewNode(node db.Node, nodeAgentPort string) *NodeAdapter {
	return &NodeAdapter{node: node, nodeAgentPort: nodeAgentPort}
}

func (a *NodeAdapter) ID() uuid.UUID { return a.node.ID }

func (a *NodeAdapter) Name() string { return a.node.Name }

// Role maps the db.Node.ControlPlane flag to a rollout.NodeRole. The controller has no
// NodePool.Role enum: the ControlPlane bool is the source of truth, and
// no-nodepool control-plane nodes carry ControlPlane=true as well.
func (a *NodeAdapter) Role() rollout.NodeRole {
	if a.node.ControlPlane {
		return rollout.NodeRoleControlPlane
	}
	return rollout.NodeRoleWorker
}

// AgentEndpoint returns "ip:port" for the agent gRPC connection, built
// the same way internal/agent/client.go builds it.
func (a *NodeAdapter) AgentEndpoint() string {
	return net.JoinHostPort(a.node.IP, a.nodeAgentPort)
}

// ClusterAdapter wraps a db.Cluster together with the node inventory the
// Loader fetched for it. cpNodes are the control-plane nodes (already filtered
// by the DB, which includes no-nodepool cluster-level CP nodes); nodes are all
// of the cluster's nodes.
type ClusterAdapter struct {
	cluster       db.Cluster
	nodes         []db.Node
	cpNodes       []db.Node
	nodeAgentPort string
}

// NewCluster wraps a db.Cluster with its node inventory. nodes is the full node
// list; cpNodes is the control-plane subset. nodeAgentPort is threaded into
// every NodeAdapter so their endpoints resolve.
func NewCluster(cluster db.Cluster, nodes, cpNodes []db.Node, nodeAgentPort string) *ClusterAdapter {
	return &ClusterAdapter{
		cluster:       cluster,
		nodes:         nodes,
		cpNodes:       cpNodes,
		nodeAgentPort: nodeAgentPort,
	}
}

func (a *ClusterAdapter) ID() uuid.UUID { return a.cluster.ID }

func (a *ClusterAdapter) Name() string { return a.cluster.Name }

func (a *ClusterAdapter) KubernetesVersion() string { return a.cluster.KubernetesVersion }

// ControlPlaneVIP returns the cluster's kube-vip control-plane VIP.
func (a *ClusterAdapter) ControlPlaneVIP() string { return a.cluster.ControlPlaneVIP }

func (a *ClusterAdapter) OIDCProviders() []controllerconfig.OIDCProvider {
	return db.ClusterOIDCProvidersToConfig(a.cluster.OIDCProviders)
}

// ControlPlaneNodes returns control-plane nodes sorted by name so the
// executor's apply order is deterministic.
func (a *ClusterAdapter) ControlPlaneNodes() []rollout.Node {
	return a.wrapSorted(a.cpNodes)
}

// Nodes returns all nodes sorted by name. Same determinism contract as
// ControlPlaneNodes.
func (a *ClusterAdapter) Nodes() []rollout.Node {
	return a.wrapSorted(a.nodes)
}

func (a *ClusterAdapter) wrapSorted(nodes []db.Node) []rollout.Node {
	out := make([]rollout.Node, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, NewNode(n, a.nodeAgentPort))
	}
	slices.SortFunc(out, func(x, y rollout.Node) int {
		return strings.Compare(x.Name(), y.Name())
	})
	return out
}
