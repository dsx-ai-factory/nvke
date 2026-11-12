// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

// Copies the node pool's NVLink logical partition into the nico payload of each
// of its node sets that lacks one.
//
// A node set is compared against its pool's partition and supplies the partition
// for the nodes it creates. Node sets backfilled when the nico payload columns
// were added carry no partition key, so without this a pool with a partition
// would read as changed, get a new node set and mark every existing node as out
// of date, and its new nodes would lose the partition.
//
// Copying the pool's current value is exact: before this release the partition
// could be set only when the pool was created, so every node set of a pool was
// created with the pool's partition.
//
// Idempotent: it matches only node sets without the key whose pool has a
// partition, so a re-run finds nothing to do. The jsonb_typeof guards skip the
// NULL payloads that other providers' rows carry, and jsonb_exists is the
// function spelling of the ? operator, which bun would otherwise read as a
// placeholder.
func init() { //nolint:gochecknoinits // this is the typical way to register bun migrations
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		return backfillNodeSetNVLinkPartition(ctx, db)
	}, func(_ context.Context, _ *bun.DB) error {
		// Nothing to undo: every controller version reads and writes this key in
		// the node set payload, so leaving it in place is correct after a downgrade.
		return nil
	})
}

func backfillNodeSetNVLinkPartition(ctx context.Context, db bun.IDB) error {
	if _, err := db.ExecContext(ctx, `
		UPDATE node_sets ns
		SET nico = ns.nico || jsonb_build_object('nvlink_logical_partition_id', np.nico->'nvlink_logical_partition_id')
		FROM node_pools np
		WHERE np.id = ns.node_pool_id
		  AND jsonb_typeof(ns.nico) = 'object'
		  AND NOT jsonb_exists(ns.nico, 'nvlink_logical_partition_id')
		  AND jsonb_typeof(np.nico->'nvlink_logical_partition_id') = 'string'
	`); err != nil {
		return fmt.Errorf("copy node_pools.nico nvlink_logical_partition_id to node_sets.nico: %w", err)
	}
	return nil
}
