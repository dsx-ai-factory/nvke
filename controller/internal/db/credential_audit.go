// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	"github.com/dsx-ai-factory/nvke/controller/internal/credentialaudit"
)

// CredentialAuditQueries exposes the durable event ledger to controllers and
// activities. Recorded evidence has no foreign key: cluster deletion retains it.
type CredentialAuditQueries interface {
	RecordCredentialAuditEvent(ctx context.Context, event credentialaudit.Event) error
	RecordCSRCredentialAuditEvent(ctx context.Context, event credentialaudit.Event, limit int, since time.Time) (bool, error)
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

// RecordCSRCredentialAuditEvent stores a CSR-derived event unless the event's
// cluster and node already have limit CSR-derived events observed since since.
// A tenant can create CSRs for a managed node at will, so the bound is counted in
// the ledger itself and survives controller restarts. It reports whether the
// event is stored, including an identical event stored earlier.
func (q *queriesImpl) RecordCSRCredentialAuditEvent(ctx context.Context, event credentialaudit.Event, limit int, since time.Time) (bool, error) {
	if err := event.Validate(); err != nil {
		return false, err
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return false, fmt.Errorf("encode credential audit event: %w", err)
	}
	stored := false
	err = q.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		// Serialize count and insert per target so concurrent reconciles cannot
		// both pass the bound.
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext(?))`,
			"credential-audit-csr/"+event.Target.ClusterID+"/"+event.Target.NodeID); err != nil {
			return fmt.Errorf("lock credential audit target: %w", err)
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM credential_audit_events WHERE event_id = ?)`, event.ID).Scan(&exists); err != nil {
			return fmt.Errorf("check credential audit event: %w", err)
		}
		if exists {
			stored = true
			return nil
		}
		// Count only the kubelet-serving events the tenant CSR controller writes.
		// Background inspections record check/observed and
		// check/observation_unavailable for the same credential and do not count.
		var count int
		err := tx.QueryRowContext(ctx, `SELECT count(*) FROM credential_audit_events
			WHERE event #>> '{target,cluster_id}' = ? AND coalesce(event #>> '{target,node_id}', '') = ?
			AND observed_at >= ? AND event->>'credential_type' = 'kubelet_serving'
			AND (event->>'action' IN ('approve', 'verify')
				OR (event->>'action' = 'check' AND event->>'reason_code' IN ('verified', 'identity_not_approved')))`,
			event.Target.ClusterID, event.Target.NodeID, since).Scan(&count)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("count credential audit events: %w", err)
		}
		if count >= limit {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO credential_audit_events (event_id, observed_at, event)
			VALUES (?, ?, ?) ON CONFLICT (event_id) DO NOTHING`, event.ID, event.Timestamp, string(encoded)); err != nil {
			return fmt.Errorf("persist credential audit event: %w", err)
		}
		stored = true
		return nil
	})
	return stored, err
}
