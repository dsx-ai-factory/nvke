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

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/db"
)

// TeardownHookConfigProfile is the fixture profile carrying a beforeRemoveNode
// hook. It must exist under test/fixtures/profiles/configProfiles.
const TeardownHookConfigProfile = "gofail-teardown-hooks"

// TeardownHookRunbook is the hook runbook the fixture profile declares, and the
// one e2e scenarios target with the CWERunbookExecutionError failpoint.
const TeardownHookRunbook = "evict-cnpg-node"

// TeardownScenario is one seeded cluster for a teardown e2e: the control-plane
// node under teardown plus a surviving joined peer. The peer matters — teardown
// skips both the hooks and live removal when the target is the only joined
// control-plane node, since it would be the sole runbook executor.
type TeardownScenario struct {
	Name        string
	ClusterID   uuid.UUID
	TargetNode  uuid.UUID
	SurvivorNde uuid.UUID
}

// SeedTeardownScenario inserts a cluster whose control-plane pool uses the
// teardown-hook config profile, and returns its identifiers. Each call creates
// independent records so scenarios can run concurrently against one worker.
func SeedTeardownScenario(t testing.TB, dsn, name string) TeardownScenario {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bunDB, err := db.NewBunDB(config.Config{
		DBConnectionString:   dsn,
		DBMaxOpenConnections: 2,
		DBQueryLogLevel:      "info",
	}, logger)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, bunDB.Close())
	}()

	scenario := TeardownScenario{
		Name:        name,
		ClusterID:   uuid.New(),
		TargetNode:  uuid.New(),
		SurvivorNde: uuid.New(),
	}
	require.NoError(t, seedTeardownScenario(context.Background(), db.NewDB(bunDB), scenario))
	return scenario
}

func seedTeardownScenario(ctx context.Context, database db.DB, scenario TeardownScenario) error {
	siteID := uuid.New()
	if _, err := database.RegisterSite(ctx, &db.Site{
		ID:        siteID,
		VIPRanges: []string{},
	}); err != nil {
		return fmt.Errorf("create site: %w", err)
	}

	if _, err := database.CreateCluster(ctx, &db.Cluster{
		ID:                scenario.ClusterID,
		Name:              "cwe-gofail-" + scenario.Name,
		OrgID:             "cwe-gofail-org",
		SiteID:            siteID,
		VpcID:             uuid.New(),
		KubernetesVersion: "1.30.0",
		Conditions: db.ClusterConditions{
			Provisioned: db.NewCondition(true, "seeded"),
		},
	}); err != nil {
		return fmt.Errorf("create cluster: %w", err)
	}

	nodePoolID := uuid.New()
	nodeSetID := uuid.New()
	instanceTypeID := uuid.New()

	if _, err := database.CreateNodePool(ctx, &db.NodePool{
		ID:                    nodePoolID,
		ClusterID:             scenario.ClusterID,
		Name:                  "cp",
		NodeNamePrefix:        "cp",
		ControlPlane:          true,
		DesiredCount:          2,
		InstanceTypeID:        instanceTypeID,
		OSProfile:             "test",
		ConfigProfile:         TeardownHookConfigProfile,
		SystemReservedMemory:  "16Gi",
		SubnetIDs:             []uuid.UUID{},
		VpcPrefixIDs:          []uuid.UUID{},
		SSHKeyGroupIDs:        []uuid.UUID{},
		NodeLabels:            []string{},
		DiskSelectionStrategy: db.DiskSelectionStrategySmallest,
		Conditions: db.NodePoolConditions{
			Provisioned: db.NewCondition(true, "seeded"),
		},
	}); err != nil {
		return fmt.Errorf("create node pool: %w", err)
	}

	if _, err := database.CreateNodeSet(ctx, &db.NodeSet{
		ID:                    nodeSetID,
		NodePoolID:            nodePoolID,
		DesiredCount:          2,
		OSProfile:             "test",
		ConfigProfile:         TeardownHookConfigProfile,
		KubernetesVersion:     "1.30.0",
		SystemReservedMemory:  "16Gi",
		SubnetIDs:             []uuid.UUID{},
		VpcPrefixIDs:          []uuid.UUID{},
		DiskSelectionStrategy: db.DiskSelectionStrategySmallest,
	}); err != nil {
		return fmt.Errorf("create node set: %w", err)
	}

	nodes := []struct {
		id   uuid.UUID
		name string
	}{
		{scenario.TargetNode, scenario.Name + "-target"},
		{scenario.SurvivorNde, scenario.Name + "-survivor"},
	}
	for _, node := range nodes {
		if _, err := database.CreateNode(ctx, &db.Node{
			ID:              node.id,
			ClusterID:       scenario.ClusterID,
			NodePoolID:      nodePoolID,
			NodeSetID:       nodeSetID,
			ControlPlane:    true,
			InstanceTypeID:  instanceTypeID,
			Name:            node.name,
			ForgeInstanceID: node.name + "-instance",
			ForgeMachineID:  node.name + "-machine",
			IP:              "127.0.0.1",
			Conditions: db.NodeConditions{
				Joined:          db.NewCondition(true, "seeded"),
				KubernetesReady: db.NewCondition(true, "seeded"),
				NodeAgentReady:  db.NewCondition(true, "seeded"),
			},
		}); err != nil {
			return fmt.Errorf("create node %s: %w", node.name, err)
		}
	}

	return nil
}
