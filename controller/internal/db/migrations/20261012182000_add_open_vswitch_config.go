// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

const (
	openVSwitchNodePoolsTable = "node_pools"
	openVSwitchNodeSetsTable  = "node_sets"
)

func init() {
	migrations.MustRegister(addOpenVSwitchConfig, func(context.Context, *bun.DB) error {
		return fmt.Errorf("down migration not implemented")
	})
}

func addOpenVSwitchConfig(ctx context.Context, database *bun.DB) error {
	return database.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		for _, table := range []string{openVSwitchNodePoolsTable, openVSwitchNodeSetsTable} {
			if _, err := tx.NewAddColumn().Table(table).ColumnExpr("open_v_switch jsonb").Exec(ctx); err != nil {
				return fmt.Errorf("add Open vSwitch configuration to %s: %w", table, err)
			}
		}
		return nil
	})
}
