// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package testutil provides test utilities for the cloud-workflow-engine project.
package testutil

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"

	sqlite3 "github.com/mattn/go-sqlite3"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"
)

// SetupDB creates an in-memory SQLite database for testing.
// It registers the provided models and creates their tables.
// The database is automatically closed when the test completes.
func SetupDB(ctx context.Context, t *testing.T, models ...interface{}) *bun.DB {
	t.Helper()

	bunDB, err := NewDB(ctx, models...)
	require.NoError(t, err)

	t.Cleanup(func() {
		bunDB.Close()
	})

	return bunDB
}

// NewDB creates an in-memory SQLite database and registers the provided
// models. The caller must close the returned database.
func NewDB(ctx context.Context, models ...interface{}) (*bun.DB, error) {
	registerDriverOnce.Do(registerDriver)

	// Each test gets a unique in-memory database by using a UUID in the filename.
	// cache=shared allows multiple connections within the same sql.DB pool to share
	// the database, but different tests get completely isolated databases.
	sqldb, err := sql.Open(sqliteDriverName,
		fmt.Sprintf("file:%s?mode=memory&cache=shared", uuid.NewString()),
	)
	if err != nil {
		return nil, fmt.Errorf("opening in-memory SQLite database: %w", err)
	}

	// One connection at a time. Two connections sharing a cache can return
	// SQLITE_LOCKED_SHAREDCACHE, which no busy handler retries, and go-sqlite3
	// only waits for it when built with -tags sqlite_unlock_notify.
	sqldb.SetMaxOpenConns(1)

	bunDB := bun.NewDB(sqldb, sqlitedialect.New())

	for _, model := range models {
		if err := createTableForSQLite(ctx, bunDB, model); err != nil {
			if closeErr := bunDB.Close(); closeErr != nil {
				return nil, fmt.Errorf("creating SQLite table: %w; closing database: %w", err, closeErr)
			}
			return nil, fmt.Errorf("creating SQLite table: %w", err)
		}
	}

	return bunDB, nil
}

//nolint:gochecknoglobals // sync.Once must be at package level to ensure single initialization
var registerDriverOnce = sync.Once{}

const (
	// sqliteDriverName is our own registration, separate from the "sqlite3" that
	// go-sqlite3 registers in its init. Registering our own is what lets the
	// gen_random_uuid hook be attached.
	sqliteDriverName = "sqlite3_nvke"
)

// registerDriver registers a SQLite driver carrying a custom gen_random_uuid
// function.
//
// SQLite has no gen_random_uuid(), but most of our models use it as the DEFAULT
// for their ID column, because the real datastore is PostgreSQL.
//
// Requires cgo. go-sqlite3 compiles to a stub when cgo is off -- which a
// cross-compile does by default, not only an explicit CGO_ENABLED=0 -- and then
// fails at the first query rather than at build time. A test binary built that
// way fails with "requires cgo to work".
func registerDriver() {
	sql.Register(sqliteDriverName, &sqlite3.SQLiteDriver{
		ConnectHook: func(conn *sqlite3.SQLiteConn) error {
			// pure=false keeps SQLite from caching one result for the whole
			// statement: a multi-row INSERT ... VALUES would otherwise give
			// every row the same ID.
			return conn.RegisterFunc("gen_random_uuid", uuid.NewString, false)
		},
	})
}

func createTableForSQLite(ctx context.Context, db *bun.DB, model interface{}) error {
	q := db.NewCreateTable().Model(model).IfNotExists()

	queryBytes, err := q.AppendQuery(db.QueryGen(), nil)
	if err != nil {
		return err
	}

	queryStr := string(queryBytes)

	// SQLite requires expressions in DEFAULT to be in parentheses, this is different from PostgreSQL.
	// We look for "DEFAULT gen_random_uuid()" and replace it with "DEFAULT (gen_random_uuid())"
	// If we start using other functions for defaults, we will need to update this, for now I choose
	// to not make this generic.
	queryStr = strings.ReplaceAll(queryStr, "DEFAULT gen_random_uuid()", "DEFAULT (gen_random_uuid())")

	_, err = db.ExecContext(ctx, queryStr)
	return err
}
