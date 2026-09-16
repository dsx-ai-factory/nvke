// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

const failureDomainPostgresDSNEnv = "NKE_TEST_POSTGRES_DSN"

// TestConcurrentFailureDomainSelectionSerializesOnClusterRowPostgres verifies
// the real PostgreSQL row-lock behavior that the SQLite controller tests cannot
// exercise. The supplied database must be disposable; the test isolates itself
// in a randomly named schema and removes that schema on completion.
func TestConcurrentFailureDomainSelectionSerializesOnClusterRowPostgres(t *testing.T) {
	dsn := os.Getenv(failureDomainPostgresDSNEnv)
	if dsn == "" {
		t.Skipf("set %s to a disposable PostgreSQL database to run this test", failureDomainPostgresDSNEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	database := setupFailureDomainPostgresTestDB(ctx, t, dsn)

	configID := uuid.New()
	cluster := db.Cluster{
		ID:                uuid.New(),
		Name:              "concurrent-failure-domain-test",
		KubernetesVersion: "1.34.2",
		ControlPlaneConfig: db.ControlPlaneConfig{
			ConfigurationID: configID,
			DesiredCount:    2,
			ControlPlaneConfigSpec: db.ControlPlaneConfigSpec{
				KubernetesVersion: "1.34.2",
			},
		},
	}
	cluster.SetNicoProvider(db.NicoClusterProvider{
		OrgID:  "test-org",
		SiteID: uuid.New(),
		VpcID:  uuid.New(),
	})
	_, err := database.CreateCluster(ctx, &cluster)
	require.NoError(t, err)
	_, err = database.CreateControlPlaneConfiguration(ctx, &db.ControlPlaneConfiguration{
		ID:        configID,
		ClusterID: cluster.ID,
		Config:    cluster.ControlPlaneConfig.ControlPlaneConfigSpec,
	})
	require.NoError(t, err)

	fdA := "fd-a"
	existingNode := db.Node{
		ID:                          uuid.New(),
		ClusterID:                   cluster.ID,
		ControlPlane:                true,
		ControlPlaneConfigurationID: configID,
		FailureDomain:               &fdA,
		Name:                        "cp-0",
		Conditions: db.NodeConditions{
			Pending: db.NewCondition(true, "pending"),
		},
	}
	existingNode.SetNico(db.NicoNodeProvider{})
	_, err = database.CreateNode(ctx, &existingNode)
	require.NoError(t, err)

	firstAcquired := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondStarted := make(chan struct{})
	secondAcquired := make(chan struct{})
	errs := make(chan error, 2)

	go func() {
		errs <- reconcileFailureDomainScaleUpForTest(
			ctx,
			database,
			cluster.ID,
			func() {
				close(firstAcquired)
				<-releaseFirst
			},
		)
	}()

	select {
	case <-firstAcquired:
	case <-ctx.Done():
		require.FailNow(t, "first transaction did not acquire the cluster row lock", ctx.Err())
	}

	go func() {
		close(secondStarted)
		errs <- reconcileFailureDomainScaleUpForTest(
			ctx,
			database,
			cluster.ID,
			func() { close(secondAcquired) },
		)
	}()
	<-secondStarted

	select {
	case <-secondAcquired:
		close(releaseFirst)
		require.FailNow(t, "second transaction acquired the cluster row while the first transaction held it")
	case <-time.After(150 * time.Millisecond):
	}
	close(releaseFirst)

	for range 2 {
		require.NoError(t, <-errs)
	}
	select {
	case <-secondAcquired:
	case <-ctx.Done():
		require.FailNow(t, "second transaction did not acquire the released cluster row lock", ctx.Err())
	}

	cpTrue := true
	nodes, err := database.ListNodes(ctx, db.NodeFilters{ClusterID: &cluster.ID, ControlPlane: &cpTrue})
	require.NoError(t, err)
	require.Len(t, nodes, 2)
	require.NotNil(t, nodes[0].FailureDomain)
	require.NotNil(t, nodes[1].FailureDomain)
	assert.ElementsMatch(t, []string{"fd-a", "fd-b"}, []string{
		*nodes[0].FailureDomain,
		*nodes[1].FailureDomain,
	})
}

// reconcileFailureDomainScaleUpForTest scales a control plane within a locked transaction.
func reconcileFailureDomainScaleUpForTest(
	ctx context.Context,
	database db.DB,
	clusterID uuid.UUID,
	afterLock func(),
) error {
	return database.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		cluster, err := tx.GetClusterForUpdate(ctx, clusterID)
		if err != nil {
			return fmt.Errorf("locking cluster: %w", err)
		}
		afterLock()

		controlPlane, err := readControlPlane(ctx, tx, cluster, true)
		if err != nil {
			return fmt.Errorf("reading control plane: %w", err)
		}
		r := newReconciler(
			config.Config{ProvisionControlPlaneWithControlPlaneController: true},
			slog.New(slog.DiscardHandler),
			tx,
			nil,
			failureDomainSnapshot{eligible: []string{"fd-a", "fd-b"}},
		)
		if err := r.prepareFailureDomainAssignments(ctx, controlPlane); err != nil {
			return fmt.Errorf("preparing failure-domain assignments: %w", err)
		}
		if err := r.reconcileControlPlaneScaleUp(ctx, cluster, controlPlane); err != nil {
			return fmt.Errorf("scaling control plane: %w", err)
		}
		return nil
	})
}

// setupFailureDomainPostgresTestDB creates an isolated PostgreSQL schema for a test.
func setupFailureDomainPostgresTestDB(
	ctx context.Context,
	t *testing.T,
	dsn string,
) db.DB {
	t.Helper()

	adminSQL := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	adminDB := bun.NewDB(adminSQL, pgdialect.New())
	t.Cleanup(func() {
		require.NoError(t, adminDB.Close())
	})
	require.NoError(t, adminDB.PingContext(ctx))

	schema := "controlplane_fd_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err := adminDB.ExecContext(ctx, "CREATE SCHEMA "+schema)
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, cleanupErr := adminDB.ExecContext(cleanupCtx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
		require.NoError(t, cleanupErr)
	})

	testSQL := sql.OpenDB(pgdriver.NewConnector(
		pgdriver.WithDSN(dsn),
		pgdriver.WithConnParams(map[string]any{"search_path": schema}),
	))
	bunDB := bun.NewDB(testSQL, pgdialect.New())
	t.Cleanup(func() {
		require.NoError(t, bunDB.Close())
	})

	for _, model := range []any{
		(*db.Cluster)(nil),
		(*db.ControlPlaneConfiguration)(nil),
		(*db.NodePool)(nil),
		(*db.NodeSet)(nil),
		(*db.Node)(nil),
	} {
		_, err := bunDB.NewCreateTable().Model(model).Exec(ctx)
		require.NoError(t, err)
	}

	return db.NewDB(bunDB)
}
