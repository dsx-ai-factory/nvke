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
	"fmt"

	"github.com/uptrace/bun"
)

//nolint:gochecknoinits // this is the typical way to register bun migrations
func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		// Add ID column to vip_allocations table
		if _, err := db.NewAddColumn().
			Table("vip_allocations").
			ColumnExpr("id uuid DEFAULT gen_random_uuid()").
			Exec(ctx); err != nil {
			return err
		}

		// Add ID column to asn_allocations table
		if _, err := db.NewAddColumn().
			Table("asn_allocations").
			ColumnExpr("id uuid DEFAULT gen_random_uuid()").
			Exec(ctx); err != nil {
			return err
		}

		// Populate ID values for existing rows and make them NOT NULL
		if _, err := db.Exec("UPDATE vip_allocations SET id = gen_random_uuid() WHERE id IS NULL"); err != nil {
			return err
		}
		if _, err := db.Exec("UPDATE asn_allocations SET id = gen_random_uuid() WHERE id IS NULL"); err != nil {
			return err
		}

		// Make ID columns NOT NULL
		if _, err := db.Exec("ALTER TABLE vip_allocations ALTER COLUMN id SET NOT NULL"); err != nil {
			return err
		}
		if _, err := db.Exec("ALTER TABLE asn_allocations ALTER COLUMN id SET NOT NULL"); err != nil {
			return err
		}

		// Drop old composite primary keys
		if _, err := db.Exec("ALTER TABLE vip_allocations DROP CONSTRAINT vip_allocations_pkey"); err != nil {
			return err
		}
		if _, err := db.Exec("ALTER TABLE asn_allocations DROP CONSTRAINT asn_allocations_pkey"); err != nil {
			return err
		}

		// Add new primary keys on ID columns
		if _, err := db.Exec("ALTER TABLE vip_allocations ADD PRIMARY KEY (id)"); err != nil {
			return err
		}
		if _, err := db.Exec("ALTER TABLE asn_allocations ADD PRIMARY KEY (id)"); err != nil {
			return err
		}

		// Add unique constraints to maintain uniqueness of site_id + vip/asn combinations
		if _, err := db.Exec("ALTER TABLE vip_allocations ADD CONSTRAINT vip_allocations_site_vip_unique UNIQUE (site_id, vip)"); err != nil {
			return err
		}
		if _, err := db.Exec("ALTER TABLE asn_allocations ADD CONSTRAINT asn_allocations_site_asn_unique UNIQUE (site_id, asn)"); err != nil {
			return err
		}

		// Add unique constraints to ensure only one VIP allocation per cluster and one ASN allocation per node
		//nolint:lll // file consistency
		if _, err := db.Exec("CREATE UNIQUE INDEX vip_allocations_cluster_unique ON vip_allocations (cluster_id) WHERE cluster_id IS NOT NULL"); err != nil {
			return err
		}
		//nolint:lll // file consistency
		if _, err := db.Exec("CREATE UNIQUE INDEX asn_allocations_node_unique ON asn_allocations (node_id) WHERE node_id IS NOT NULL"); err != nil {
			return err
		}

		return nil
	}, func(ctx context.Context, db *bun.DB) error {
		return fmt.Errorf("down migration not implemented")
	})
}
