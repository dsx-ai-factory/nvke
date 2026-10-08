// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"

	"github.com/dsx-ai-factory/nvke/controller/internal/credentialaudit"
)

func TestCredentialAuditLedgerRetainsFirstRecordAcrossRetriesAndRejectsInvalid(t *testing.T) {
	sqlite, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	database := bun.NewDB(sqlite, sqlitedialect.New())
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	_, err = database.ExecContext(t.Context(), `CREATE TABLE credential_audit_events (
		event_id TEXT PRIMARY KEY, observed_at TIMESTAMP NOT NULL, event TEXT NOT NULL)`)
	require.NoError(t, err)
	queries := &queriesImpl{db: database}
	input := credentialaudit.Event{
		Timestamp: time.Now().UTC(), Action: credentialaudit.ActionRotate, Credential: credentialaudit.TypeEtcdKey,
		Plane: credentialaudit.PlaneT2, Target: credentialaudit.Target{ClusterID: uuid.NewString()},
		Actor:   credentialaudit.Actor{Kind: credentialaudit.ActorOperatorUnattributed},
		Outcome: credentialaudit.OutcomeSuccess, Reason: credentialaudit.ReasonCompleted,
		Correlation: credentialaudit.Correlation{OperationHash: credentialaudit.Hash("operation", "test-rotation")},
	}
	event, err := credentialaudit.New(input)
	require.NoError(t, err)
	require.NoError(t, queries.RecordCredentialAuditEvent(t.Context(), event))
	input.Timestamp = input.Timestamp.Add(time.Hour)
	duplicate, err := credentialaudit.New(input)
	require.NoError(t, err)
	require.Equal(t, event.ID, duplicate.ID)
	require.NoError(t, queries.RecordCredentialAuditEvent(t.Context(), duplicate))
	var count int
	require.NoError(t, database.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM credential_audit_events").Scan(&count))
	require.Equal(t, 1, count)
	var encoded string
	require.NoError(t, database.QueryRowContext(t.Context(), "SELECT event FROM credential_audit_events").Scan(&encoded))
	var stored credentialaudit.Event
	require.NoError(t, json.Unmarshal([]byte(encoded), &stored))
	require.Equal(t, event.Timestamp, stored.Timestamp)
	event.Reason = "example-token"
	require.Error(t, queries.RecordCredentialAuditEvent(t.Context(), event))
	require.NoError(t, database.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM credential_audit_events").Scan(&count))
	require.Equal(t, 1, count)
}
