// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/NVIDIA/nke/controller/internal/db"
)

//nolint:gochecknoinits // this is the typical way to register bun migrations
func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback() //nolint:errcheck // Rollback is safe to ignore: it's a no-op after successful Commit and we handle tx errors at Commit

		if _, err := tx.NewCreateTable().
			Model((*NodeSet20250812073655)(nil)).
			ForeignKey(`("node_pool_id") REFERENCES node_pools ("id")`).
			IfNotExists().
			Exec(ctx); err != nil {
			return fmt.Errorf("failed to create node_sets table: %w", err)
		}

		// Add nodes.node_set_id column
		// Without the not null constraint, which will be added after migrating the data.
		if _, err := tx.NewAddColumn().
			Table("nodes").
			ColumnExpr("node_set_id uuid").
			IfNotExists().
			Exec(ctx); err != nil {
			return fmt.Errorf("failed to add nodes.node_set_id column: %w", err)
		}

		// Add foreign key constraint to nodes.node_set_id column
		if _, err := tx.ExecContext(ctx, "ALTER TABLE nodes ADD CONSTRAINT fk_nodes_node_set_id FOREIGN KEY (node_set_id) REFERENCES node_sets(id)"); err != nil {
			return fmt.Errorf("failed to add foreign key constraint to nodes.node_set_id column: %w", err)
		}

		// Add new node_pools fields for auto-provisioning
		if _, err := tx.NewAddColumn().
			Table("node_pools").
			ColumnExpr("desired_count integer not null default 0").
			Exec(ctx); err != nil {
			return fmt.Errorf("failed to add node_pools.desired_count column: %w", err)
		}
		if _, err := tx.NewAddColumn().
			Table("node_pools").
			ColumnExpr("auto_provision_disabled boolean not null default false").
			Exec(ctx); err != nil {
			return fmt.Errorf("failed to add node_pools.auto_provision_disabled column: %w", err)
		}
		if _, err := tx.NewAddColumn().
			Table("node_pools").
			ColumnExpr("current_node_set_id uuid").
			Exec(ctx); err != nil {
			return fmt.Errorf("failed to add node_pools.current_node_set_id column: %w", err)
		}

		// Add sites.next_auto_provision_check_due column
		if _, err := tx.NewAddColumn().
			Table("sites").
			ColumnExpr("next_auto_provision_check_due timestamptz").
			Exec(ctx); err != nil {
			return fmt.Errorf("failed to add sites.next_auto_provision_check_due column: %w", err)
		}

		// Add constraint to ensure desired_count is non-negative
		if _, err := tx.ExecContext(ctx, "ALTER TABLE node_pools ADD CONSTRAINT non_negative_desired_count CHECK (desired_count >= 0)"); err != nil {
			return fmt.Errorf("failed to add non_negative_desired_count constraint to node_pools.desired_count column: %w", err)
		}

		//
		// Migrate existing NodePools -> NodePool + NodeSet(s)
		// Migrate nodes to use NodeSet.ID in addition to NodePool.ID
		//
		if err := migrateNodePools(ctx, tx); err != nil {
			return fmt.Errorf("failed to migrate node pools: %w", err)
		}

		// Add foreign key constraint to node_pools.current_node_set_id column
		if _, err := tx.ExecContext(ctx, "ALTER TABLE node_pools ADD CONSTRAINT fk_node_pools_current_node_set_id FOREIGN KEY (current_node_set_id) REFERENCES node_sets(id)"); err != nil {
			return fmt.Errorf("failed to add foreign key constraint to node_pools.current_node_set_id column: %w", err)
		}

		// Add not null constraint to nodes.node_set_id column
		if _, err := tx.ExecContext(ctx, "ALTER TABLE nodes ALTER COLUMN node_set_id SET NOT NULL"); err != nil {
			return fmt.Errorf("failed to add not null constraint to nodes.node_set_id column: %w", err)
		}

		// Drop deprecated field
		if _, err := tx.NewDropColumn().
			Table("node_pools").
			Column("deprecated").
			Exec(ctx); err != nil {
			return fmt.Errorf("failed to drop node_pools.deprecated column: %w", err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("failed to commit transaction: %w", err)
		}
		return nil
	}, func(ctx context.Context, db *bun.DB) error {
		return fmt.Errorf("down migration not implemented")
	})
}

