// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"github.com/google/uuid"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// ApplyControlPlaneConfigPayloads sets nodeSet's and nodePool's NICo payload
// from snapshotCfg.Nico. nodePool additionally carries node's pinned
// instance type and the SSH key groups — fields NodeSet has no equivalent
// of.
func (*Client) ApplyControlPlaneConfigPayloads(node db.Node, snapshotCfg db.ControlPlaneConfigSpec, nodeSet *db.NodeSet, nodePool *db.NodePool) {
	if snapshotCfg.Nico == nil {
		return
	}

	// Control-plane nodes default to the smallest disk unless the snapshot
	// pins a different strategy.
	diskSelectionStrategy := db.DiskSelectionStrategySmallest
	if snapshotCfg.Nico.DiskSelectionStrategy != "" {
		diskSelectionStrategy = snapshotCfg.Nico.DiskSelectionStrategy
	}

	nodeSet.SetNico(db.NicoNodePoolProvider{
		SubnetIDs:             append([]uuid.UUID(nil), snapshotCfg.Nico.SubnetIDs...),
		VpcPrefixIDs:          append([]uuid.UUID(nil), snapshotCfg.Nico.VpcPrefixIDs...),
		Release:               snapshotCfg.Nico.Release,
		ConfigProfile:         snapshotCfg.Nico.ConfigProfile,
		DiskSelectionStrategy: diskSelectionStrategy,
	})
	nodePool.SetNico(db.NicoNodePoolProvider{
		InstanceTypeID:        node.NicoInstanceTypeID(),
		SubnetIDs:             append([]uuid.UUID(nil), snapshotCfg.Nico.SubnetIDs...),
		VpcPrefixIDs:          append([]uuid.UUID(nil), snapshotCfg.Nico.VpcPrefixIDs...),
		SSHKeyGroupIDs:        append([]uuid.UUID(nil), snapshotCfg.Nico.SSHKeyGroupIDs...),
		Release:               snapshotCfg.Nico.Release,
		ConfigProfile:         snapshotCfg.Nico.ConfigProfile,
		DiskSelectionStrategy: diskSelectionStrategy,
	})
}
