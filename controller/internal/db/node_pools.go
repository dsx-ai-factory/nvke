// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

const (
	ConstraintNodePoolsClusterIDNameUnique = "node_pools_cluster_id_name_unique"
)

var ErrNodePoolNameNotUniqueInCluster = errors.New("node pool name not unique in cluster")

type SpireAttestationMethod string

const (
	SpireAttestationMethodTPM       SpireAttestationMethod = "tpm"
	SpireAttestationMethodJoinToken SpireAttestationMethod = "joinToken"
)

type NodePool struct {
	ID                       uuid.UUID          `bun:"type:uuid,pk"`
	ClusterID                uuid.UUID          `bun:"type:uuid,notnull"`
	Name                     string             `bun:"type:text,notnull"`
	NodeNamePrefix           string             `bun:"type:text,notnull"`
	SubnetIDs                []uuid.UUID        `bun:"subnet_ids,type:uuid[],notnull"`
	VpcPrefixIDs             []uuid.UUID        `bun:"vpc_prefix_ids,type:uuid[],notnull"`
	InstanceTypeID           uuid.UUID          `bun:"type:uuid,notnull"`
	Description              *string            `bun:"type:text"`
	InfinibandPartitionID    *uuid.UUID         `bun:"type:uuid"`
	NvLinkLogicalPartitionID *uuid.UUID         `bun:"type:uuid"`
	SSHKeyGroupIDs           []uuid.UUID        `bun:"type:uuid[],notnull"`
	OSProfile                string             `bun:"type:text,notnull"`
	ConfigProfile            string             `bun:"type:text,notnull"`
	SystemReservedMemory     string             `bun:"type:text,notnull,default:'16Gi'"`
	ControlPlane             bool               `bun:"type:boolean,notnull"`
	NodeLabels               []string           `bun:"type:text[],notnull"`
	Conditions               NodePoolConditions `bun:"type:jsonb"`
	CreatedAt                time.Time          `bun:"type:timestamptz,notnull,default:current_timestamp"`
	UpdatedAt                time.Time          `bun:"type:timestamptz,notnull,default:current_timestamp"`

	// This is the desired Kubernetes version for this NodePool.
	// During upgrades, existing NodeSets may contain different Kubernetes versions
	// until the upgrade is complete.
	KubernetesVersion string `bun:"type:text,notnull,default:''"`

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

	// MaxSurge controls surge node provisioning during upgrades:
	// - 0: Sequential upgrade (teardown then provision)
	// - 1: Provision 1 surge node during upgrade
	// Default: 0
	MaxSurge int `bun:"type:integer,notnull,default:0"`

	// CurrentNodeSetID is the id of the current/desired/default NodeSet for this NodePool.
	// Any new nodes provisioned into the NodePool will use this config.
	// All other NodeSets for this NodePool are considered deprecated.
	CurrentNodeSetID *uuid.UUID `bun:"type:uuid"`

	// AutoModeDisabled is a flag to disable auto-mode for this NodePool.
	// This is used to prevent auto-mode for this NodePool.
	// This is useful when we want to manually provision or recycle nodes into NodePool(s).
	AutoModeDisabled bool `bun:"type:boolean,notnull,default:false"`

	// ProvisioningMonitorDisabled is a flag to disable the timeout-based provisioning monitor for this NodePool.
	// When true, NodeProvisioningMonitorController will not recycle nodes in this pool that have
	// exceeded NodeProvisioningTimeout. Operators set this to investigate stuck nodes without having
	// them torn down, and as a noise-mute switch for known-flaky pools.
	ProvisioningMonitorDisabled bool `bun:"type:boolean,notnull,default:false"`

	// RecycleBudget is the number of nodes that can be recycled at once.
	// Only apply recycle condition if (desiredCount - readyCount) < recycleBudget
	// This is used to prevent overwhelming the system with too many nodes being recycled at once.
	RecycleBudget int `bun:"type:integer,notnull,default:0"`

	TeleportEnabled bool `bun:"type:boolean,notnull,default:false"`

	// DiskSelectionStrategy is the strategy to use for selecting the OS install disk to use for the nodes in this NodePool.
	DiskSelectionStrategy DiskSelectionStrategy `bun:"type:text,notnull"`

	CapiInfraMachineTemplate   json.RawMessage        `bun:"type:jsonb,notnull,default:'{}'"`
	CapiBootstrapFiles         []CloudInitFile        `bun:"capi_bootstrap_files,type:jsonb,notnull,default:'[]'"`
	CapiPreKubeadmCommands     []string               `bun:"capi_pre_kubeadm_commands,type:text[],notnull,default:'{}'"`
	CapiSpireAttestationMethod SpireAttestationMethod `bun:"capi_spire_attestation_method,type:text,notnull,default:'tpm'"`
}

