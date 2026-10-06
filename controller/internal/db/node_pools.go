// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
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
	SpireAttestationMethodDSXIMDS   SpireAttestationMethod = "dsxImds"
)

type NicoNodePoolProvider struct {
	SubnetIDs                []uuid.UUID           `json:"subnet_ids,omitempty"`
	VpcPrefixIDs             []uuid.UUID           `json:"vpc_prefix_ids,omitempty"`
	InstanceTypeID           uuid.UUID             `json:"instance_type_id,omitempty"`
	InfinibandPartitionID    *uuid.UUID            `json:"infiniband_partition_id,omitempty"`
	NvLinkLogicalPartitionID *uuid.UUID            `json:"nvlink_logical_partition_id,omitempty"`
	SSHKeyGroupIDs           []uuid.UUID           `json:"ssh_key_group_ids,omitempty"`
	OSProfile                string                `json:"os_profile,omitempty"`
	ConfigProfile            string                `json:"config_profile,omitempty"`
	DiskSelectionStrategy    DiskSelectionStrategy `json:"disk_selection_strategy,omitempty"`
}

type CAPINodePoolProvider struct {
	InfraMachineTemplate   json.RawMessage        `json:"infra_machine_template,omitempty"`
	BootstrapFiles         []CloudInitFile        `json:"bootstrap_files,omitempty"`
	PreKubeadmCommands     []string               `json:"pre_kubeadm_commands,omitempty"`
	SpireAttestationMethod SpireAttestationMethod `json:"spire_attestation_method,omitempty"`
}

// Equal reports whether two NicoNodePoolProvider values represent the same configuration.
// SubnetIDs and VpcPrefixIDs are compared as sets (order-independent).
func (n NicoNodePoolProvider) Equal(other NicoNodePoolProvider) bool {
	return n.OSProfile == other.OSProfile &&
		n.ConfigProfile == other.ConfigProfile &&
		n.DiskSelectionStrategy == other.DiskSelectionStrategy &&
		uuidSlicesSetEqual(n.SubnetIDs, other.SubnetIDs) &&
		uuidSlicesSetEqual(n.VpcPrefixIDs, other.VpcPrefixIDs)
}

// Equal reports whether two CAPINodePoolProvider values represent the same configuration.
func (c CAPINodePoolProvider) Equal(other CAPINodePoolProvider) bool {
	return bytes.Equal(c.InfraMachineTemplate, other.InfraMachineTemplate) &&
		c.SpireAttestationMethod == other.SpireAttestationMethod &&
		cloudInitFilesEqual(c.BootstrapFiles, other.BootstrapFiles) &&
		slices.Equal(c.PreKubeadmCommands, other.PreKubeadmCommands)
}

func uuidSlicesSetEqual(a, b []uuid.UUID) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[uuid.UUID]struct{}, len(a))
	for _, id := range a {
		seen[id] = struct{}{}
	}
	for _, id := range b {
		if _, ok := seen[id]; !ok {
			return false
		}
	}
	return true
}

func cloudInitFilesEqual(a, b []CloudInitFile) bool {
	return slices.EqualFunc(a, b, func(x, y CloudInitFile) bool {
		return x.Path == y.Path && x.Owner == y.Owner && x.Permissions == y.Permissions && x.Encoding == y.Encoding && x.Content == y.Content
	})
}

type NodePool struct {
	ID                   uuid.UUID          `bun:"type:uuid,pk"`
	ClusterID            uuid.UUID          `bun:"type:uuid,notnull"`
	Name                 string             `bun:"type:text,notnull"`
	NodeNamePrefix       string             `bun:"type:text,notnull"`
	Description          *string            `bun:"type:text"`
	SystemReservedMemory string             `bun:"type:text,notnull,default:'16Gi'"`
	KubeReservedMemory   string             `bun:"type:text,notnull,default:''"`
	ControlPlane         bool               `bun:"type:boolean,notnull"`
	NodeLabels           map[string]string  `bun:"type:jsonb,notnull,default:'{}'"`
	NodeTaints           []NodeTaint        `bun:"type:jsonb,notnull,default:'[]'"`
	Conditions           NodePoolConditions `bun:"type:jsonb"`
	CreatedAt            time.Time          `bun:"type:timestamptz,notnull,default:current_timestamp"`
	UpdatedAt            time.Time          `bun:"type:timestamptz,notnull,default:current_timestamp"`

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

	Nico *NicoNodePoolProvider `bun:"nico,type:jsonb"`
	Capi *CAPINodePoolProvider `bun:"capi,type:jsonb"`
}

