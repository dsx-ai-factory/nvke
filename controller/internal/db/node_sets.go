// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"database/sql"
	"encoding/json"
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

	OSProfile            string      `bun:"type:text,notnull"`
	ConfigProfile        string      `bun:"type:text,notnull"`
	KubernetesVersion    string      `bun:"type:text,notnull,default:''"`
	SystemReservedMemory string      `bun:"type:text,notnull,default:'16Gi'"`
	KubeReservedMemory   string      `bun:"type:text,notnull,default:''"`
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
	Nico                       *NicoNodePoolProvider  `bun:"nico,type:jsonb"`
	Capi                       *CAPINodePoolProvider  `bun:"capi,type:jsonb"`
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

func (n *NodeSet) SetNico(provider NicoNodePoolProvider) {
	n.Nico = cloneNicoNodePoolProvider(provider)
	n.Capi = nil
	n.SubnetIDs = append([]uuid.UUID{}, provider.SubnetIDs...)
	n.VpcPrefixIDs = append([]uuid.UUID{}, provider.VpcPrefixIDs...)
	n.OSProfile = provider.OSProfile
	n.ConfigProfile = provider.ConfigProfile
	n.DiskSelectionStrategy = provider.DiskSelectionStrategy
}

func (n *NodeSet) SetCapi(provider CAPINodePoolProvider) {
	n.Capi = cloneCAPINodePoolProvider(provider)
	n.Nico = nil
	n.CapiInfraMachineTemplate = cloneRawMessage(provider.InfraMachineTemplate)
	n.CapiBootstrapFiles = append([]CloudInitFile{}, provider.BootstrapFiles...)
	n.CapiPreKubeadmCommands = append([]string{}, provider.PreKubeadmCommands...)
	n.CapiSpireAttestationMethod = provider.SpireAttestationMethod
	if n.SubnetIDs == nil {
		n.SubnetIDs = []uuid.UUID{}
	}
	if n.VpcPrefixIDs == nil {
		n.VpcPrefixIDs = []uuid.UUID{}
	}
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
