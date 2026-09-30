// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type DiskSelectionStrategy string

const (
	DiskSelectionStrategySmallest DiskSelectionStrategy = "smallest"
	DiskSelectionStrategyLargest  DiskSelectionStrategy = "largest"
)

type NodeSet struct {
	ID         uuid.UUID `bun:"type:uuid,pk"`
	NodePoolID uuid.UUID `bun:"type:uuid,notnull"`

	// DesiredCount is the desired count of nodes in the NodeSet.
	// The desired count is only for this NodeSet, the parent NodePool
	// contains the desired count for the entire NodePool.
	DesiredCount int `bun:"type:integer,notnull,default:0"`

	KubernetesVersion    string `bun:"type:text,notnull,default:''"`
	SystemReservedMemory string `bun:"type:text,notnull,default:'16Gi'"`
	KubeReservedMemory   string `bun:"type:text,notnull,default:''"`

	CreatedAt       time.Time `bun:"type:timestamptz,notnull,default:current_timestamp"`
	UpdatedAt       time.Time `bun:"type:timestamptz,notnull,default:current_timestamp"`
	TeleportEnabled bool      `bun:"type:boolean,notnull,default:false"`

	Nico *NicoNodePoolProvider `bun:"nico,type:jsonb"`
	Capi *CAPINodePoolProvider `bun:"capi,type:jsonb"`
}

// NodePoolNeedsNewNodeSet reports whether reconciling nodePool requires a replacement node set.
func NodePoolNeedsNewNodeSet(nodePool NodePool, current NodeSet) bool {
	if nodePool.CurrentNodeSetID == nil ||
		nodePool.KubernetesVersion != current.KubernetesVersion ||
		nodePool.SystemReservedMemory != current.SystemReservedMemory ||
		nodePool.KubeReservedMemory != current.KubeReservedMemory ||
		nodePool.TeleportEnabled != current.TeleportEnabled {
		return true
	}

	switch {
	case nodePool.Nico != nil:
		currentProvider, _ := current.NicoProvider()
		return !nodePool.Nico.Equal(currentProvider)
	case nodePool.Capi != nil:
		currentProvider, _ := current.CAPIProvider()
		return !nodePool.Capi.Equal(currentProvider)
	default:
		return true
	}
}

func (n *NodeSet) NicoProvider() (NicoNodePoolProvider, error) {
	if n.Nico == nil {
		return NicoNodePoolProvider{}, fmt.Errorf("%w: node set %s nico", ErrMissingProviderPayload, n.ID)
	}
	return *cloneNicoNodePoolProvider(*n.Nico), nil
}

func (n *NodeSet) CAPIProvider() (CAPINodePoolProvider, error) {
	if n.Capi == nil {
		return CAPINodePoolProvider{}, fmt.Errorf("%w: node set %s capi", ErrMissingProviderPayload, n.ID)
	}
	return *cloneCAPINodePoolProvider(*n.Capi), nil
}

// ResolveProviderType infers the node set's provider type from which
// provider payload is set. NodeSet has no persisted ProviderType field the
// way Cluster does, so this is the node-set equivalent of
// Cluster.ResolveProviderType.
func (n *NodeSet) ResolveProviderType() (ProviderType, error) {
	switch {
	case n.Nico != nil:
		return ProviderTypeNico, nil
	case n.Capi != nil:
		return ProviderTypeCAPI, nil
	default:
		return "", fmt.Errorf("%w: node set %s has no provider payload set", ErrMissingProviderPayload, n.ID)
	}
}

func (n *NodeSet) SetNico(provider NicoNodePoolProvider) {
	n.Nico = cloneNicoNodePoolProvider(provider)
	n.Capi = nil
}

func (n *NodeSet) SetCapi(provider CAPINodePoolProvider) {
	n.Capi = cloneCAPINodePoolProvider(provider)
	n.Nico = nil
}

type NodeSetFilters struct {
	NodeSetID  *uuid.UUID
	NodePoolID *uuid.UUID
	SiteID     *uuid.UUID
}

type NodeSetQueries interface {
	ListNodeSets(ctx context.Context, filters NodeSetFilters) ([]NodeSet, error)
	GetNodeSet(ctx context.Context, id uuid.UUID) (NodeSet, error)
	CreateNodeSet(ctx context.Context, nodeSet *NodeSet) (NodeSet, error)
	UpdateNodeSet(ctx context.Context, nodeSet *NodeSet) (NodeSet, error)
	DeleteNodeSet(ctx context.Context, id uuid.UUID) error
}

func (q *queriesImpl) ListNodeSets(ctx context.Context, filters NodeSetFilters) ([]NodeSet, error) {
	var nodeSets []NodeSet
	query := q.db.NewSelect().Model(&nodeSets).
		Join("JOIN node_pools np ON np.id = node_set.node_pool_id").
		Join("JOIN clusters c ON c.id = np.cluster_id")

	if filters.NodePoolID != nil {
		query = query.Where("node_set.node_pool_id = ?", filters.NodePoolID)
	}
	if filters.NodeSetID != nil {
		query = query.Where("node_set.id = ?", filters.NodeSetID)
	}

	if filters.SiteID != nil {
		query = query.Where("c.nico->>'site_id' = ?", filters.SiteID.String())
	}

	err := query.Scan(ctx)
	return nodeSets, err
}

func (q *queriesImpl) GetNodeSet(ctx context.Context, id uuid.UUID) (NodeSet, error) {
	var config NodeSet
	err := q.db.NewSelect().
		Model(&config).
		Where("id = ?", id).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return NodeSet{}, ErrNotFound
		}
		return NodeSet{}, err
	}
	return config, nil
}

func (q *queriesImpl) CreateNodeSet(ctx context.Context, nodeSet *NodeSet) (NodeSet, error) {
	_, err := q.db.NewInsert().Model(nodeSet).Exec(ctx)
	if err != nil {
		return NodeSet{}, err
	}
	return *nodeSet, nil
}

func (q *queriesImpl) UpdateNodeSet(ctx context.Context, nodeSet *NodeSet) (NodeSet, error) {
	nodeSet.UpdatedAt = time.Now()
	res, err := q.db.NewUpdate().Model(nodeSet).WherePK().Exec(ctx)
	if err != nil {
		return NodeSet{}, err
	}
	return *nodeSet, notFoundErrIfNoRowsAffected(res)
}

func (q *queriesImpl) DeleteNodeSet(ctx context.Context, id uuid.UUID) error {
	_, err := q.db.NewDelete().Model(&NodeSet{}).Where("id = ?", id).Exec(ctx)
	return err
}
