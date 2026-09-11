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

// seedTeardownScenario inserts the whole fixture in one transaction.
//
// The transaction is load-bearing, not tidiness. The worker is already running
// and reconciling when this is called, and the node pool it inserts has no
// CurrentNodeSetID, so the node-pool controller treats the pool's config as
// changed, mints its own current NodeSet, and then garbage-collects every
// non-current NodeSet that has no nodes -- the ListNodeSets sweep in
// nodepool.Controller.reconcile. Committing the NodeSet before its
// nodes exposes exactly that state, and the controller deletes the NodeSet out
// from under the pending CreateNode calls -- surfacing as an
// fk_nodes_node_set_id violation. Inserting the set and its nodes atomically
// means the childless window is never visible outside this transaction.
func seedTeardownScenario(ctx context.Context, database db.DB, scenario TeardownScenario) error {
	return database.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		return seedTeardownScenarioTx(ctx, tx, scenario)
	})
}

func seedTeardownScenarioTx(ctx context.Context, tx db.Tx, scenario TeardownScenario) error {
	siteID := uuid.New()
	cluster := db.Cluster{
		ID:                scenario.ClusterID,
		Name:              "cwe-gofail-" + scenario.Name,
		KubernetesVersion: "1.30.0",
		Conditions: db.ClusterConditions{
			Provisioned: db.NewCondition(true, "seeded"),
		},
	}
	cluster.SetNicoProvider(db.NicoClusterProvider{
		OrgID:  "cwe-gofail-org",
		SiteID: siteID,
		VpcID:  uuid.New(),
	})
	if _, err := tx.CreateCluster(ctx, &cluster); err != nil {
		return fmt.Errorf("create cluster: %w", err)
	}

	nodePoolID := uuid.New()
	nodeSetID := uuid.New()
	instanceTypeID := uuid.New()

	nodePool := db.NodePool{
		ID:                   nodePoolID,
		ClusterID:            scenario.ClusterID,
		Name:                 "cp",
		NodeNamePrefix:       "cp",
		ControlPlane:         true,
		DesiredCount:         2,
		SystemReservedMemory: "16Gi",
		NodeLabels:           []string{},
		Conditions: db.NodePoolConditions{
			Provisioned: db.NewCondition(true, "seeded"),
		},
	}
	nodePool.SetNico(db.NicoNodePoolProvider{
		InstanceTypeID:        instanceTypeID,
		OSProfile:             "test",
		ConfigProfile:         TeardownHookConfigProfile,
		SubnetIDs:             []uuid.UUID{},
		VpcPrefixIDs:          []uuid.UUID{},
		SSHKeyGroupIDs:        []uuid.UUID{},
		DiskSelectionStrategy: db.DiskSelectionStrategySmallest,
	})
	if _, err := tx.CreateNodePool(ctx, &nodePool); err != nil {
		return fmt.Errorf("create node pool: %w", err)
	}

	nodeSet := db.NodeSet{
		ID:                   nodeSetID,
		NodePoolID:           nodePoolID,
		DesiredCount:         2,
		KubernetesVersion:    "1.30.0",
		SystemReservedMemory: "16Gi",
	}
	nodeSet.SetNico(db.NicoNodePoolProvider{
		OSProfile:             "test",
		ConfigProfile:         TeardownHookConfigProfile,
		SubnetIDs:             []uuid.UUID{},
		VpcPrefixIDs:          []uuid.UUID{},
		DiskSelectionStrategy: db.DiskSelectionStrategySmallest,
	})
	if _, err := tx.CreateNodeSet(ctx, &nodeSet); err != nil {
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
		dbNode := db.Node{
			ID:           node.id,
			ClusterID:    scenario.ClusterID,
			NodePoolID:   nodePoolID,
			NodeSetID:    nodeSetID,
			ControlPlane: true,
			Name:         node.name,
			IP:           "127.0.0.1",
			Conditions: db.NodeConditions{
				Joined:          db.NewCondition(true, "seeded"),
				KubernetesReady: db.NewCondition(true, "seeded"),
				NodeAgentReady:  db.NewCondition(true, "seeded"),
			},
		}
		dbNode.SetNico(db.NicoNodeProvider{
			InstanceTypeID: instanceTypeID,
		})
		if _, err := tx.CreateNode(ctx, &dbNode); err != nil {
			return fmt.Errorf("create node %s: %w", node.name, err)
		}
	}

	return nil
}
