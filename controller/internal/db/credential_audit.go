// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/dsx-ai-factory/nvke/controller/internal/credentialaudit"
)

// CredentialAuditQueries exposes the durable event ledger to controllers and
// activities. Recorded evidence has no foreign key: cluster deletion retains it.
type CredentialAuditQueries interface {
	RecordCredentialAuditEvent(ctx context.Context, event credentialaudit.Event) error
}

func (q *queriesImpl) RecordCredentialAuditEvent(ctx context.Context, event credentialaudit.Event) error {
	if err := event.Validate(); err != nil {
		return err
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode credential audit event: %w", err)
	}
	_, err = q.db.ExecContext(ctx,
		`INSERT INTO credential_audit_events (event_id, observed_at, event)
		 VALUES (?, ?, ?) ON CONFLICT (event_id) DO NOTHING`, event.ID, event.Timestamp, string(encoded))
	if err != nil {
		return fmt.Errorf("persist credential audit event: %w", err)
	}
	return nil
}
