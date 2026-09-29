// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/uptrace/bun/extra/bunotel"
	"github.com/uptrace/bun/extra/bunslog"

	"github.com/dsx-ai-factory/nke/controller/internal/config"
)

// logFormat creates slog attributes for query events, including PostgreSQL
// protocol fields (SQLSTATE, severity, message) when the error is a pgdriver.Error.
func logFormat(event *bun.QueryEvent) []slog.Attr {
	duration := time.Since(event.StartTime)
	attrs := []slog.Attr{
		slog.String("operation", event.Operation()),
		slog.String("query", event.Query),
		slog.String("duration", duration.String()),
	}
	if event.Err != nil {
		attrs = append(attrs, slog.Any("error", event.Err))

		// Add PostgreSQL protocol fields when err wraps pgdriver.Error
		var pgErr pgdriver.Error
		if errors.As(event.Err, &pgErr) {
			if sqlstate := pgErr.Field('C'); sqlstate != "" {
				attrs = append(attrs,
					slog.String("pg_sqlstate", sqlstate),
					slog.String("pg_severity", pgErr.Field('S')),
					slog.String("pg_message", pgErr.Field('M')),
					slog.Bool("pg_statement_timeout", pgErr.StatementTimeout()),
				)
			}
		}
	}
	return attrs
}

// NewBunDB creates bun database object based on configuration.
func NewBunDB(cfg config.Config, logger *slog.Logger) (*bun.DB, error) {
	var db *bun.DB

	dsn, err := cfg.EffectiveDBConnectionString()
	if err != nil {
		return nil, fmt.Errorf("resolve DB connection string: %w", err)
	}
	sqldb := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))

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

	db.AddQueryHook(bunslog.NewQueryHook(
		bunslog.WithQueryLogLevel(queryLogLevel),
		bunslog.WithSlowQueryLogLevel(slog.LevelWarn),
		bunslog.WithErrorQueryLogLevel(slog.LevelError),
		bunslog.WithSlowQueryThreshold(3*time.Second),
		bunslog.WithLogger(logger),
		bunslog.WithLogFormat(logFormat),
	))

	if cfg.Tracing.Enabled {
		db.AddQueryHook(bunotel.NewQueryHook(
			bunotel.WithDBName(dbNameFromDSN(dsn)),
		))
	}

	if err := db.PingContext(context.Background()); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	return db, nil
}

// dbNameFromDSN extracts the database name from a Postgres connection string,
// for the OTel db.name span attribute. Reading it from the DSN rather than a
// separately-configured value means it can never drift from the database the
// connection actually targets, whichever of appDb.dbName or a Secret-sourced
// DB_CONNECTION_STRING (see values.yaml dbConnectionString) set it. Returns
// "" if the DSN does not parse, since a span attribute should degrade rather
// than block startup.
func dbNameFromDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(u.Path, "/")
}
