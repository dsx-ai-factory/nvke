// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		// Make control_plane_vip column nullable to support clusters without Kube-VIP
		_, err := db.ExecContext(ctx, "ALTER TABLE clusters ALTER COLUMN control_plane_vip DROP NOT NULL")
		return err
	}, func(ctx context.Context, db *bun.DB) error {
		return fmt.Errorf("down migration not implemented")
	})
}
