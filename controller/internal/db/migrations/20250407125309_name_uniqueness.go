// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package migrations

import (
	"context"

	"github.com/uptrace/bun"

	dbpackage "github.com/NVIDIA/nke/controller/internal/db"
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
