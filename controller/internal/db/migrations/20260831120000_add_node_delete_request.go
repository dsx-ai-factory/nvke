// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

//nolint:gochecknoinits // this is the typical way to register bun migrations
func init() {
	migrations.MustRegister(addNodeDeleteRequest, func(ctx context.Context, db *bun.DB) error {
		return fmt.Errorf("down migration not supported")
	})
}

// addNodeDeleteRequest adds the column that carries the TeardownNode input of a
// control-plane delete that the API accepted but has not started yet. The
// control-plane controller reads it back when it promotes the request into an
// actual teardown, so an operator's forceUnreachable / etcd overrides /
// machine-health-issue survive the hand-off. NULL on every other delete path.
func addNodeDeleteRequest(ctx context.Context, db *bun.DB) error {
	if _, err := db.ExecContext(ctx, `ALTER TABLE nodes ADD COLUMN IF NOT EXISTS delete_request jsonb`); err != nil {
		return fmt.Errorf("failed to add nodes.delete_request column: %w", err)
	}
	return nil
}
