// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"

	"github.com/uptrace/bun"

	dbpackage "github.com/dsx-ai-factory/nke/controller/internal/db"
)

//nolint:gochecknoinits // this is the typical way to register bun migrations
func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		if _, err := db.NewCreateIndex().
			Table("clusters").
			Index(dbpackage.ConstraintClustersNameUnique).
			Column("name").
			Unique().
			Exec(ctx); err != nil {
			return err
		}
		if _, err := db.NewCreateIndex().
			Table("node_pools").
			Index(dbpackage.ConstraintNodePoolsClusterIDNameUnique).
			Column("cluster_id", "name").
			Unique().
			Exec(ctx); err != nil {
			return err
		}
		return nil
	}, func(ctx context.Context, db *bun.DB) error {
		if _, err := db.NewDropIndex().
			Index(dbpackage.ConstraintClustersNameUnique).
			Exec(ctx); err != nil {
			return err
		}
		if _, err := db.NewDropIndex().
			Index(dbpackage.ConstraintNodePoolsClusterIDNameUnique).
			Exec(ctx); err != nil {
			return err
		}
		return nil
	})
}
