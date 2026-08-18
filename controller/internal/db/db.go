// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"errors"

	"github.com/uptrace/bun"
)

var ErrIntegrityConstraintViolation = errors.New("integrity constraint violation")

type DB interface {
	Queries
	RunInTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error
	Ping(ctx context.Context) error // Verifies database connectivity; used by /readyz.
}

type Tx interface {
	Queries
}

type Queries interface {
	SiteQueries
	ClusterQueries
	ControlPlaneConfigurationQueries
	NodePoolQueries
	NodeQueries
	WorkflowOutboxQueries
	NodeSetQueries
}

func NewDB(db *bun.DB) *dbImpl {
	return &dbImpl{
		db:          db,
		queriesImpl: queriesImpl{db: db},
	}
}

type dbImpl struct {
	db *bun.DB
	queriesImpl
}

func (d *dbImpl) RunInTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error {
	return d.db.RunInTx(ctx, nil, func(ctx context.Context, bunTx bun.Tx) error {
		tx := &txImpl{
			tx:          &bunTx,
			queriesImpl: queriesImpl{db: bunTx},
		}
		return fn(ctx, tx)
	})
}

func (d *dbImpl) Ping(ctx context.Context) error {
	return d.db.PingContext(ctx)
}

type txImpl struct {
	tx *bun.Tx
	queriesImpl
}

type queriesImpl struct {
	db bun.IDB
}
