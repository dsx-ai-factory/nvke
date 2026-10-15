// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/nodes"
)

type NodeProvisioningContext struct {
	NodeID                uuid.UUID
	ConfigProfile         string
	ManagedByNodePool     bool // True when the node is managed by a node pool (has a non-nil node pool ID).
	NodeAgentVersion      string
	NodeAgentUpdateMethod string
}

func (a *Activities) GetNodeProvisioningContext(ctx context.Context, nodeID uuid.UUID) (NodeProvisioningContext, error) {
	node, err := a.db.GetNode(ctx, nodeID)
	if err != nil {
		return NodeProvisioningContext{}, wrapProvisioningContextLookupError("node", err)
	}
	snapshot, err := a.nodeAgentLifecycleSnapshot(ctx, node)
	if err != nil {
		return NodeProvisioningContext{}, err
	}

	nodeSet, _, err := nodes.GetNodeContext(ctx, a.db, node)
	if err != nil {
		return NodeProvisioningContext{}, fmt.Errorf("failed to resolve node context: %w", err)
	}
	// Intentionally not routed through a provider-capability client: a
	// single nil-safe field read, the same shape as the already-exempted
	// db-layer accessors, just one layer higher. CAPI has no config-profile
	// concept to read instead. Revisit only if resolving this needs more
	// than one field or more than a nil check.
	configProfile := ""
	if nodeSet.Nico != nil {
		nicoNodeSetProvider, err := nodeSet.NicoProvider()
		if err != nil {
			return NodeProvisioningContext{}, fmt.Errorf("failed to read node set nico provider: %w", err)
		}
		configProfile = nicoNodeSetProvider.ConfigProfile
	}

	return NodeProvisioningContext{
		NodeID:                node.ID,
		ConfigProfile:         configProfile,
		ManagedByNodePool:     node.IsManagedByNodePool(),
		NodeAgentVersion:      snapshot.Version,
		NodeAgentUpdateMethod: snapshot.Method,
	}, nil
}

func wrapProvisioningContextLookupError(resource string, err error) error {
	if errors.Is(err, db.ErrNotFound) {
		return temporal.NewNonRetryableApplicationError(fmt.Sprintf("%s not found in database", resource), "db.ErrNotFound", err)
	}

	return fmt.Errorf("failed to get %s: %w", resource, err)
}

func (a *Activities) nodeAgentLifecycleSnapshot(ctx context.Context, node db.Node) (db.NodeAgentLifecycle, error) {
	if !node.IsManagedByControlPlaneController() {
		return db.NodeAgentLifecycle{}, nil
	}
	var snapshot db.NodeAgentLifecycle
	err := a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		locked, err := tx.GetNodeForUpdate(ctx, node.ID)
		if err != nil {
			return err
		}
		if locked.NodeAgentLifecycle != nil {
			snapshot = *locked.NodeAgentLifecycle
			return nil
		}
		version, err := lifecycleAgentTarget(ctx, tx, locked)
		if err != nil {
			return err
		}
		snapshot = db.NodeAgentLifecycle{Version: version, Method: a.cfg.NodeAgent.UpdateMethod}
		locked.NodeAgentLifecycle = &snapshot
		_, err = tx.UpdateNode(ctx, &locked)
		return err
	})
	return snapshot, err
}

func lifecycleAgentTarget(ctx context.Context, tx db.Tx, node db.Node) (string, error) {
	if node.ControlPlaneConfigurationID == uuid.Nil {
		cluster, err := tx.GetCluster(ctx, node.ClusterID)
		return cluster.ControlPlaneConfig.NodeAgentConfig.Version, err
	}
	configuration, err := tx.GetControlPlaneConfiguration(ctx, node.ControlPlaneConfigurationID)
	return configuration.Config.NodeAgentConfig.Version, err
}
