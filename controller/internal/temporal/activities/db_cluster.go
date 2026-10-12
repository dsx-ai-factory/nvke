// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

type ClusterConditionsUpdate struct {
	ClusterID  uuid.UUID
	Conditions ClusterConditions
}

type ClusterConditions struct {
	Provisioning *ConditionUpdate `json:"provisioning,omitempty"`
	Provisioned  *ConditionUpdate `json:"provisioned,omitempty"`
	Deleting     *ConditionUpdate `json:"deleting,omitempty"`
}

// UpdateClusterConditions updates the conditions of a cluster in the database.
func (a *Activities) UpdateClusterConditions(ctx context.Context, update ClusterConditionsUpdate) error {
	return a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		// Serialize whole-row updates so other condition writers are preserved.
		cluster, err := tx.GetClusterForUpdate(ctx, update.ClusterID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				// This will not be resolved by retrying, so return a non-retryable temporal error
				return temporal.NewNonRetryableApplicationError("cluster not found in database", "db.ErrNotFound", err)
			}
			return fmt.Errorf("failed to get cluster: %w", err)
		}

		updateCondition(&cluster.Conditions.Provisioning, update.Conditions.Provisioning)
		updateCondition(&cluster.Conditions.Provisioned, update.Conditions.Provisioned)
		updateCondition(&cluster.Conditions.Deleting, update.Conditions.Deleting)

		// Update cluster with merged conditions
		_, err = tx.UpdateCluster(ctx, &cluster)
		if err != nil {
			return fmt.Errorf("failed to update cluster conditions: %w", err)
		}

		return nil
	})
}

// DeleteCluster deletes a cluster from the database.
func (a *Activities) DeleteCluster(ctx context.Context, clusterID uuid.UUID) error {
	return a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		if err := tx.DeleteCluster(ctx, clusterID); err != nil {
			return fmt.Errorf("failed to delete cluster from database: %w", err)
		}

		return nil
	})
}
