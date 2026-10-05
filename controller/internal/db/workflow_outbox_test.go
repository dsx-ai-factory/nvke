// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/testutil"
)

func TestCreateWorkflowOutboxEntryStoresTraceContext(t *testing.T) {
	ctx := context.Background()
	bunDB := testutil.SetupDB(ctx, t, (*db.WorkflowOutbox)(nil))
	database := db.NewDB(bunDB)
	provider := sdktrace.NewTracerProvider()
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(ctx)) })
	requestCtx, span := provider.Tracer("test").Start(ctx, "request")
	defer span.End()

	created, err := database.CreateWorkflowOutboxEntry(requestCtx, "ProvisionCluster", "provision-cluster-1", map[string]string{"clusterID": "c1"})
	require.NoError(t, err)

	var stored db.WorkflowOutbox
	require.NoError(t, bunDB.NewSelect().Model(&stored).Where("id = ?", created.ID).Scan(ctx))
	require.Contains(t, stored.TraceContext["traceparent"], span.SpanContext().TraceID().String())
	require.Contains(t, stored.TraceContext["traceparent"], span.SpanContext().SpanID().String())
	require.NotContains(t, stored.TraceContext, "baggage")
}

func TestCreateWorkflowOutboxEntryWithoutSpanStoresNoTraceparent(t *testing.T) {
	ctx := context.Background()
	bunDB := testutil.SetupDB(ctx, t, (*db.WorkflowOutbox)(nil))
	database := db.NewDB(bunDB)

	created, err := database.CreateWorkflowOutboxEntry(ctx, "ProvisionCluster", "provision-cluster-2", map[string]string{"clusterID": "c2"})
	require.NoError(t, err)

	var stored db.WorkflowOutbox
	require.NoError(t, bunDB.NewSelect().Model(&stored).Where("id = ?", created.ID).Scan(ctx))
	require.NotContains(t, stored.TraceContext, "traceparent")
}

// Rows written before the trace_context column existed have no value there and
// must still load, so the processor can dispatch them.
func TestWorkflowOutboxRowWithoutTraceContextStillLoads(t *testing.T) {
	ctx := context.Background()
	bunDB := testutil.SetupDB(ctx, t, (*db.WorkflowOutbox)(nil))
	id := uuid.New()
	_, err := bunDB.ExecContext(ctx,
		`INSERT INTO workflow_outbox (id, workflow_type, workflow_id, workflow_input) VALUES (?, ?, ?, ?)`,
		id, "ProvisionCluster", "provision-cluster-legacy", json.RawMessage(`{"clusterID":"c3"}`))
	require.NoError(t, err)

	var stored db.WorkflowOutbox
	require.NoError(t, bunDB.NewSelect().Model(&stored).Where("id = ?", id).Scan(ctx))
	require.Nil(t, stored.TraceContext)
	require.Equal(t, "provision-cluster-legacy", stored.WorkflowID)
}
