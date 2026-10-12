// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// controlPlaneConfigSnapshot fetches the control-plane-config snapshot pinned
// to node.ControlPlaneConfigurationID. Interpreting the snapshot's contents
// is the caller's job; this is just the DB-backed fetch only Activities can
// perform.
func (a *Activities) controlPlaneConfigSnapshot(ctx context.Context, node db.Node) (db.ControlPlaneConfigSpec, error) {
	row, err := a.db.GetControlPlaneConfiguration(ctx, node.ControlPlaneConfigurationID)
	if err != nil {
		return db.ControlPlaneConfigSpec{}, fmt.Errorf("failed to get control plane configuration %s: %w", node.ControlPlaneConfigurationID, err)
	}
	return row.Config, nil
}

// controlPlaneInstanceTypePreference returns the ordered instance type preference
// the node is pinned to. Only control plane nodes provisioned from a control plane
// configuration snapshot carry one; everything else returns an empty list so the
// caller treats rotation as a no-op.
func (a *Activities) controlPlaneInstanceTypePreference(ctx context.Context, node db.Node) ([]uuid.UUID, error) {
	if !node.ControlPlane || node.ControlPlaneConfigurationID == uuid.Nil {
		return nil, nil
	}
	snapshot, err := a.controlPlaneConfigSnapshot(ctx, node)
	if err != nil {
		return nil, err
	}
	nico, err := snapshot.NicoProvider()
	if err != nil {
		return nil, err
	}
	return nico.InstanceTypePreference, nil
}

// seedControlPlaneInstanceType fills the initial instance type for a templateless
// control-plane NicoMachine when none is resolved yet. On the Nico path the controller does
// not pre-fill/persist a type onto the node, so this seeds the first pinned
// preference; Nico then owns availability and rotateNicoMachineInstanceType walks
// the rest in place. No-op for nodes that already carry a type or that carry no
// control-plane preference. Returns the node pool, updated if a type was seeded.
func (a *Activities) seedControlPlaneInstanceType(ctx context.Context, node db.Node, nodePool db.NodePool) (db.NodePool, error) {
	nicoNodePoolProvider, err := nodePool.NicoProvider()
	if err != nil {
		return db.NodePool{}, err
	}
	if nicoNodePoolProvider.InstanceTypeID != uuid.Nil {
		return nodePool, nil
	}
	preference, err := a.controlPlaneInstanceTypePreference(ctx, node)
	if err != nil {
		return db.NodePool{}, err
	}
	if len(preference) == 0 {
		return nodePool, nil
	}
	nicoNodePoolProvider.InstanceTypeID = preference[0]
	nodePool.SetNico(nicoNodePoolProvider)
	return nodePool, nil
}
