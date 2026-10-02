// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/uptrace/bun/extra/bunotel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// The DSNs are the shapes the controller chart renders: an inline env value
// (legacy and greenfield) and a chart-created secret with an escaped password.
func TestTracingHookDBNameFollowsDSN(t *testing.T) {
	tests := []struct {
		name string
		dsn  string
		want string
	}{
		{
			name: "legacy inline env value",
			dsn:  "postgres://postgres:postgres@cnpg-cloud-rw.cnpg-cloud.svc.cluster.local/cloud-workflow?sslmode=disable",
			want: "cloud-workflow",
		},
		{
			name: "greenfield inline env value",
			dsn:  "postgres://postgres:postgres@cnpg-cloud-rw.cnpg-cloud.svc.cluster.local/nke-controller?sslmode=disable",
			want: "nke-controller",
		},
		{
			name: "chart-created secret with escaped password",
			dsn:  "postgresql://postgres:p%40ss%2Fword@postgresql:5432/nke-controller?sslmode=require",
			want: "nke-controller",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := tracetest.NewSpanRecorder()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
			t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
			connector := pgdriver.NewConnector(pgdriver.WithDSN(tt.dsn))
			hook := newTracingHook(connector, bunotel.WithTracerProvider(tp))

			// sql.OpenDB does not dial, so no database is needed.
			bdb := bun.NewDB(sql.OpenDB(connector), pgdialect.New())
			t.Cleanup(func() { _ = bdb.Close() })
			event := &bun.QueryEvent{DB: bdb, Query: "SELECT 1", StartTime: time.Now()}
			ctx := hook.BeforeQuery(context.Background(), event)
			hook.AfterQuery(ctx, event)

			spans := recorder.Ended()
			require.Len(t, spans, 1)
			var got string
			for _, attr := range spans[0].Attributes() {
				if attr.Key == "db.name" {
					got = attr.Value.AsString()
				}
			}
			assert.Equal(t, tt.want, got)
		})
	}
}
