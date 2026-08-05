// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package cwe

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/rollout"
)

// Loader builds a rollout.Cluster from CWE's inventory. It implements the
// rollout executor's loader contract:
//
//	LoadCluster(ctx context.Context, clusterID uuid.UUID) (rollout.Cluster, error)
type Loader struct {
	db  db.DB
	cfg config.Config
}

// NewLoader returns a Loader over the CWE database. cfg supplies NodeAgentPort,
// which each node's AgentEndpoint is built from.
func NewLoader(database db.DB, cfg config.Config) *Loader {
	return &Loader{db: database, cfg: cfg}
}

// LoadCluster returns a rollout.Cluster for clusterID: the cluster row, its
// control-plane nodes, and all of its nodes. The control-plane filter is
// COALESCE(np.control_plane, node.control_plane), so it captures both
// nodepool-managed and no-nodepool cluster-level control-plane nodes.
func (l *Loader) LoadCluster(ctx context.Context, clusterID uuid.UUID) (rollout.Cluster, error) {
	cluster, nodes, cpNodes, err := l.LoadClusterData(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	return NewCluster(cluster, nodes, cpNodes, l.cfg.NodeAgentPort), nil
}

// LoadClusterData returns clusterID's serializable inventory: the cluster row,
// all of its nodes, and its control-plane subset. It backs the workflow-side
// load activity, which cannot ship a rollout.Cluster (an interface) across the
// Temporal activity boundary, so the raw rows travel and the workflow rewraps
// them via NewCluster. Same COALESCE(np.control_plane, node.control_plane)
// control-plane filter as LoadCluster.
func (l *Loader) LoadClusterData(ctx context.Context, clusterID uuid.UUID) (db.Cluster, []db.Node, []db.Node, error) {
	cluster, err := l.db.GetCluster(ctx, clusterID)
	if err != nil {
		return db.Cluster{}, nil, nil, fmt.Errorf("get cluster %s: %w", clusterID, err)
	}

	controlPlane := true
	cpNodes, err := l.db.ListNodes(ctx, db.NodeFilters{
		ClusterID:    &clusterID,
		ControlPlane: &controlPlane,
	})
	if err != nil {
		return db.Cluster{}, nil, nil, fmt.Errorf("list control-plane nodes for cluster %s: %w", clusterID, err)
	}

	nodes, err := l.db.ListNodes(ctx, db.NodeFilters{ClusterID: &clusterID})
	if err != nil {
		return db.Cluster{}, nil, nil, fmt.Errorf("list nodes for cluster %s: %w", clusterID, err)
	}

	return cluster, nodes, cpNodes, nil
}
