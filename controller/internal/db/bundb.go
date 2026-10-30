// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/uptrace/bun/extra/bunotel"
	"github.com/uptrace/bun/extra/bunslog"
	"github.com/uptrace/bun/schema"
	"go.opentelemetry.io/otel/trace"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
)

// queryLogFormat returns the attribute builder for query events: the operation,
// the query and the duration, and on failure the error with the PostgreSQL
// SQLSTATE, severity and message when it is a pgdriver.Error. The query has a
// "?" for each value, and a PostgreSQL error is logged without its message,
// unless logValues is set.
func queryLogFormat(logValues bool) func(*bun.QueryEvent) []slog.Attr {
	return func(event *bun.QueryEvent) []slog.Attr {
		// A statement's values can be operator-supplied content such as cloud-init
		// files, and a failed or slow statement is logged at error or warn, so
		// the values are logged only on request.
		query := placeholderQuery(event)
		if logValues {
			query = event.Query
		}
		duration := time.Since(event.StartTime)
		attrs := []slog.Attr{
			slog.String("operation", event.Operation()),
			slog.String("query", query),
			slog.String("duration", duration.String()),
		}
		if event.Err != nil {
			// A PostgreSQL error message can repeat the value it rejected, for
			// example "invalid input syntax for type uuid", so unless logValues
			// is set the error is logged by its severity and SQLSTATE alone.
			var pgErr pgdriver.Error
			isPG := errors.As(event.Err, &pgErr)
			if isPG && !logValues {
				attrs = append(attrs, slog.String("error", fmt.Sprintf("%s: (SQLSTATE=%s)", pgErr.Field('S'), pgErr.Field('C'))))
			} else {
				attrs = append(attrs, slog.Any("error", event.Err))
			}
			if isPG {
				if sqlstate := pgErr.Field('C'); sqlstate != "" {
					attrs = append(attrs,
						slog.String("pg_sqlstate", sqlstate),
						slog.String("pg_severity", pgErr.Field('S')),
						slog.Bool("pg_statement_timeout", pgErr.StatementTimeout()),
					)
					if logValues {
						attrs = append(attrs, slog.String("pg_message", pgErr.Field('M')))
					}
				}
			}
		}
		return attrs
	}
}

// placeholderQuery returns the SQL of event with a "?" where each value was, or
// an empty string when the SQL cannot be rebuilt.
func placeholderQuery(event *bun.QueryEvent) string {
	// A query run with db.ExecContext has no query object, and its template
	// already has the placeholders.
	if event.IQuery == nil {
		return event.QueryTemplate
	}
	// For ORM queries and bun.NewRaw, bun puts the values into both Query and
	// QueryTemplate, so rebuild the SQL from the query object without formatting
	// its arguments, as bunotel does for the db.statement span attribute.
	b, err := event.IQuery.AppendQuery(schema.NewNopQueryGen(), nil)
	if err != nil {
		// Falling back to Query or QueryTemplate would log the values.
		return ""
	}
	return string(b)
}

// parentedTracingHook prevents background queries from becoming standalone
// root traces. Embedding preserves Bun's Init and AfterQuery behavior, including
// database metrics for queries that have no trace parent.
type parentedTracingHook struct {
	*bunotel.QueryHook
}

type querySpansDisabledKey struct{}

// WithoutQuerySpans returns a context that keeps trace correlation for logs but
// asks the Bun tracing hook not to create spans for database queries. Temporal
// wait and poll loops should wrap their repeated read-only queries with this
// context. Do not pass it through mixed read/write transactions, because it
// suppresses spans for every query in the transaction. Use it for other
// repetitive queries only when their individual spans add no diagnostic value.
func WithoutQuerySpans(ctx context.Context) context.Context {
	return context.WithValue(ctx, querySpansDisabledKey{}, true)
}

func (h *parentedTracingHook) BeforeQuery(ctx context.Context, event *bun.QueryEvent) context.Context {
	spanContext := trace.SpanContextFromContext(ctx)
	if !spanContext.IsValid() {
		return ctx
	}
	if disabled, _ := ctx.Value(querySpansDisabledKey{}).(bool); disabled {
		// Preserve the trace and span IDs for query logs, but replace the
		// recording parent so bunotel.AfterQuery records only the metric and
		// cannot mutate or end the activity span.
		return trace.ContextWithSpanContext(ctx, spanContext)
	}
	return h.QueryHook.BeforeQuery(ctx, event)
}

// newTracingHook labels spans with the database actually connected to, so
// sites that set a different appDb.dbName are not reported as cloud-workflow.
func newTracingHook(connector *pgdriver.Connector, opts ...bunotel.Option) *parentedTracingHook {
	return &parentedTracingHook{
		QueryHook: bunotel.NewQueryHook(append([]bunotel.Option{bunotel.WithDBName(connector.Config().Database)}, opts...)...),
	}
}

// NewBunDB creates bun database object based on configuration.
func NewBunDB(cfg config.Config, logger *slog.Logger) (*bun.DB, error) {
	var db *bun.DB

	dsn, err := cfg.EffectiveDBConnectionString()
	if err != nil {
		return nil, fmt.Errorf("resolve DB connection string: %w", err)
	}
	connector := pgdriver.NewConnector(pgdriver.WithDSN(dsn))
	sqldb := sql.OpenDB(connector)

	// https://bun.uptrace.dev/guide/running-bun-in-production.html#running-bun-in-production
	// Note: The bun docs show 4 * GOMAXPROCS(0) as the default max open connections, however this causes
	// problems as our uber/automaxprocs library sets GOMAXPROCS to 128. 128 * 4 = 512, which is far greater
	// than our current PG limit of 100.
	sqldb.SetMaxOpenConns(cfg.DBMaxOpenConnections)
	sqldb.SetMaxIdleConns(cfg.DBMaxOpenConnections)

	db = bun.NewDB(sqldb, pgdialect.New())

	queryLogLevel := slog.LevelDebug
	if cfg.DBQueryLogLevel == "info" {
		queryLogLevel = slog.LevelInfo
	}

	db = db.WithQueryHook(bunslog.NewQueryHook(
		bunslog.WithQueryLogLevel(queryLogLevel),
		bunslog.WithSlowQueryLogLevel(slog.LevelWarn),
		bunslog.WithErrorQueryLogLevel(slog.LevelError),
		bunslog.WithSlowQueryThreshold(3*time.Second),
		bunslog.WithLogger(logger),
		bunslog.WithLogFormat(queryLogFormat(cfg.DBQueryLogValues)),
	))

	if cfg.Tracing.Enabled {
		db = db.WithQueryHook(newTracingHook(connector))
	}

	if err := db.PingContext(context.Background()); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	return db, nil
}
