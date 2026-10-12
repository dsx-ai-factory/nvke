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
	NodeID            uuid.UUID
	ConfigProfile     string
	ManagedByNodePool bool // True when the node is managed by a node pool (has a non-nil node pool ID).
}

func (a *Activities) GetNodeProvisioningContext(ctx context.Context, nodeID uuid.UUID) (NodeProvisioningContext, error) {
	node, err := a.db.GetNode(ctx, nodeID)
	if err != nil {
		return NodeProvisioningContext{}, wrapProvisioningContextLookupError("node", err)
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
		NodeID:            node.ID,
		ConfigProfile:     configProfile,
		ManagedByNodePool: node.IsManagedByNodePool(),
	}, nil
}

func wrapProvisioningContextLookupError(resource string, err error) error {
	if errors.Is(err, db.ErrNotFound) {
		return temporal.NewNonRetryableApplicationError(fmt.Sprintf("%s not found in database", resource), "db.ErrNotFound", err)
	}

	return fmt.Errorf("failed to get %s: %w", resource, err)
}