// NodeTaint is a Kubernetes taint stored as the desired set for every node in a pool.
type NodeTaint struct {
	Key    string `json:"key"`
	Value  string `json:"value,omitempty"`
	Effect string `json:"effect"`
}

// CloneStringMap returns a non-nil copy of in. A nil or empty map becomes an empty map.
func CloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return map[string]string{}
	}
	return maps.Clone(in)
}

// normalizeNodePoolCollections stores an empty object for labels and an empty
// array for taints so a nil Go value is not written as JSON null.
func normalizeNodePoolCollections(nodePool *NodePool) {
	nodePool.NodeLabels = CloneStringMap(nodePool.NodeLabels)
	if nodePool.NodeTaints == nil {
		nodePool.NodeTaints = []NodeTaint{}
	}
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

func (n *NodePool) NicoProvider() (NicoNodePoolProvider, error) {
	if n.Nico == nil {
		return NicoNodePoolProvider{}, fmt.Errorf("%w: node pool %s nico", ErrMissingProviderPayload, n.ID)
	}
	return *cloneNicoNodePoolProvider(*n.Nico), nil
}

func (n *NodePool) CAPIProvider() (CAPINodePoolProvider, error) {
	if n.Capi == nil {
		return CAPINodePoolProvider{}, fmt.Errorf("%w: node pool %s capi", ErrMissingProviderPayload, n.ID)
	}
	return *cloneCAPINodePoolProvider(*n.Capi), nil
}

// ResolveProviderType infers the node pool's provider type from which
// provider payload is set. NodePool has no persisted ProviderType field the
// way Cluster does, so this is the node-pool equivalent of
// Cluster.ResolveProviderType.
func (n *NodePool) ResolveProviderType() (ProviderType, error) {
	switch {
	case n.Nico != nil:
		return ProviderTypeNico, nil
	case n.Capi != nil:
		return ProviderTypeCAPI, nil
	default:
		return "", fmt.Errorf("%w: node pool %s has no provider payload set", ErrMissingProviderPayload, n.ID)
	}
}

func (n *NodePool) SetNico(provider NicoNodePoolProvider) {
	n.Nico = cloneNicoNodePoolProvider(provider)
	n.Capi = nil
}

func (n *NodePool) SetCapi(provider CAPINodePoolProvider) {
	n.Capi = cloneCAPINodePoolProvider(provider)
	n.Nico = nil
}

func cloneNicoNodePoolProvider(provider NicoNodePoolProvider) *NicoNodePoolProvider {
	return &NicoNodePoolProvider{
		SubnetIDs:                append([]uuid.UUID(nil), provider.SubnetIDs...),
		VpcPrefixIDs:             append([]uuid.UUID(nil), provider.VpcPrefixIDs...),
		InstanceTypeID:           provider.InstanceTypeID,
		InfinibandPartitionID:    cloneUUIDPtr(provider.InfinibandPartitionID),
		NvLinkLogicalPartitionID: cloneUUIDPtr(provider.NvLinkLogicalPartitionID),
		SSHKeyGroupIDs:           append([]uuid.UUID(nil), provider.SSHKeyGroupIDs...),
		OSProfile:                provider.OSProfile,
		ConfigProfile:            provider.ConfigProfile,
		DiskSelectionStrategy:    provider.DiskSelectionStrategy,
	}
}

func cloneCAPINodePoolProvider(provider CAPINodePoolProvider) *CAPINodePoolProvider {
	return &CAPINodePoolProvider{
		InfraMachineTemplate:   cloneRawMessage(provider.InfraMachineTemplate),
		BootstrapFiles:         append([]CloudInitFile(nil), provider.BootstrapFiles...),
		PreKubeadmCommands:     append([]string(nil), provider.PreKubeadmCommands...),
		SpireAttestationMethod: provider.SpireAttestationMethod,
	}
}

func cloneUUIDPtr(id *uuid.UUID) *uuid.UUID {
	if id == nil {
		return nil
	}
	clone := *id
	return &clone
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
		query = query.Where("c.nico->>'site_id' = ?", filters.SiteID.String())
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
	normalizeNodePoolCollections(nodePool)
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
	normalizeNodePoolCollections(nodePool)
	nodePool.UpdatedAt = time.Now()
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
