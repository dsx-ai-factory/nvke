// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package workflows

import (
	"github.com/google/uuid"
	"go.temporal.io/sdk/workflow"
)

// RecordNodePoolConfigChangeInput is the audit record for a node pool configuration change.
// Temporal stores this in workflow history — query via the Temporal UI or API using workflow
// type "RecordNodePoolConfigChange" and workflow ID prefix "record-node-pool-config-change-".
type RecordNodePoolConfigChangeInput struct {
	NodePoolID    uuid.UUID  `json:"nodePoolID"`
	NewNodeSetID  uuid.UUID  `json:"newNodeSetID"`
	PrevNodeSetID *uuid.UUID `json:"prevNodeSetID,omitempty"`

	OldOSProfile string `json:"oldOSProfile"`
	NewOSProfile string `json:"newOSProfile"`

	OldConfigProfile string `json:"oldConfigProfile"`
	NewConfigProfile string `json:"newConfigProfile"`

	OldKubernetesVersion string `json:"oldKubernetesVersion"`
	NewKubernetesVersion string `json:"newKubernetesVersion"`

	OldSystemReservedMemory string `json:"oldSystemReservedMemory"`
	NewSystemReservedMemory string `json:"newSystemReservedMemory"`

	OldDiskSelectionStrategy string `json:"oldDiskSelectionStrategy"`
	NewDiskSelectionStrategy string `json:"newDiskSelectionStrategy"`

	OldCapiSpireAttestationMethod string `json:"oldCapiSpireAttestationMethod"`
	NewCapiSpireAttestationMethod string `json:"newCapiSpireAttestationMethod"`

	OldTeleportEnabled bool `json:"oldTeleportEnabled"`
	NewTeleportEnabled bool `json:"newTeleportEnabled"`

	OldSubnetIDs []uuid.UUID `json:"oldSubnetIDs"`
	NewSubnetIDs []uuid.UUID `json:"newSubnetIDs"`

	OldVpcPrefixIDs []uuid.UUID `json:"oldVpcPrefixIDs"`
	NewVpcPrefixIDs []uuid.UUID `json:"newVpcPrefixIDs"`

	OldCapiPreKubeadmCommands []string `json:"oldCapiPreKubeadmCommands"`
	NewCapiPreKubeadmCommands []string `json:"newCapiPreKubeadmCommands"`

	// CapiInfraMachineTemplate and CapiBootstrapFiles are intentionally excluded: they can be
	// large (manifests, cloud-init write_files content) and are already stored in the NodeSet
	// record. Fetch the old/new NodeSet via GET /nodeSets/{id} to compare their full content.
}

// RecordNodePoolConfigChange is a no-op workflow whose sole purpose is to persist a node pool
// configuration change event in Temporal's workflow history. No activities run; the input
// itself is the audit record and is retained for the namespace's workflow retention period.
func RecordNodePoolConfigChange(ctx workflow.Context, input RecordNodePoolConfigChangeInput) error {
	workflow.GetLogger(ctx).Info("Node pool configuration changed",
		"nodePoolID", input.NodePoolID,
		"newNodeSetID", input.NewNodeSetID,
		"prevNodeSetID", input.PrevNodeSetID,
		"oldOSProfile", input.OldOSProfile,
		"newOSProfile", input.NewOSProfile,
		"oldConfigProfile", input.OldConfigProfile,
		"newConfigProfile", input.NewConfigProfile,
		"oldKubernetesVersion", input.OldKubernetesVersion,
		"newKubernetesVersion", input.NewKubernetesVersion,
	)
	return nil
}