// Defines the NodePool model as it existed before this migration.
// So that we can use it to migrate the data.
// We only need to define the fields needed for this migration.
type NodePoolPre20250812073655 struct {
	bun.BaseModel `bun:"table:node_pools"`

	ID                    uuid.UUID             `bun:"type:uuid,pk"`
	ClusterID             uuid.UUID             `bun:"type:uuid,notnull"`
	Name                  string                `bun:"type:text,notnull"`
	NodeNamePrefix        string                `bun:"type:text,notnull"`
	SubnetIDs             []uuid.UUID           `bun:"subnet_ids,type:uuid[],notnull"`
	InstanceTypeID        uuid.UUID             `bun:"type:uuid,notnull"`
	Description           *string               `bun:"type:text"`
	Deprecated            bool                  `bun:"type:boolean,notnull,default:false"`
	InfinibandPartitionID *uuid.UUID            `bun:"type:uuid"`
	SSHKeyGroupIDs        []uuid.UUID           `bun:"type:uuid[],notnull"`
	OSProfile             string                `bun:"type:text,notnull"`
	ConfigProfile         string                `bun:"type:text,notnull"`
	ControlPlane          bool                  `bun:"type:boolean,notnull"`
	NodeLabels            []string              `bun:"type:text[],notnull"`
	Conditions            db.NodePoolConditions `bun:"type:jsonb"`
	CreatedAt             time.Time             `bun:"type:timestamptz,notnull,default:current_timestamp"`
	UpdatedAt             time.Time             `bun:"type:timestamptz,notnull,default:current_timestamp"`
}

// Defines the NodePool model at the time of this migration.
type NodePool2025081207365 struct {
	bun.BaseModel `bun:"table:node_pools"`

	ID                    uuid.UUID             `bun:"type:uuid,pk"`
	ClusterID             uuid.UUID             `bun:"type:uuid,notnull"`
	Name                  string                `bun:"type:text,notnull"`
	NodeNamePrefix        string                `bun:"type:text,notnull"`
	SubnetIDs             []uuid.UUID           `bun:"subnet_ids,type:uuid[],notnull"`
	InstanceTypeID        uuid.UUID             `bun:"type:uuid,notnull"`
	Description           *string               `bun:"type:text"`
	InfinibandPartitionID *uuid.UUID            `bun:"type:uuid"`
	SSHKeyGroupIDs        []uuid.UUID           `bun:"type:uuid[],notnull"`
	OSProfile             string                `bun:"type:text,notnull"`
	ConfigProfile         string                `bun:"type:text,notnull"`
	ControlPlane          bool                  `bun:"type:boolean,notnull"`
	NodeLabels            []string              `bun:"type:text[],notnull"`
	Conditions            db.NodePoolConditions `bun:"type:jsonb"`
	CreatedAt             time.Time             `bun:"type:timestamptz,notnull,default:current_timestamp"`
	UpdatedAt             time.Time             `bun:"type:timestamptz,notnull,default:current_timestamp"`

	// DesiredCount is the desired count of nodes in the NodePool.
	// The desired count is for the entire NodePool, not just the current NodeSet.
	// Each NodeSet has it's own DesiredCount.
	// Ideally we would have a 1:1 relationship between NodePool and NodeSet.
	// But node recycling takes time, and requires user workload disruption.
	// So we intend to have constant maintenance to recycle nodes to the current NodeSet.
	// While respecting the SLO / Disruption Budget.
	DesiredCount int `bun:"type:integer,notnull,default:0"`

	// AutoProvisionDisabled is a flag to disable auto-provisioning for this NodePool.
	// This is used to prevent auto-provisioning into this NodePool.
	// This is useful when we want to manually provision or recycle nodes into NodePool(s).
	// This can also be used as an emergency stop for auto-provisioning.
	AutoProvisionDisabled bool `bun:"type:boolean,notnull,default:false"`

	// CurrentNodeSetID is the id of the current/desired/default NodeSet for this NodePool.
	// Any new nodes provisioned into the NodePool will use this config.
	// All other NodeSets for this NodePool are considered deprecated.
	CurrentNodeSetID *uuid.UUID `bun:"type:uuid"`
}

