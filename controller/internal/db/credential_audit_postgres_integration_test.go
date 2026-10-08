// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package db_test

import (
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/credentialaudit"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// TestCredentialAuditPostgres runs against a fresh migrated database that
// newMigratedPostgres creates on the NVKE_TEST_POSTGRES_DSN server and drops at
// cleanup.
func TestCredentialAuditPostgres(t *testing.T) {
	database := newMigratedPostgres(t.Context(), t)
	store := db.NewDB(database)
	cluster, err := store.CreateCluster(t.Context(), &db.Cluster{
		ID: uuid.New(), Name: "audit-" + uuid.NewString(), KubernetesVersion: "1.34.2",
		ProviderType: db.ProviderTypeCAPI, Capi: &db.CAPIClusterProvider{},
	})
	require.NoError(t, err)
	input := credentialaudit.Event{
		Timestamp: time.Now().UTC(), Action: credentialaudit.ActionRotate, Credential: credentialaudit.TypeEtcdKey,
		Plane: credentialaudit.PlaneT2, Target: credentialaudit.Target{ClusterID: cluster.ID.String()},
		Actor:   credentialaudit.Actor{Kind: credentialaudit.ActorOperatorUnattributed},
		Outcome: credentialaudit.OutcomeSuccess, Reason: credentialaudit.ReasonCompleted,
		Correlation: credentialaudit.Correlation{OperationHash: credentialaudit.Hash("operation", uuid.NewString())},
	}
	event, err := credentialaudit.New(input)
	require.NoError(t, err)
	require.NoError(t, store.RecordCredentialAuditEvent(t.Context(), event))
	input.Timestamp = input.Timestamp.Add(time.Hour)
	duplicate, err := credentialaudit.New(input)
	require.NoError(t, err)
	require.NoError(t, store.RecordCredentialAuditEvent(t.Context(), duplicate))
	var encoded string
	require.NoError(t, database.QueryRowContext(t.Context(), "SELECT event::text FROM credential_audit_events WHERE event_id = ?", event.ID).Scan(&encoded))
	var stored credentialaudit.Event
	require.NoError(t, json.Unmarshal([]byte(encoded), &stored))
	require.Equal(t, event.Timestamp, stored.Timestamp)
	var count int
	require.NoError(t, database.QueryRowContext(t.Context(), "SELECT count(*) FROM credential_audit_events WHERE event_id = ?", event.ID).Scan(&count))
	require.Equal(t, 1, count)
	// Committed evidence outlives the managed cluster, without a cascade or FK.
	require.NoError(t, store.DeleteCluster(t.Context(), cluster.ID))
	_, err = store.GetCluster(t.Context(), cluster.ID)
	require.ErrorIs(t, err, db.ErrNotFound)
	var retained string
	require.NoError(t, database.QueryRowContext(t.Context(), "SELECT event::text FROM credential_audit_events WHERE event_id = ?", event.ID).Scan(&retained))
	require.JSONEq(t, encoded, retained)
	// A different attempt or outcome remains distinct even after cluster deletion.
	input.Correlation.Attempt = 2
	retry, err := credentialaudit.New(input)
	require.NoError(t, err)
	require.NoError(t, store.RecordCredentialAuditEvent(t.Context(), retry))
	input.Outcome, input.Reason = credentialaudit.OutcomeFailure, credentialaudit.ReasonFailed
	failure, err := credentialaudit.New(input)
	require.NoError(t, err)
	require.NoError(t, store.RecordCredentialAuditEvent(t.Context(), failure))
	require.NoError(t, database.QueryRowContext(t.Context(), "SELECT count(*) FROM credential_audit_events WHERE event #>> '{target,cluster_id}' = ?", cluster.ID.String()).Scan(&count))
	require.Equal(t, 3, count)
	// Force a real schema-level sink failure. The recorder must report it without
	// affecting committed lifecycle tables or manufacturing a successful insert.
	_, err = database.ExecContext(t.Context(), "ALTER TABLE credential_audit_events RENAME TO credential_audit_events_unavailable")
	require.NoError(t, err)
	var observations []string
	recorder := credentialaudit.NewRecorder(store, slog.New(slog.DiscardHandler), func(outcome string) {
		observations = append(observations, outcome)
	})
	recorder.Record(t.Context(), event)
	require.Equal(t, []string{credentialaudit.WriteFailed}, observations)
}
