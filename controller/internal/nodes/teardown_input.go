// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodes

import (
	"github.com/google/uuid"

	"github.com/NVIDIA/nke/controller/internal/providers/forge"
)

type MachineHealthIssue = forge.MachineHealthIssue

// TeardownNodeInput is the workflow input for TeardownNode.
type TeardownNodeInput struct {
	NodeID uuid.UUID

	// SkipRemoveFromCluster is used by whole-cluster teardown. In that path
	// there is no surviving Kubernetes or etcd cluster to update, so TeardownNode
	// skips live-cluster cleanup and only resets local node state before deleting
	// infrastructure.
	SkipRemoveFromCluster bool
	Reason                string
	ForceUnreachable      bool
	AllowQuorumLoss       bool
	AllowSingleVoter      bool

	// Tearing down a control-plane node is not allowed by default,
	// But we do want to allow deleting control-plane nodes from the API.
	AllowCPNode bool

	// If MachineHealthIssue is provided, it is non nil it signals to Forge to flag the instance for repair.
	MachineHealthIssue *MachineHealthIssue
}
