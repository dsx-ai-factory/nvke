// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build gofail

package gofail

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/e2e/framework"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/db/migrations"
)

// TestClusterConditionLocking verifies both update orders with independent
// PostgreSQL connections. The second reader must wait for the first commit and
// merge its condition into the committed row, preserving the first writer.
func TestClusterConditionLocking(t *testing.T) {
	dsn := framework.StartPostgres(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bunDB, err := db.NewBunDB(config.Config{DBConnectionString: dsn, DBMaxOpenConnections: 4}, logger)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bunDB.Close()) })
	require.NoError(t, migrations.Migrate(t.Context(), bunDB))
	database := db.NewDB(bunDB)
	updateFailed := db.Condition{Status: db.ConditionStatusFalse, Message: "Cluster update failed", LastTransitionTime: time.Now().UTC()}
	provisioned := db.Condition{Status: db.ConditionStatusTrue, Message: "Provisioned", LastTransitionTime: time.Now().UTC()}
	writers := []struct {
		name  string
		apply func(*db.Cluster)
	}{
		{name: "update failure", apply: func(c *db.Cluster) { c.Conditions.UpdateFailed = &updateFailed }},
		{name: "lifecycle", apply: func(c *db.Cluster) { c.Conditions.Provisioned = provisioned }},
	}
	for first := range writers {
		t.Run(writers[first].name+" first", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			cluster := db.Cluster{ID: uuid.New(), Name: "condition-locking-" + uuid.NewString(), KubernetesVersion: "1.35.0"}
			cluster.SetCAPIProvider(db.CAPIClusterProvider{})
			_, err := database.CreateCluster(ctx, &cluster)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, database.DeleteCluster(context.Background(), cluster.ID)) })

			locked := make(chan struct{})
			release := make(chan struct{})
			firstDone := make(chan error, 1)
			secondDone := make(chan error, 1)
			go func() {
				firstDone <- database.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
					row, err := tx.GetClusterForUpdate(ctx, cluster.ID)
					if err != nil {
						return err
					}
					close(locked)
					select {
					case <-release:
					case <-ctx.Done():
						return ctx.Err()
					}
					writers[first].apply(&row)
					_, err = tx.UpdateCluster(ctx, &row)
					return err
				})
			}()
			select {
			case <-locked:
			case err := <-firstDone:
				require.NoError(t, err)
				t.Fatal("writer ended before locking")
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			go func() {
				secondDone <- database.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
					row, err := tx.GetClusterForUpdate(ctx, cluster.ID)
					if err != nil {
						return err
					}
					writers[1-first].apply(&row)
					_, err = tx.UpdateCluster(ctx, &row)
					return err
				})
			}()
			// Check the actual database wait, not a goroutine scheduling delay.
			require.Eventually(t, func() bool {
				var blocked int
				err := bunDB.NewRaw(`SELECT count(*) FROM pg_stat_activity
					WHERE pid <> pg_backend_pid() AND cardinality(pg_blocking_pids(pid)) > 0 AND query LIKE ?`, "%"+cluster.ID.String()+"%").Scan(ctx, &blocked)
				return err == nil && blocked == 1
			}, 5*time.Second, 10*time.Millisecond, "second reader did not wait for the row lock")
			close(release)
			require.NoError(t, <-firstDone)
			require.NoError(t, <-secondDone)
			row, err := database.GetCluster(ctx, cluster.ID)
			require.NoError(t, err)
			require.NotNil(t, row.Conditions.UpdateFailed)
			require.True(t, row.Conditions.UpdateFailed.Equal(updateFailed))
			require.True(t, row.Conditions.Provisioned.Equal(provisioned))
		})
	}
}
