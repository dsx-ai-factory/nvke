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
		// Drop asn_allocations table
		if _, err := db.Exec("DROP TABLE IF EXISTS asn_allocations"); err != nil {
			return err
		}

		// Drop asn column from nodes table
		if _, err := db.Exec("ALTER TABLE nodes DROP COLUMN IF EXISTS asn"); err != nil {
			return err
		}

		// Drop asn_range column from sites table
		if _, err := db.Exec("ALTER TABLE sites DROP COLUMN IF EXISTS asn_range"); err != nil {
			return err
		}

		return nil
	}, func(ctx context.Context, db *bun.DB) error {
		return fmt.Errorf("down migration not implemented")
	})
}
