// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"database/sql"
	"errors"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

// TenantQueries look up the tenant that owns a cluster, node pool, node set or
// node. A NULL clusters.tenant means the cluster is unowned, and the lookup
// returns "". Each lookup returns ErrNotFound when the object doesn't exist.
type TenantQueries interface {
	GetClusterTenant(ctx context.Context, id uuid.UUID) (string, error)
	GetNodePoolTenant(ctx context.Context, id uuid.UUID) (string, error)
	GetNodeSetTenant(ctx context.Context, id uuid.UUID) (string, error)
	GetNodeTenant(ctx context.Context, id uuid.UUID) (string, error)
}

// whereTenant limits query to rows whose cluster, aliased alias, tenant owns.
// A nil tenant leaves query unchanged. An empty tenant is an error, and an
// unowned cluster matches no tenant.
func whereTenant(query *bun.SelectQuery, alias string, tenant *string) (*bun.SelectQuery, error) {
	if tenant == nil {
		return query, nil
	}
	if *tenant == "" {
		return nil, errors.New("tenant filter is empty")
	}
	return query.Where("?.tenant = ?", bun.Ident(alias), *tenant), nil
}

func (q *queriesImpl) GetClusterTenant(ctx context.Context, id uuid.UUID) (string, error) {
	return scanTenant(ctx, q.db.NewSelect().TableExpr("clusters AS c").Where("c.id = ?", id))
}

func (q *queriesImpl) GetNodePoolTenant(ctx context.Context, id uuid.UUID) (string, error) {
	return scanTenant(ctx, q.db.NewSelect().TableExpr("node_pools AS np").
		Join("JOIN clusters c ON c.id = np.cluster_id").
		Where("np.id = ?", id))
}

func (q *queriesImpl) GetNodeSetTenant(ctx context.Context, id uuid.UUID) (string, error) {
	return scanTenant(ctx, q.db.NewSelect().TableExpr("node_sets AS ns").
		Join("JOIN node_pools np ON np.id = ns.node_pool_id").
		Join("JOIN clusters c ON c.id = np.cluster_id").
		Where("ns.id = ?", id))
}

func (q *queriesImpl) GetNodeTenant(ctx context.Context, id uuid.UUID) (string, error) {
	return scanTenant(ctx, q.db.NewSelect().TableExpr("nodes AS n").
		Join("JOIN clusters c ON c.id = n.cluster_id").
		Where("n.id = ?", id))
}

// scanTenant reads the tenant of the cluster aliased c, or "" when it is NULL.
// It returns ErrNotFound when no row matches.
func scanTenant(ctx context.Context, query *bun.SelectQuery) (string, error) {
	var tenant sql.NullString
	err := query.ColumnExpr("c.tenant").Scan(ctx, &tenant)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return tenant.String, err
}
