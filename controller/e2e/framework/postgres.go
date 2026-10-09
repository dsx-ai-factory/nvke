//go:build gofail

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package framework

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun/driver/pgdriver"
)

const (
	CWEGofailPostgresDSNEnv = "NKE_CONTROLLER_GOFAIL_E2E_DB_CONNECTION_STRING"
	postgresReadyTimeout    = 15 * time.Second
	// legacyPostgresDSNEnv is a deprecated alias; nke#1399 removes it.
	legacyPostgresDSNEnv = "CWE_GOFAIL_E2E_DB_CONNECTION_STRING"
)

// StartPostgres returns the CI-provided PostgreSQL DSN or starts an ephemeral local server.
func StartPostgres(t testing.TB) string {
	t.Helper()

	if dsn := getenvWithLegacy(CWEGofailPostgresDSNEnv, legacyPostgresDSNEnv); dsn != "" {
		waitForPostgres(t, dsn)
		return dsn
	}

	initdb, err := exec.LookPath("initdb")
	require.NoError(t, err, "initdb is required locally; set %s to use an existing disposable PostgreSQL database", CWEGofailPostgresDSNEnv)
	postgres, err := exec.LookPath("postgres")
	require.NoError(t, err, "postgres is required locally; set %s to use an existing disposable PostgreSQL database", CWEGofailPostgresDSNEnv)

	dataDir := filepath.Join(t.TempDir(), "data")
	initCmd := exec.Command(initdb, "-D", dataDir, "-A", "trust", "-U", "postgres", "--no-locale", "--encoding=UTF8", "--no-sync") // #nosec G204 -- test-controlled executable and directory.
	output, err := initCmd.CombinedOutput()
	require.NoError(t, err, "initdb failed:\n%s", output)

	addr := FreeLocalhostAddress(t)
	_, port, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	process := StartProcess(t, ProcessConfig{
		Command:       postgres,
		Args:          []string{"-D", dataDir, "-h", "127.0.0.1", "-p", port, "-F"},
		ReadyContains: "database system is ready to accept connections",
		ReadyTimeout:  postgresReadyTimeout,
	})
	process.WaitReady(t)
	t.Cleanup(func() {
		process.Stop(t)
	})

	dsn := fmt.Sprintf("postgres://postgres@%s/postgres?sslmode=disable", addr)
	waitForPostgres(t, dsn)
	return dsn
}

func waitForPostgres(t testing.TB, dsn string) {
	t.Helper()

	database := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	t.Cleanup(func() {
		require.NoError(t, database.Close())
	})

	ctx, cancel := context.WithTimeout(context.Background(), postgresReadyTimeout)
	defer cancel()
	for {
		if err := database.PingContext(ctx); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			require.FailNow(t, "PostgreSQL did not become ready", "dsn=%s", dsn)
		case <-time.After(100 * time.Millisecond):
		}
	}
}
