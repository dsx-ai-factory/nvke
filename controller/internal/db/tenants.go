// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

// TenantQueries look up the tenant that owns a cluster, node pool, node set or
// node. A NULL clusters.tenant reads as operatorTenant. Each lookup returns
// ErrNotFound when the object doesn't exist.
type TenantQueries interface {
	GetClusterTenant(ctx context.Context, id uuid.UUID, operatorTenant string) (string, error)
	GetNodePoolTenant(ctx context.Context, id uuid.UUID, operatorTenant string) (string, error)
	GetNodeSetTenant(ctx context.Context, id uuid.UUID, operatorTenant string) (string, error)
	GetNodeTenant(ctx context.Context, id uuid.UUID, operatorTenant string) (string, error)
	// FillClusterTenant sets every NULL clusters.tenant to operatorTenant and
	// returns the number of clusters it changed.
	FillClusterTenant(ctx context.Context, operatorTenant string) (int64, error)
}

// TenantFilter limits a list to the objects Tenant owns.
type TenantFilter struct {
	Tenant string
	// OperatorTenant owns the clusters with a NULL clusters.tenant.
	OperatorTenant string
}

// where limits query to rows whose cluster, aliased alias, f.Tenant owns. A nil
// f leaves query unchanged.
func (f *TenantFilter) where(query *bun.SelectQuery, alias string) (*bun.SelectQuery, error) {
	if f == nil {
		return query, nil
	}
	if f.Tenant == "" || f.OperatorTenant == "" {
		return nil, errors.New("tenant filter: tenant and operator tenant must be set")
	}
	return query.Where("COALESCE(?.tenant, ?) = ?", bun.Ident(alias), f.OperatorTenant, f.Tenant), nil
}

func (q *queriesImpl) GetClusterTenant(ctx context.Context, id uuid.UUID, operatorTenant string) (string, error) {
	return scanTenant(ctx, q.db.NewSelect().TableExpr("clusters AS c").Where("c.id = ?", id), operatorTenant)
}

func (q *queriesImpl) GetNodePoolTenant(ctx context.Context, id uuid.UUID, operatorTenant string) (string, error) {
	return scanTenant(ctx, q.db.NewSelect().TableExpr("node_pools AS np").
		Join("JOIN clusters c ON c.id = np.cluster_id").
		Where("np.id = ?", id), operatorTenant)
}

func (q *queriesImpl) GetNodeSetTenant(ctx context.Context, id uuid.UUID, operatorTenant string) (string, error) {
	return scanTenant(ctx, q.db.NewSelect().TableExpr("node_sets AS ns").
		Join("JOIN node_pools np ON np.id = ns.node_pool_id").
		Join("JOIN clusters c ON c.id = np.cluster_id").
		Where("ns.id = ?", id), operatorTenant)
}

func (q *queriesImpl) GetNodeTenant(ctx context.Context, id uuid.UUID, operatorTenant string) (string, error) {
	return scanTenant(ctx, q.db.NewSelect().TableExpr("nodes AS n").
		Join("JOIN clusters c ON c.id = n.cluster_id").
		Where("n.id = ?", id), operatorTenant)
}

// scanTenant reads the tenant of the cluster aliased c. It returns ErrNotFound
// when no row matches.
func scanTenant(ctx context.Context, query *bun.SelectQuery, operatorTenant string) (string, error) {
	if operatorTenant == "" {
		return "", errors.New("look up tenant: operator tenant is empty")
	}
	var tenant string
	err := query.ColumnExpr("COALESCE(c.tenant, ?)", operatorTenant).Scan(ctx, &tenant)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return tenant, err
}

func (q *queriesImpl) FillClusterTenant(ctx context.Context, operatorTenant string) (int64, error) {
	if operatorTenant == "" {
		return 0, errors.New("fill clusters.tenant: operator tenant is empty")
	}
	res, err := q.db.NewUpdate().TableExpr("clusters").
		Set("tenant = ?", operatorTenant).
		Where("tenant IS NULL").
		Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("fill clusters.tenant: %w", err)
	}
	return res.RowsAffected()
}