// Defines the NodeSet model at the time of this migration (initial creation).
// Defining it here allows the dbpackage model to change without having to back-port the changes to old migrations.
type NodeSet20250812073655 struct {
	bun.BaseModel `bun:"table:node_sets"`

	ID            uuid.UUID `bun:"type:uuid,pk,default:gen_random_uuid()"`
	NodePoolID    uuid.UUID `bun:"type:uuid,notnull"`
	DesiredCount  int       `bun:"type:integer,notnull,default:0"`
	OSProfile     string    `bun:"type:text,notnull"`
	ConfigProfile string    `bun:"type:text,notnull"`
	CreatedAt     time.Time `bun:"type:timestamptz,notnull,default:current_timestamp"`
	UpdatedAt     time.Time `bun:"type:timestamptz,notnull,default:current_timestamp"`
}

type nodePoolMigration struct {
	ExistingNodePool NodePoolPre20250812073655
	NewNodePool      *newNodePool
	NewNodeSet       *newNodeSet
}

func (n *nodePoolMigration) Migrate(ctx context.Context, tx bun.Tx) error {
	_, err := tx.NewInsert().
		Model(n.NewNodePool.NodePool).
		On("CONFLICT (id) DO NOTHING").
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to insert new node pool: %w", err)
	}

	for _, nodeSet := range n.NewNodePool.NodeSets {
		_, err := tx.NewInsert().
			Model(nodeSet.NodeSet).
			On("CONFLICT (id) DO NOTHING").
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("failed to insert new node set: %w", err)
		}
	}

	_, err = tx.NewUpdate().
		Table("nodes").
		Set("node_pool_id = ?", n.NewNodePool.NodePool.ID).
		Set("node_set_id = ?", n.NewNodeSet.NodeSet.ID).
		Where("node_pool_id = ?", n.ExistingNodePool.ID).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to update nodes: %w", err)
	}

	_, err = tx.NewDelete().
		Model(&n.ExistingNodePool).
		WherePK().
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to delete existing node pool: %w", err)
	}
	return nil
}

type newNodePool struct {
	NodePool *NodePool2025081207365
	NodeSets []*newNodeSet
}

type newNodeSet struct {
	// Indicates that this node set was created from a deprecated node pool.
	// We need to track this so we can correctly select the current node set for the new node pool.
	WasDeprecated bool
	NodeSet       *NodeSet20250812073655
}

