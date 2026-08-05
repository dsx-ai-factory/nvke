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
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

//nolint:gochecknoinits // this is the typical way to register bun migrations
func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		if _, err := db.NewCreateTable().
			Model((*Site20250411082410)(nil)).
			IfNotExists().
			Exec(ctx); err != nil {
			return err
		}
		if _, err := db.NewCreateTable().
			Model((*VIPAllocation20250411082410)(nil)).
			IfNotExists().
			Exec(ctx); err != nil {
			return err
		}
		if _, err := db.NewCreateTable().
			Model((*ASNAllocation20250411082410)(nil)).
			IfNotExists().
			Exec(ctx); err != nil {
			return err
		}
		if _, err := db.NewAddColumn().
			Table("nodes").
			ColumnExpr("asn bigint").
			Exec(ctx); err != nil {
			return err
		}
		return nil
	}, func(ctx context.Context, db *bun.DB) error {
		if _, err := db.NewDropTable().
			Model((*ASNAllocation20250411082410)(nil)).
			IfExists().
			Exec(ctx); err != nil {
			return err
		}
		if _, err := db.NewDropTable().
			Model((*VIPAllocation20250411082410)(nil)).
			IfExists().
			Exec(ctx); err != nil {
			return err
		}
		if _, err := db.NewDropTable().
			Model((*Site20250411082410)(nil)).
			IfExists().
			Exec(ctx); err != nil {
			return err
		}
		if _, err := db.NewDropColumn().
			Table("nodes").
			ColumnExpr("asn").
			Exec(ctx); err != nil {
			return err
		}
		return nil
	})
}

type Site20250411082410 struct {
	bun.BaseModel `bun:"table:sites"`

	ID        uuid.UUID `bun:"type:uuid,pk"`
	VIPRange  string    `bun:"type:text,notnull"`
	ASNRange  string    `bun:"type:text,notnull"`
	CreatedAt time.Time `bun:"type:timestamptz,notnull,default:current_timestamp"`
}

type VIPAllocation20250411082410 struct {
	bun.BaseModel `bun:"table:vip_allocations"`

	SiteID         uuid.UUID  `bun:"type:uuid,pk"`
	VIP            string     `bun:"type:text,pk"` // Part of composite primary key
	AllocationType string     `bun:"type:text,notnull"`
	ClusterID      *uuid.UUID `bun:"type:uuid"`
	CreatedAt      time.Time  `bun:"type:timestamptz,notnull,default:current_timestamp"`
}

type ASNAllocation20250411082410 struct {
	bun.BaseModel `bun:"table:asn_allocations"`

	SiteID         uuid.UUID  `bun:"type:uuid,pk"`
	ASN            int64      `bun:"type:bigint,pk"` // Part of composite primary key
	AllocationType string     `bun:"type:text,notnull"`
	NodeID         *uuid.UUID `bun:"type:uuid"`
	CreatedAt      time.Time  `bun:"type:timestamptz,notnull,default:current_timestamp"`
}
