// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/uptrace/bun/extra/bunotel"
	"github.com/uptrace/bun/schema"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type recordingMeterProvider struct {
	metric.MeterProvider
	histogram *recordingHistogram
}

func newRecordingMeterProvider() *recordingMeterProvider {
	return &recordingMeterProvider{
		MeterProvider: noop.NewMeterProvider(),
		histogram:     &recordingHistogram{},
	}
}

func (p *recordingMeterProvider) Meter(name string, opts ...metric.MeterOption) metric.Meter {
	return &recordingMeter{
		Meter:     p.MeterProvider.Meter(name, opts...),
		histogram: p.histogram,
	}
}

type recordingMeter struct {
	metric.Meter
	histogram *recordingHistogram
}

func (m *recordingMeter) Int64Histogram(string, ...metric.Int64HistogramOption) (metric.Int64Histogram, error) {
	return m.histogram, nil
}

type recordingHistogram struct {
	noop.Int64Histogram
	records int
}

func (h *recordingHistogram) Record(context.Context, int64, ...metric.RecordOption) {
	h.records++
}

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
			dsn:  "postgres://postgres:postgres@cnpg-cloud-rw.cnpg-cloud.svc.cluster.local/nvke-controller?sslmode=disable",
			want: "nvke-controller",
		},
		{
			name: "chart-created secret with escaped password",
			dsn:  "postgresql://postgres:p%40ss%2Fword@postgresql:5432/nvke-controller?sslmode=require",
			want: "nvke-controller",
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
			ctx, parent := tp.Tracer("test").Start(context.Background(), "parent")
			ctx = hook.BeforeQuery(ctx, event)
			hook.AfterQuery(ctx, event)

			spans := recorder.Ended()
			require.Len(t, spans, 1)
			assert.Equal(t, parent.SpanContext().TraceID(), spans[0].SpanContext().TraceID())
			assert.Equal(t, parent.SpanContext().SpanID(), spans[0].Parent().SpanID())
			var got string
			for _, attr := range spans[0].Attributes() {
				if attr.Key == "db.name" {
					got = attr.Value.AsString()
				}
			}
			assert.Equal(t, tt.want, got)
			parent.End()
		})
	}
}

func TestTracingHookDoesNotCreateRootSpans(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	meterProvider := newRecordingMeterProvider()
	connector := pgdriver.NewConnector(pgdriver.WithDSN("postgres://postgres:postgres@localhost:5432/nvke-controller?sslmode=disable"))
	hook := newTracingHook(connector, bunotel.WithTracerProvider(tp), bunotel.WithMeterProvider(meterProvider))

	bdb := bun.NewDB(sql.OpenDB(connector), pgdialect.New())
	t.Cleanup(func() { _ = bdb.Close() })
	event := &bun.QueryEvent{DB: bdb, Query: "SELECT 1", StartTime: time.Now()}
	ctx := hook.BeforeQuery(context.Background(), event)
	hook.AfterQuery(ctx, event)

	assert.Empty(t, recorder.Ended())
	assert.Equal(t, 1, meterProvider.histogram.records)
}

type queryLogRow struct {
	bun.BaseModel `bun:"table:query_log_rows"`

	ID       int      `bun:",pk"`
	Content  string   `bun:"type:text"`
	Commands []string `bun:"type:jsonb"`
}

// unrebuildableQuery is a bun.Query whose AppendQuery always fails.
type unrebuildableQuery struct{}

func (unrebuildableQuery) AppendQuery(schema.QueryGen, []byte) ([]byte, error) {
	return nil, errors.New("cannot append")
}
func (unrebuildableQuery) Operation() string    { return "INSERT" }
func (unrebuildableQuery) GetModel() bun.Model  { return nil }
func (unrebuildableQuery) GetTableName() string { return "query_log_rows" }