// migrateNodePools migrates from the legacy NodePools to the new NodePools + NodeSets.
// Note: It does not attempt to retain any existing NodePool.ID by identifying the "current" NodePool.
// Instead creating all the newly consolidated NodePools and NodeSets and deleting the old NodePools.
//
// This consolidation reduces the number of node pools by grouping similar pools together while preserving
// configuration diversity through multiple node sets per pool.
//
// Algorithm Overview:
//  1. Validation: Ensures no node pools are currently being deleted (fails migration if found, which will be retried by k8s)
//  2. NodePool Consolidation: Groups existing node pools with similar characteristics into new consolidated pools:
//     - Same cluster, instance type, infiniband partition, SSH keys, control plane status
//     - Similar node name prefixes (ignoring trailing numbers: "w", "worker", "w2", "w33" → same pool)
//  3. NodeSet Creation: Creates node sets within consolidated pools based on configuration differences:
//     - Separate node sets for different OS profiles, config profiles, or deprecation status
//  4. Subnet Aggregation: Combines subnet IDs from all contributing legacy pools into new pools
//  5. Count Calculation: Sets initial desired count based on existing node count at migration time
//  6. Current NodeSet Selection: Identifies the "current" node set (newest non-deprecated, or newest overall)
//  7. Migration Execution: Performs database operations (insert new, update nodes, delete old)
func migrateNodePools(ctx context.Context, tx bun.Tx) error {
	// Get all existing NodePools that need migration
	var existingNodePools []NodePoolPre20250812073655
	if err := tx.NewSelect().
		Model(&existingNodePools).
		Scan(ctx, &existingNodePools); err != nil {
		return fmt.Errorf("failed to select existing node pools: %w", err)
	}

	// This is a course grained check, suitable only for the migration.
	// If any node pools are deleting, we just fail the migration, giving the currently running
	// CWE software a chance to complete the deletion, then the following migration attempt should succeed.
	for _, existingNodePool := range existingNodePools {
		if existingNodePool.Conditions.Deleting.Status == db.ConditionStatusTrue {
			panic(fmt.Sprintf("node pool %s is deleting", existingNodePool.ID))
		}
	}

	// There will be exactly one nodePoolMigration object per existing node pool.
	// Which will contain a pointer to it's new node pool and new node set.
	// The new node pool and new node set may be re-used by multiple existing node pools.
	nodePoolMigrations := []*nodePoolMigration{}

	// First create / consolidate new node pool(s) for each existing node pool.
	for _, existingNodePool := range existingNodePools {
		newNodePool := createOrConsolidateEquivalentNewNodePool(nodePoolMigrations, existingNodePool)

		nodePoolMigration := &nodePoolMigration{
			ExistingNodePool: existingNodePool,
			NewNodePool:      newNodePool,
		}
		nodePoolMigrations = append(nodePoolMigrations, nodePoolMigration)
	}

	// Then create / consolidate new node set(s) for each existing node pool.
	for _, nodePoolMigration := range nodePoolMigrations {
		nodePoolMigration.NewNodeSet = createOrConsolidateEquivalentNewNodeSet(nodePoolMigration)
	}

	// Now we have a list of every existing node pool, with a pointer to it's new node pool and new node set.

	// Map new node pools by id
	newNodePoolMap := map[uuid.UUID]*newNodePool{}
	for _, nodePoolMigration := range nodePoolMigrations {
		newNodePoolMap[nodePoolMigration.NewNodePool.NodePool.ID] = nodePoolMigration.NewNodePool
	}

	// Now consolidate the subnet ids for each new node pool.
	for _, nodePoolMigration := range nodePoolMigrations {
		for _, subnetID := range nodePoolMigration.ExistingNodePool.SubnetIDs {
			if !slices.Contains(newNodePoolMap[nodePoolMigration.NewNodePool.NodePool.ID].NodePool.SubnetIDs, subnetID) {
				newNodePoolMap[nodePoolMigration.NewNodePool.NodePool.ID].NodePool.SubnetIDs = append(newNodePoolMap[nodePoolMigration.NewNodePool.NodePool.ID].NodePool.SubnetIDs, subnetID)
			}
		}
	}

	// Then sort the subnet ids for consistency
	for _, newNodePool := range newNodePoolMap {
		sort.Slice(newNodePool.NodePool.SubnetIDs, func(i, j int) bool {
			return newNodePool.NodePool.SubnetIDs[i].String() < newNodePool.NodePool.SubnetIDs[j].String()
		})
	}

	// Calculate the desired for each new node pool.
	// The initial desired count is just based on what exists in the database at the time of the migration.
	// We should go through each site after the migration and adjust the desired counts to the RG desired count.
	for _, nodePoolMigration := range nodePoolMigrations {
		nodes := []db.Node{}
		err := tx.NewSelect().
			Model(&nodes).
			Where("node_pool_id = ?", nodePoolMigration.ExistingNodePool.ID).
			Scan(ctx, &nodes)
		if err != nil {
			return fmt.Errorf("failed to list nodes: %w", err)
		}
		newNodePoolMap[nodePoolMigration.NewNodePool.NodePool.ID].NodePool.DesiredCount += len(nodes)
	}

	// Update the new node pools with the values from the "current" node set.
	for _, newNodePool := range newNodePoolMap {
		// Sort node sets by creation date (oldest to newest)
		sort.Slice(newNodePool.NodeSets, func(i, j int) bool {
			return newNodePool.NodeSets[i].NodeSet.CreatedAt.Before(newNodePool.NodeSets[j].NodeSet.CreatedAt)
		})

		// Find the newest node set.
		latestNodeSet := newNodePool.NodeSets[len(newNodePool.NodeSets)-1]

		// Find the newest non-deprecated node set.
		var currentNodeSet *newNodeSet
		for _, nodeSet := range newNodePool.NodeSets {
			if !nodeSet.WasDeprecated {
				if currentNodeSet == nil || nodeSet.NodeSet.CreatedAt.After(currentNodeSet.NodeSet.CreatedAt) {
					currentNodeSet = nodeSet
				}
			}
		}
		if currentNodeSet == nil {
			// This will require some manual tuning up before/after the migration.
			//  - Example: az27: storage node pool has no current configuration
			currentNodeSet = latestNodeSet
		}

		// Set the os profile, config profile, and current node set id based on the current node set.
		// Note: I was just taking the newest node set, but az06 somehow has a deprecated node set that is newer than the newest non-deprecated node set.
		// So we had to add the deprecation status to the comparison.
		newNodePool.NodePool.OSProfile = currentNodeSet.NodeSet.OSProfile
		newNodePool.NodePool.ConfigProfile = currentNodeSet.NodeSet.ConfigProfile
		newNodePool.NodePool.CurrentNodeSetID = &currentNodeSet.NodeSet.ID
		newNodePool.NodePool.AutoProvisionDisabled = false
	}

	// Add Deprecated to the description of the new node pools where all node sets are deprecated.
	for _, newNodePool := range newNodePoolMap {
		allDeprecated := true
		for _, nodeSet := range newNodePool.NodeSets {
			if !nodeSet.WasDeprecated {
				allDeprecated = false
			}
		}
		if allDeprecated {
			desc := "Deprecated"
			if newNodePool.NodePool.Description != nil && *newNodePool.NodePool.Description != "" {
				desc += "\n\n" + *newNodePool.NodePool.Description
			}
			newNodePool.NodePool.Description = &desc
		}
		// We could set the desired count to zero here for deprecated node pools.
		// newNodePool.NodePool.DesiredCount = 0
	}

	// Do the actual migration
	for _, nodePoolMigration := range nodePoolMigrations {
		if err := nodePoolMigration.Migrate(ctx, tx); err != nil {
			return fmt.Errorf("failed to migrate node pool %s: %w", nodePoolMigration.ExistingNodePool.ID, err)
		}
	}

	return nil
}

