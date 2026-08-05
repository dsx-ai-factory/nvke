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

	OSProfile            string      `bun:"type:text,notnull"`
	ConfigProfile        string      `bun:"type:text,notnull"`
	KubernetesVersion    string      `bun:"type:text,notnull,default:''"`
	SystemReservedMemory string      `bun:"type:text,notnull,default:'16Gi'"`
	SubnetIDs            []uuid.UUID `bun:"subnet_ids,type:uuid[],notnull"`
	VpcPrefixIDs         []uuid.UUID `bun:"vpc_prefix_ids,type:uuid[],notnull"`

	CreatedAt       time.Time `bun:"type:timestamptz,notnull,default:current_timestamp"`
	UpdatedAt       time.Time `bun:"type:timestamptz,notnull,default:current_timestamp"`
	TeleportEnabled bool      `bun:"type:boolean,notnull,default:false"`

	// DiskSelectionStrategy is the strategy to use for selecting the OS install disk to use for the nodes in this NodeSet.
	DiskSelectionStrategy DiskSelectionStrategy `bun:"type:text,notnull"`

	CapiInfraMachineTemplate   json.RawMessage        `bun:"type:jsonb,notnull,default:'{}'"`
	CapiBootstrapFiles         []CloudInitFile        `bun:"capi_bootstrap_files,type:jsonb,notnull,default:'[]'"`
	CapiPreKubeadmCommands     []string               `bun:"capi_pre_kubeadm_commands,type:text[],notnull,default:'{}'"`
	CapiSpireAttestationMethod SpireAttestationMethod `bun:"capi_spire_attestation_method,type:text,notnull,default:'tpm'"`
}

func (n *NodeSet) BeforeUpdate() error {
	n.UpdatedAt = time.Now()
	return nil
}

type NodeSetFilters struct {
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
		ColumnExpr("node_set.*").
		Join("JOIN node_pools np ON np.id = node_set.node_pool_id").
		Join("JOIN clusters c ON c.id = np.cluster_id")

	if filters.NodePoolID != nil {
		query = query.Where("node_set.node_pool_id = ?", filters.NodePoolID)
	}

	if filters.SiteID != nil {
		query = query.Where("c.site_id = ?", filters.SiteID)
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
	if nodeSet.CapiPreKubeadmCommands == nil {
		nodeSet.CapiPreKubeadmCommands = []string{}
	}

	_, err := q.db.NewInsert().Model(nodeSet).Exec(ctx)
	if err != nil {
		return NodeSet{}, err
	}
	return *nodeSet, nil
}

func (q *queriesImpl) UpdateNodeSet(ctx context.Context, nodeSet *NodeSet) (NodeSet, error) {
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
