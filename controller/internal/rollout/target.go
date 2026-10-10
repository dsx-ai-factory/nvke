// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package rollout provides the ClusterExecutor API: composable
// primitives for safely sequencing configuration operations across a
// cluster's nodes. Interfaces and value types only — the production
// executor lives in sub-package temporal; model adapters under adapters/.
package rollout

import (
	"github.com/google/uuid"

	"github.com/dsx-ai-factory/nke/controller/internal/controllerconfig"
)

// NodeRole identifies whether a Node is a control-plane node or a worker.
type NodeRole string

const (
	NodeRoleControlPlane NodeRole = "control-plane"
	NodeRoleWorker       NodeRole = "worker"
	// NodeRoleUnknown — adapter could not determine role. Adapters
	// MUST NOT silently downgrade to NodeRoleWorker (misrouting a CP
	// as a worker is dangerous for any role-sensitive decision).
	NodeRoleUnknown NodeRole = "unknown"
)

// Node is the atomic target of a per-node operation.
type Node interface {
	ID() uuid.UUID
	Name() string
	Role() NodeRole
	// AgentEndpoint returns "ip:port" for the agent gRPC connection.
	AgentEndpoint() string
}

// Cluster is the target of a cluster-scoped rollout. ControlPlaneNodes
// and Nodes MUST return nodes sorted by name — the executor's apply
// order depends on it.
type Cluster interface {
	ID() uuid.UUID
	Name() string
	KubernetesVersion() string
	ControlPlaneVIP() string
	ControlPlaneNodes() []Node
	Nodes() []Node
	OIDCProviders() []controllerconfig.OIDCProvider
}