// TestQueryLogKeepsBoundValuesOutOfTheLogUnlessAsked covers the values of a
// statement: bun puts them into both Query and QueryTemplate for ORM queries, so
// the log line has to be rebuilt without them unless DB_QUERY_LOG_VALUES is set.
// sql.OpenDB does not dial, so no database is needed.
func TestQueryLogKeepsBoundValuesOutOfTheLogUnlessAsked(t *testing.T) {
	const secret = "SECRET-TOKEN-123"

	bdb := bun.NewDB(sql.OpenDB(pgdriver.NewConnector()), pgdialect.New())
	t.Cleanup(func() { _ = bdb.Close() })

	row := &queryLogRow{Content: secret, Commands: []string{"login --token " + secret}}
	queries := []struct {
		name string
		q    bun.Query
	}{
		{"orm insert", bdb.NewInsert().Model(row)},
		{"orm update", bdb.NewUpdate().Model(row).Set("content = ?", secret).WherePK()},
		{"new raw", bdb.NewRaw("INSERT INTO query_log_rows (content) VALUES (?)", secret)},
	}
	modes := []struct {
		name      string
		logValues bool
		level     slog.Level
	}{
		{"values off at info", false, slog.LevelInfo},
		{"values off at debug", false, slog.LevelDebug},
		{"values on", true, slog.LevelInfo},
	}

	logQuery := func(logValues bool, level slog.Level, event *bun.QueryEvent) string {
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: level}))
		attrs := queryLogFormat(logValues)(event)
		logger.LogAttrs(context.Background(), slog.LevelError, "", attrs...)
		return buf.String()
	}

	for _, tt := range queries {
		formatted, err := tt.q.AppendQuery(bdb.QueryGen(), nil)
		require.NoError(t, err)
		for _, m := range modes {
			t.Run(fmt.Sprintf("%s, %s", tt.name, m.name), func(t *testing.T) {
				out := logQuery(m.logValues, m.level, &bun.QueryEvent{
					DB:            bdb,
					IQuery:        tt.q,
					Query:         string(formatted),
					QueryTemplate: string(formatted),
					Err:           errors.New("boom"),
					StartTime:     time.Now(),
				})
				assert.Equal(t, m.logValues, strings.Contains(out, secret), out)
				assert.Contains(t, out, "operation=")
				if !m.logValues {
					assert.Contains(t, out, "?", "the shape of the statement is still logged")
				}
			})
		}
	}

	t.Run("a query run with db.ExecContext keeps its placeholder template", func(t *testing.T) {
		out := logQuery(false, slog.LevelInfo, &bun.QueryEvent{
			DB:            bdb,
			Query:         "INSERT INTO query_log_rows (content) VALUES ('" + secret + "')",
			QueryTemplate: "INSERT INTO query_log_rows (content) VALUES (?)",
			QueryArgs:     []any{secret},
			Err:           errors.New("boom"),
			StartTime:     time.Now(),
		})
		assert.NotContains(t, out, secret)
		assert.Contains(t, out, "VALUES (?)")
	})

	t.Run("a query that cannot be rebuilt logs no SQL rather than the values", func(t *testing.T) {
		out := logQuery(false, slog.LevelInfo, &bun.QueryEvent{
			DB:            bdb,
			IQuery:        unrebuildableQuery{},
			Query:         "INSERT INTO query_log_rows (content) VALUES ('" + secret + "')",
			QueryTemplate: "INSERT INTO query_log_rows (content) VALUES ('" + secret + "')",
			Err:           errors.New("boom"),
			StartTime:     time.Now(),
		})
		assert.NotContains(t, out, secret)
		assert.Contains(t, out, "operation=INSERT")
	})
}

// TestQueryLogKeepsPostgresErrorMessagesOutOfTheLogUnlessAsked covers the
// message of a PostgreSQL error, which can repeat the value it rejected, for
// example "invalid input syntax for type uuid".
func TestQueryLogKeepsPostgresErrorMessagesOutOfTheLogUnlessAsked(t *testing.T) {
	const secret = "SECRET-TOKEN-123"

	pgErr := pgdriver.NewError(map[byte]string{
		'S': "ERROR",
		'C': "22P02",
		'M': `invalid input syntax for type uuid: "` + secret + `"`,
	})
	errs := []struct {
		name string
		err  error
	}{
		{"a PostgreSQL error", pgErr},
		{"a wrapped PostgreSQL error", fmt.Errorf("exec: %w", pgErr)},
	}
	for _, tt := range errs {
		for _, logValues := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, values on=%t", tt.name, logValues), func(t *testing.T) {
				var buf bytes.Buffer
				logger := slog.New(slog.NewTextHandler(&buf, nil))
				attrs := queryLogFormat(logValues)(&bun.QueryEvent{Err: tt.err, StartTime: time.Now()})
				logger.LogAttrs(context.Background(), slog.LevelError, "", attrs...)
				out := buf.String()

				assert.Equal(t, logValues, strings.Contains(out, secret), out)
				assert.Contains(t, out, "pg_sqlstate=22P02")
				assert.Contains(t, out, "pg_severity=ERROR")
				assert.Contains(t, out, "SQLSTATE=22P02", "the error attribute still names the condition")
			})
		}
	}

	t.Run("an error from outside PostgreSQL is logged as is", func(t *testing.T) {
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))
		attrs := queryLogFormat(false)(&bun.QueryEvent{Err: errors.New("context canceled"), StartTime: time.Now()})
		logger.LogAttrs(context.Background(), slog.LevelError, "", attrs...)
		assert.Contains(t, buf.String(), `error="context canceled"`)
	})
}