func createOrConsolidateEquivalentNewNodePool(nodePoolMigrations []*nodePoolMigration, existingNodePool NodePoolPre20250812073655) *newNodePool {
	for _, nodePoolMigration := range nodePoolMigrations {
		newNodePool := nodePoolMigration.NewNodePool

		existingInfinibandPartitionID := ""
		if existingNodePool.InfinibandPartitionID != nil {
			existingInfinibandPartitionID = existingNodePool.InfinibandPartitionID.String()
		}
		newInfinibandPartitionID := ""
		if newNodePool.NodePool.InfinibandPartitionID != nil {
			newInfinibandPartitionID = newNodePool.NodePool.InfinibandPartitionID.String()
		}

		sameClusterID := existingNodePool.ClusterID == newNodePool.NodePool.ClusterID
		sameInstanceType := existingNodePool.InstanceTypeID == newNodePool.NodePool.InstanceTypeID
		sameInfinibandPartitionID := existingInfinibandPartitionID == newInfinibandPartitionID
		sameSSHKeyGroupIDs := slices.Equal(existingNodePool.SSHKeyGroupIDs, newNodePool.NodePool.SSHKeyGroupIDs)
		sameIsControlPlane := existingNodePool.ControlPlane == newNodePool.NodePool.ControlPlane

		// Compare base names, ignoring trailing numbers
		// We want e.g. "w" and "w2" to be equivalent.
		// But not "infra", and "monitoring", even if they use the same instance type.
		normalizedExisting := normalizeNodePrefix(existingNodePool.NodeNamePrefix)
		normalizedNew := normalizeNodePrefix(newNodePool.NodePool.NodeNamePrefix)
		similarNodeNamePrefix := normalizedExisting == normalizedNew

		// If the node name prefix, instance type, infiniband partition id, ssh key group ids, and control plane status are the same, we consider them equivalent.
		// The subnets for the new node pool are aggregated later from the node sets.
		// If the osProfile or configProfile is different, we will create new node sets later.
		isEquivalentNewNodePool := sameClusterID &&
			similarNodeNamePrefix &&
			sameInstanceType &&
			sameInfinibandPartitionID &&
			sameSSHKeyGroupIDs &&
			sameIsControlPlane

		if isEquivalentNewNodePool {
			// If the incoming (existing) node pool has an older created timestamp than new node pool, update the created at.
			if existingNodePool.CreatedAt.Before(newNodePool.NodePool.CreatedAt) {
				newNodePool.NodePool.CreatedAt = existingNodePool.CreatedAt
			} else {
				// If the incoming (existing) node pool has a newer created timestamp than new node pool, update the description and conditions.
				newNodePool.NodePool.Description = existingNodePool.Description
				newNodePool.NodePool.Conditions = existingNodePool.Conditions
				newNodePool.NodePool.UpdatedAt = existingNodePool.UpdatedAt
			}
			return newNodePool
		}
	}

	//
	// We didn't find an equivalent new node pool, so we need to create a new one
	//

	// Sort the existing node pool ssh key group ids and node labels to match the new node pool.
	// For consistency, and for the comparison above.
	sort.Slice(existingNodePool.SSHKeyGroupIDs, func(i, j int) bool {
		return existingNodePool.SSHKeyGroupIDs[i].String() < existingNodePool.SSHKeyGroupIDs[j].String()
	})

	sort.Slice(existingNodePool.NodeLabels, func(i, j int) bool {
		return existingNodePool.NodeLabels[i] < existingNodePool.NodeLabels[j]
	})

	uuid := uuid.New()
	np := &NodePool2025081207365{
		ID:                    uuid,
		ClusterID:             existingNodePool.ClusterID,
		Name:                  uuid.String(),
		NodeNamePrefix:        normalizeNodePrefix(existingNodePool.NodeNamePrefix),
		InstanceTypeID:        existingNodePool.InstanceTypeID,
		Description:           existingNodePool.Description,
		InfinibandPartitionID: existingNodePool.InfinibandPartitionID,
		SSHKeyGroupIDs:        existingNodePool.SSHKeyGroupIDs,
		OSProfile:             existingNodePool.OSProfile,
		ConfigProfile:         existingNodePool.ConfigProfile,
		ControlPlane:          existingNodePool.ControlPlane,
		NodeLabels:            []string{}, // NodeLabels are no longer used as this is set from config profile; there was a single node pool in az61 that had values, but they can be cleared out.
		Conditions:            existingNodePool.Conditions,
		CreatedAt:             existingNodePool.CreatedAt,
		UpdatedAt:             existingNodePool.UpdatedAt,

		// Commented out fields are set later, based on the node sets.
		// SubnetIDs:             existingNodePool.SubnetIDs,
		// DesiredCount:          0,
		// AutoProvisionDisabled: false,
		// CurrentNodeSetID:      nil,
	}

	return &newNodePool{
		NodePool: np,
		NodeSets: []*newNodeSet{},
	}
}