// IsCurrentNodeSet returns true if the NodeSet is the current NodeSet for the NodePool.
func (n *NodePool) IsCurrentNodeSet(nodeSetID uuid.UUID) bool {
	return n.CurrentNodeSetID != nil && *n.CurrentNodeSetID == nodeSetID
}

type NodePoolConditions struct {
	Provisioning Condition `json:"provisioning"`
	Provisioned  Condition `json:"provisioned"`
	Deleting     Condition `json:"deleting"`
}

func (n *NodePool) BeforeUpdate() error {
	n.UpdatedAt = time.Now()
	return nil
}

type NodePoolFilters struct {
	ClusterID    *uuid.UUID
	ControlPlane *bool
	SiteID       *uuid.UUID
	Name         *string
	ClusterName  *string
}

// NodePoolMetrics represents aggregated metrics for a node pool
type NodePoolMetrics struct {
	ClusterName     string    `bun:"cluster_name"`
	NodePoolName    string    `bun:"node_pool_name"`
	NodePoolID      uuid.UUID `bun:"node_pool_id"`
	DesiredCount    int       `bun:"desired_count"`
	ActualCount     int       `bun:"actual_count"`
	PendingCount    int       `bun:"pending_count"`
	DeprecatedCount int       `bun:"deprecated_count"`
}

type NodePoolQueries interface {
	ListNodePools(ctx context.Context, filters NodePoolFilters) ([]NodePool, error)
	ListNodePoolMetrics(ctx context.Context) ([]NodePoolMetrics, error)
	GetNodePool(ctx context.Context, id uuid.UUID) (NodePool, error)
	// GetNodePoolForUpdate locks the node pool row for update (SELECT ... FOR UPDATE). Use only inside a transaction.
	GetNodePoolForUpdate(ctx context.Context, id uuid.UUID) (NodePool, error)
	CreateNodePool(ctx context.Context, nodePool *NodePool) (NodePool, error)
	UpdateNodePool(ctx context.Context, nodePool *NodePool) (NodePool, error)
	DeleteNodePool(ctx context.Context, id uuid.UUID) error
}

func (q *queriesImpl) ListNodePools(ctx context.Context, filters NodePoolFilters) ([]NodePool, error) {
	var nodePools []NodePool
	query := q.db.NewSelect().
		Model(&nodePools).
		Join("JOIN clusters c ON c.id = node_pool.cluster_id")

	if filters.ClusterID != nil {
		query = query.Where("cluster_id = ?", filters.ClusterID)
	}
	if filters.ControlPlane != nil {
		query = query.Where("control_plane = ?", filters.ControlPlane)
	}
	if filters.SiteID != nil {
		query = query.Where("c.site_id = ?", filters.SiteID)
	}
	if filters.Name != nil {
		query = query.Where("node_pool.name = ?", filters.Name)
	}
	if filters.ClusterName != nil {
		query = query.Where("c.name = ?", filters.ClusterName)
	}

	err := query.Scan(ctx)
	return nodePools, err
}

// ListNodePoolMetrics returns aggregated metrics for all node pools
func (q *queriesImpl) ListNodePoolMetrics(ctx context.Context) ([]NodePoolMetrics, error) {
	var metrics []NodePoolMetrics
	err := q.db.NewRaw(`
		SELECT
			c.name as cluster_name,
			np.id as node_pool_id,
			np.name as node_pool_name,
			np.desired_count,
			COALESCE(node_counts.actual_count, 0) as actual_count,
			COALESCE(node_counts.pending_count, 0) as pending_count,
			COALESCE(node_counts.deprecated_count, 0) as deprecated_count
		FROM node_pools np
		JOIN clusters c ON c.id = np.cluster_id
		LEFT JOIN (
			SELECT
				np2.id as node_pool_id,
				COUNT(*) FILTER (WHERE COALESCE(n.conditions->'pending'->>'status', 'False') != 'True') as actual_count,
				COUNT(*) FILTER (WHERE COALESCE(n.conditions->'pending'->>'status', 'False') = 'True') as pending_count,
				COUNT(*) FILTER (WHERE ns.id != np2.current_node_set_id AND COALESCE(n.conditions->'pending'->>'status', 'False') != 'True') as deprecated_count
			FROM nodes n
			JOIN node_sets ns ON ns.id = n.node_set_id
			JOIN node_pools np2 ON np2.id = ns.node_pool_id
			GROUP BY np2.id
		) node_counts ON node_counts.node_pool_id = np.id
		ORDER BY c.name, np.name
	`).Scan(ctx, &metrics)
	if err != nil {
		return nil, err
	}
	return metrics, nil
}

