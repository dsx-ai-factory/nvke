// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"

	"github.com/NVIDIA/nke/controller/internal/db"
)

type NodePoolConditionsUpdate struct {
	NodePoolID uuid.UUID
	Conditions NodePoolConditions
}

type NodePoolConditions struct {
	Provisioning *ConditionUpdate `json:"provisioning,omitempty"`
	Provisioned  *ConditionUpdate `json:"provisioned,omitempty"`
	Deleting     *ConditionUpdate `json:"deleting,omitempty"`
}

// ListNodePoolIDs lists the IDs of node pools in the database, with optional filters.
func (a *Activities) ListNodePoolIDs(ctx context.Context, filters db.NodePoolFilters) ([]uuid.UUID, error) {
	nodePools, err := a.db.ListNodePools(ctx, filters)
	if err != nil {
		return nil, fmt.Errorf("failed to list node pools: %w", err)
	}

	nodePoolIDs := make([]uuid.UUID, len(nodePools))
	for i := range nodePools {
		nodePoolIDs[i] = nodePools[i].ID
	}

	return nodePoolIDs, nil
}

// UpdateNodePoolConditions updates the conditions of a node pool in the database.
func (a *Activities) UpdateNodePoolConditions(ctx context.Context, update NodePoolConditionsUpdate) error {
	return a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		// Get existing node pool
		nodePool, err := tx.GetNodePool(ctx, update.NodePoolID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				// This will not be resolved by retrying, so return a non-retryable temporal error
				return temporal.NewNonRetryableApplicationError("node pool not found in database", "db.ErrNotFound", err)
			}
			return fmt.Errorf("failed to get node pool: %w", err)
		}

		updateCondition(&nodePool.Conditions.Provisioning, update.Conditions.Provisioning)
		updateCondition(&nodePool.Conditions.Provisioned, update.Conditions.Provisioned)
		updateCondition(&nodePool.Conditions.Deleting, update.Conditions.Deleting)

		// Update node pool with merged conditions
		_, err = tx.UpdateNodePool(ctx, &nodePool)
		if err != nil {
			return fmt.Errorf("failed to update node pool conditions: %w", err)
		}

		return nil
	})
}

// DeleteNodePool deletes a node pool from the database (hard delete) and refreshes cluster
// control_plane_config in the same transaction when the pool is control-plane.
func (a *Activities) DeleteNodePool(ctx context.Context, nodePoolID uuid.UUID) error {
	if err := a.db.DeleteNodePool(ctx, nodePoolID); err != nil {
		return fmt.Errorf("failed to delete node pool from database: %w", err)
	}
	return nil
}