func createOrConsolidateEquivalentNewNodeSet(nodePoolMigration *nodePoolMigration) *newNodeSet {
	for _, newNodeSet := range nodePoolMigration.NewNodePool.NodeSets {
		sameOSProfile := nodePoolMigration.ExistingNodePool.OSProfile == newNodeSet.NodeSet.OSProfile
		sameConfigProfile := nodePoolMigration.ExistingNodePool.ConfigProfile == newNodeSet.NodeSet.ConfigProfile
		sameDeprecationStatus := nodePoolMigration.ExistingNodePool.Deprecated == newNodeSet.WasDeprecated

		// By having deprecation status in this check, this could result in otherwise equivalent node sets being considered different.
		// Which is fine, as we are more concerned with correctly selecting the current node set for the new node pool.
		// There was an example in az06 where a deprecated node pool had a newer creation time than the non-deprecated node pool.
		isEquivalentNewNodeSet := sameOSProfile && sameConfigProfile && sameDeprecationStatus

		if isEquivalentNewNodeSet {
			return newNodeSet
		}
	}

	// We didn't find an equivalent new node set, so we need to create a new one
	nns := &newNodeSet{
		WasDeprecated: nodePoolMigration.ExistingNodePool.Deprecated,
		NodeSet: &NodeSet20250812073655{
			ID:            uuid.New(),
			NodePoolID:    nodePoolMigration.NewNodePool.NodePool.ID,
			DesiredCount:  0, // Just let the nodePoolController set this later.
			OSProfile:     nodePoolMigration.ExistingNodePool.OSProfile,
			ConfigProfile: nodePoolMigration.ExistingNodePool.ConfigProfile,
			CreatedAt:     nodePoolMigration.ExistingNodePool.CreatedAt,
			UpdatedAt:     nodePoolMigration.ExistingNodePool.UpdatedAt,
		},
	}
	nodePoolMigration.NewNodePool.NodeSets = append(nodePoolMigration.NewNodePool.NodeSets, nns)
	return nns
}

// Compare base names, ignoring trailing numbers
// e.g. "w" -> "w", "w2" -> "w"
func normalizeNodePrefix(name string) string {
	knownNodePrefixSynonyms := map[string]string{
		"c":          "cp", // az02 has "c" and "cp"
		"worker":     "w",  // az06 has "w" and "worker"
		"storage-cp": "cp", // az27 has "storage-cp" to indicate using storage nodes as control plane nodes
	}

	if synonym, ok := knownNodePrefixSynonyms[name]; ok {
		return synonym
	}

	return strings.TrimRightFunc(name, func(r rune) bool {
		return r >= '0' && r <= '9'
	})
}