func (q *queriesImpl) GetNodePool(ctx context.Context, id uuid.UUID) (NodePool, error) {
	var nodePool NodePool
	err := q.db.NewSelect().Model(&nodePool).Where("id = ?", id).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return NodePool{}, ErrNotFound
		}
		return NodePool{}, err
	}
	return nodePool, nil
}

func (q *queriesImpl) GetNodePoolForUpdate(ctx context.Context, id uuid.UUID) (NodePool, error) {
	var nodePool NodePool
	err := q.db.NewSelect().Model(&nodePool).Where("id = ?", id).For("UPDATE").Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return NodePool{}, ErrNotFound
		}
		return NodePool{}, err
	}
	return nodePool, nil
}

func (q *queriesImpl) CreateNodePool(ctx context.Context, nodePool *NodePool) (NodePool, error) {
	if nodePool.CapiPreKubeadmCommands == nil {
		nodePool.CapiPreKubeadmCommands = []string{}
	}

	if nodePool.ControlPlane {
		if err := q.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			qi := queriesImpl{db: tx}
			_, err := qi.db.NewInsert().Model(nodePool).Exec(ctx)
			if isViolationOfConstraint(err, ConstraintNodePoolsClusterIDNameUnique) {
				return ErrNodePoolNameNotUniqueInCluster
			}
			if err != nil {
				return err
			}
			if err := qi.SyncClusterControlPlaneConfigFromCPNodePools(ctx, nodePool.ClusterID); err != nil {
				return fmt.Errorf("sync cluster control plane config: %w", err)
			}
			return nil
		}); err != nil {
			return NodePool{}, err
		}
		return *nodePool, nil
	}

	_, err := q.db.NewInsert().Model(nodePool).Exec(ctx)
	if isViolationOfConstraint(err, ConstraintNodePoolsClusterIDNameUnique) {
		return NodePool{}, ErrNodePoolNameNotUniqueInCluster
	}
	if err != nil {
		return NodePool{}, err
	}
	return *nodePool, nil
}

func (q *queriesImpl) UpdateNodePool(ctx context.Context, nodePool *NodePool) (NodePool, error) {
	res, err := q.db.NewUpdate().Model(nodePool).WherePK().Exec(ctx)
	if err != nil {
		return NodePool{}, err
	}
	if err := notFoundErrIfNoRowsAffected(res); err != nil {
		return NodePool{}, err
	}

	if nodePool.ControlPlane {
		if err := q.SyncClusterControlPlaneConfigFromCPNodePools(ctx, nodePool.ClusterID); err != nil {
			return NodePool{}, fmt.Errorf("sync cluster control plane config: %w", err)
		}
	}

	return *nodePool, nil
}

func (q *queriesImpl) DeleteNodePool(ctx context.Context, id uuid.UUID) error {
	return q.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		qi := queriesImpl{db: tx}
		np, err := qi.GetNodePool(ctx, id)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil
			}
			return err
		}
		wasCP := np.ControlPlane
		clusterID := np.ClusterID

		// Remove the current node set id from the node pool.
		if _, err := qi.db.NewUpdate().Model(&NodePool{}).Where("id = ?", id).Set("current_node_set_id = NULL").Exec(ctx); err != nil {
			return err
		}

		// Cascade delete NodeSets.
		if _, err := qi.db.NewDelete().Model(&NodeSet{}).Where("node_pool_id = ?", id).Exec(ctx); err != nil {
			return err
		}

		// Delete the node pool.
		if _, err := qi.db.NewDelete().Model(&NodePool{}).Where("id = ?", id).Exec(ctx); err != nil {
			return err
		}

		if wasCP {
			return qi.SyncClusterControlPlaneConfigFromCPNodePools(ctx, clusterID)
		}
		return nil
	})
}
