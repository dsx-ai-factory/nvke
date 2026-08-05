// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

// Package testutil provides test utilities for the cloud-workflow-engine project.
package testutil

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"modernc.org/sqlite"
)

// SetupDB creates an in-memory SQLite database for testing.
// It registers the provided models and creates their tables.
// The database is automatically closed when the test completes.
func SetupDB(ctx context.Context, t *testing.T, models ...interface{}) *bun.DB {
	t.Helper()

	driverName, err := registerDriver()
	require.NoError(t, err)

	// Each test gets a unique in-memory database by using a UUID in the filename.
	// cache=shared allows multiple connections within the same sql.DB pool to share
	// the database, but different tests get completely isolated databases.
	sqldb, err := sql.Open(driverName,
		fmt.Sprintf("file:%s?mode=memory&cache=shared", uuid.NewString()),
	)
	require.NoError(t, err)

	bunDB := bun.NewDB(sqldb, sqlitedialect.New())

	for _, model := range models {
		require.NoError(t, createTableForSQLite(ctx, bunDB, model))
	}

	t.Cleanup(func() {
		bunDB.Close()
	})

	return bunDB
}

//nolint:gochecknoglobals // sync.Once must be at package level to ensure single initialization
var registerDriverOnce = sync.Once{}

// registerDriver registers the modernc.org/sqlite driver with a custom gen_random_uuid function.
// It returns the driver name to use with sql.Open.
//
//nolint:nonamedreturns // named returns improve readability without using naked returns
func registerDriver() (driverName string, err error) {
	registerDriverOnce.Do(func() {
		// We register a custom gen_random_uuid function
		// because sqlite does not support the gen_random_uuid postgres function
		// This is used by most of our models to generate a default value for the ID field.
		err = sqlite.RegisterScalarFunction("gen_random_uuid", 0, func(_ *sqlite.FunctionContext, _ []driver.Value) (driver.Value, error) {
			return uuid.NewString(), nil
		})
	})
	if err != nil {
		return "", err
	}

	// modernc sqlite module registers the driver in an init,
	// there is no way to customize the driver name
	// or register our own instance of the driver
	// so we just return the default driver name they use
	return "sqlite", nil
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
