// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

func init() { //nolint:gochecknoinits // Bun registers migrations at package initialization.
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		return db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			statements := []string{
				`ALTER TABLE nodes
					ADD COLUMN spire_attestation_method text NOT NULL DEFAULT 'tpm',
					ADD COLUMN node_agent_external_mtls_enabled boolean NOT NULL DEFAULT false,
					ADD COLUMN node_agent_tls_jwt_enabled boolean NOT NULL DEFAULT false`,
				`UPDATE nodes AS n SET spire_attestation_method = 'joinToken'
					FROM node_sets AS ns
					WHERE n.node_set_id = ns.id AND ns.capi->>'spire_attestation_method' = 'joinToken'`,
				`ALTER TABLE nodes ADD CONSTRAINT nodes_spire_attestation_method_valid
					CHECK (spire_attestation_method IN ('tpm', 'joinToken', 'dsxImds'))`,
				`ALTER TABLE nodes ADD CONSTRAINT nodes_spire_attestation_transport_valid
					CHECK (spire_attestation_method = 'tpm' OR spiffe_enabled)`,
				`ALTER TABLE nodes ADD CONSTRAINT nodes_tls_jwt_transport_valid
					CHECK (NOT node_agent_tls_jwt_enabled OR spiffe_enabled OR node_agent_external_mtls_enabled)`,
				`ALTER TABLE nodes ALTER COLUMN spire_attestation_method DROP DEFAULT`,
			}
			for _, statement := range statements {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return fmt.Errorf("pin node agent security: %w", err)
				}
			}
			return nil
		})
	}, func(context.Context, *bun.DB) error {
		return fmt.Errorf("rollback not supported")
	})
}
