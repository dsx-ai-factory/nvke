//go:build gofail

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package framework

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// SeedRebootWorker adds an independent worker pool to a seeded cluster. Its
// existing control-plane nodes supply the tenant kubeconfig through the agent.
func SeedRebootWorker(t testing.TB, dsn string, cluster TeardownScenario) db.Node {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bunDB, err := db.NewBunDB(config.Config{
		DBConnectionString: dsn, DBMaxOpenConnections: 2, DBQueryLogLevel: "info",
	}, logger)
	require.NoError(t, err)
	defer func() { require.NoError(t, bunDB.Close()) }()

	var worker db.Node
	err = db.NewDB(bunDB).RunInTx(t.Context(), func(ctx context.Context, tx db.Tx) error {
		node, err := tx.GetNode(ctx, cluster.TargetNode)
		if err != nil {
			return fmt.Errorf("get seed node: %w", err)
		}
		pool, err := tx.GetNodePool(ctx, node.NodePoolID)
		if err != nil {
			return fmt.Errorf("get seed pool: %w", err)
		}
		set, err := tx.GetNodeSet(ctx, node.NodeSetID)
		if err != nil {
			return fmt.Errorf("get seed set: %w", err)
		}
		pool.ID, pool.Name, pool.NodeNamePrefix = uuid.New(), "worker", "worker"
		pool.ControlPlane, pool.DesiredCount = false, 1
		pool.CurrentNodeSetID = nil
		pool.AutoProvisionDisabled, pool.AutoModeDisabled = true, true
		if _, err := tx.CreateNodePool(ctx, &pool); err != nil {
			return fmt.Errorf("create worker pool: %w", err)
		}
		set.ID, set.NodePoolID, set.DesiredCount = uuid.New(), pool.ID, 1
		if _, err := tx.CreateNodeSet(ctx, &set); err != nil {
			return fmt.Errorf("create worker set: %w", err)
		}
		node.ID, node.Name = uuid.New(), cluster.Name+"-worker"
		node.NodePoolID, node.NodeSetID, node.ControlPlane = pool.ID, set.ID, false
		worker, err = tx.CreateNode(ctx, &node)
		if err != nil {
			return fmt.Errorf("create worker: %w", err)
		}
		return nil
	})
	require.NoError(t, err)
	return worker
}
