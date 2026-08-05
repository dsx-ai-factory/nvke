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

func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		// The "joined" condition is new, and gets set as part of the node provisioning process when the node joins.
		// We have some new code that looks for the "joined" condition to determine if a node is ready to be used, so
		// it's important that we set this condition for all existing nodes that have already been provisioned.
		_, err := db.NewUpdate().
			Table("nodes").
			Set("conditions = jsonb_set(conditions, '{joined}', '{\"status\":\"True\"}')").
			Where("conditions->'provisioned'->>'status' = 'True'").
			Exec(ctx)

		return err
	}, func(ctx context.Context, db *bun.DB) error {
		return fmt.Errorf("down migration not implemented")
	})
}
