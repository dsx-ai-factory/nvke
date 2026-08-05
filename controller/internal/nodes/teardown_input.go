// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

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
