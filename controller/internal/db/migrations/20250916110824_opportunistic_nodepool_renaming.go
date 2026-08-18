// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// This migration renames node pools that currently have UUID-based names to more descriptive
// names using the pattern: <cluster-name>-<node-name-prefix>[-<index>]
//
// Examples:
//   - Single pool in cluster "prod" with prefix "cp" → "prod-cp"
//   - Multiple pools in cluster "test" with prefix "w" → "test-w-1", "test-w-2"
//
// Only pools with UUID names are renamed, and existing
// descriptive names are preserved to avoid data loss.
//
//nolint:gochecknoinits // this is the typical way to register bun migrations
func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("failed to begin transaction: %w", err)
		}
		defer tx.Rollback() //nolint:errcheck // Rollback is safe to ignore

		// Node pool names must be unique within a cluster, so we need to process them individually
		type clusterInfo struct {
			ID   uuid.UUID
			Name string
		}
		var clusters []clusterInfo
		err = tx.NewSelect().
			Table("clusters").
			Column("id", "name").
			Scan(ctx, &clusters)
		if err != nil {
			return fmt.Errorf("failed to fetch clusters: %w", err)
		}

		for _, cluster := range clusters {
			if err := processClusterNodePools(ctx, tx, cluster.ID, cluster.Name); err != nil {
				// Continue processing other clusters even if one fails
				_ = err
			}
		}

		return tx.Commit()
	}, func(ctx context.Context, db *bun.DB) error {
		return fmt.Errorf("down migration not implemented")
	})
}

// processClusterNodePools processes all node pools for a single cluster
func processClusterNodePools(ctx context.Context, tx bun.Tx, clusterID uuid.UUID, clusterName string) error {
	var nodePools []NodePool20250916110824
	err := tx.NewSelect().
		Model(&nodePools).
		Where("cluster_id = ?", clusterID).
		Order("created_at ASC"). // Process in creation order for consistent indexing
		Scan(ctx)
	if err != nil {
		return fmt.Errorf("failed to fetch node pools for cluster %s: %w", clusterID, err)
	}

	if len(nodePools) == 0 {
		return nil
	}

	// Calculate what renames should happen
	updates := calculateNodePoolRenames(nodePools, clusterName)
	// Perform all updates
	for _, update := range updates {
		_, err := tx.NewUpdate().
			Table("node_pools").
			Set("name = ?", update.newName).
			Where("id = ?", update.nodePoolID).
			Exec(ctx)
		if err != nil {
			// Continue with other updates even if one fails
			_ = err
		}
	}

	return nil
}

// NodePool model for this migration
type NodePool20250916110824 struct {
	bun.BaseModel `bun:"table:node_pools"`

	ID             uuid.UUID `bun:"type:uuid,pk"`
	ClusterID      uuid.UUID `bun:"type:uuid,notnull"`
	Name           string    `bun:"type:text,notnull"`
	NodeNamePrefix string    `bun:"type:text,notnull"`
	ControlPlane   bool      `bun:"type:boolean,notnull"`
	CreatedAt      time.Time `bun:"type:timestamptz,notnull,default:current_timestamp"`
}

func isUUIDName(name string) bool {
	return uuidPattern.MatchString(name)
}

// calculateNodePoolRenames determines what renames should happen for a cluster
func calculateNodePoolRenames(nodePools []NodePool20250916110824, clusterName string) []struct {
	nodePoolID uuid.UUID
	newName    string
} {
	// Build map of existing names to avoid collisions
	nameTracker := make(map[string]bool)
	for _, np := range nodePools {
		nameTracker[np.Name] = true
	}

	// Group node pools by prefix for indexing
	prefixGroups := make(map[string][]NodePool20250916110824)
	for _, np := range nodePools {
		if isUUIDName(np.Name) {
			prefixGroups[np.NodeNamePrefix] = append(prefixGroups[np.NodeNamePrefix], np)
		}
	}

	var updates []struct {
		nodePoolID uuid.UUID
		newName    string
	}

	// Process each prefix group
	for prefix, pools := range prefixGroups {
		// Sort by ID for consistent ordering
		sort.Slice(pools, func(i, j int) bool {
			return pools[i].ID.String() < pools[j].ID.String()
		})

		// Generate names for each pool with this prefix
		for i, pool := range pools {
			var baseName string
			if len(pools) == 1 {
				// Single pool with this prefix: cluster-prefix
				baseName = fmt.Sprintf("%s-%s", clusterName, prefix)
			} else {
				// Multiple pools with this prefix: cluster-prefix-index
				baseName = fmt.Sprintf("%s-%s-%d", clusterName, prefix, i+1)
			}

			// Ensure uniqueness
			newName := baseName
			index := 2
			for nameTracker[newName] {
				newName = fmt.Sprintf("%s-%d", baseName, index)
				index++
			}

			updates = append(updates, struct {
				nodePoolID uuid.UUID
				newName    string
			}{pool.ID, newName})

			// Remove old name and add new name to tracking
			delete(nameTracker, pool.Name)
			nameTracker[newName] = true
		}
	}

	return updates
}
