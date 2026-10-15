// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package metrics_test

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/metrics"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

// --- Test helpers ---

type collectorTestEnv struct {
	database db.DB
	siteID   uuid.UUID
}

func setupCollectorTestEnv(ctx context.Context, t *testing.T) *collectorTestEnv {
	t.Helper()

	bunDB := testutil.SetupDB(ctx, t,
		(*db.Cluster)(nil),
		(*db.NodePool)(nil),
		(*db.NodeSet)(nil),
		(*db.Node)(nil),
	)
	database := db.NewDB(bunDB)

	return &collectorTestEnv{database: database, siteID: uuid.New()}
}

func (te *collectorTestEnv) createCluster(ctx context.Context, t *testing.T, name string, teleportEnabled bool) db.Cluster {
	t.Helper()
	cluster := db.Cluster{
		ID:                uuid.New(),
		Name:              name,
		KubernetesVersion: "1.31.0",
		TeleportEnabled:   teleportEnabled,
	}
	cluster.SetNicoProvider(db.NicoClusterProvider{
		OrgID:  "test-org",
		SiteID: te.siteID,
		VpcID:  uuid.New(),
	})
	created, err := te.database.CreateCluster(ctx, &cluster)
	require.NoError(t, err)
	return created
}

func (te *collectorTestEnv) createNodePool(ctx context.Context, t *testing.T, cluster db.Cluster, name string) db.NodePool {
	t.Helper()
	np := db.NodePool{
		ID:             uuid.New(),
		ClusterID:      cluster.ID,
		Name:           name,
		NodeNamePrefix: "n",
		NodeLabels:     map[string]string{},
		Nico: &db.NicoNodePoolProvider{
			SubnetIDs:      []uuid.UUID{uuid.New()},
			VpcPrefixIDs:   []uuid.UUID{uuid.New()},
			InstanceTypeID: uuid.New(),
			SSHKeyGroupIDs: []uuid.UUID{},
			Release:        "test-os",
			ConfigProfile:  "test-config",
		},
	}
	created, err := te.database.CreateNodePool(ctx, &np)
	require.NoError(t, err)
	return created
}

func (te *collectorTestEnv) createNodeSet(ctx context.Context, t *testing.T, np db.NodePool, teleportEnabled bool) db.NodeSet {
	t.Helper()
	ns := db.NodeSet{
		ID:                   uuid.New(),
		NodePoolID:           np.ID,
		SystemReservedMemory: "16Gi",
		TeleportEnabled:      teleportEnabled,
		Nico: &db.NicoNodePoolProvider{
			Release:       "test-os",
			ConfigProfile: "test-config",
		},
	}
	created, err := te.database.CreateNodeSet(ctx, &ns)
	require.NoError(t, err)
	return created
}

func (te *collectorTestEnv) createEligibleNode(ctx context.Context, t *testing.T, np db.NodePool, ns db.NodeSet, name string) db.Node {
	t.Helper()
	node := db.Node{
		ID:                     uuid.New(),
		SpireAttestationMethod: db.SpireAttestationMethodTPM,
		NodePoolID:             np.ID,
		NodeSetID:              ns.ID,
		Name:                   name,
		IP:                     "10.0.0.1",
		Nico: &db.NicoNodeProvider{
			ForgeInstanceID: "forge-" + name,
			ForgeMachineID:  "machine-" + name,
		},
	}
	created, err := te.database.CreateNode(ctx, &node)
	require.NoError(t, err)
	return created
}

func (te *collectorTestEnv) markNodeRegistered(ctx context.Context, t *testing.T, nodeID uuid.UUID) {
	t.Helper()
	node, err := te.database.GetNode(ctx, nodeID)
	require.NoError(t, err)
	node.Conditions.TeleportRegistered = db.NewCondition(true, "Registered")
	_, err = te.database.UpdateNode(ctx, &node)
	require.NoError(t, err)
}

func (te *collectorTestEnv) markNodeDeleting(ctx context.Context, t *testing.T, nodeID uuid.UUID) {
	t.Helper()
	node, err := te.database.GetNode(ctx, nodeID)
	require.NoError(t, err)
	node.Conditions.Deleting = db.NewCondition(true, "Deleting")
	_, err = te.database.UpdateNode(ctx, &node)
	require.NoError(t, err)
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// --- Tests for ListTeleportRolloutStats (DB query) ---

func TestListTeleportRolloutStats(t *testing.T) {
	ctx := context.Background()

	t.Run("returns empty for empty database", func(t *testing.T) {
		te := setupCollectorTestEnv(ctx, t)
		// No clusters or nodes at all
		rows, err := te.database.ListTeleportRolloutStats(ctx)
		require.NoError(t, err)
		assert.Empty(t, rows)
	})

	t.Run("returns row for cluster with node pool but no nodes", func(t *testing.T) {
		te := setupCollectorTestEnv(ctx, t)
		cluster := te.createCluster(ctx, t, "cluster-a", true)
		np := te.createNodePool(ctx, t, cluster, "pool-1")
		te.createNodeSet(ctx, t, np, true)

		rows, err := te.database.ListTeleportRolloutStats(ctx)
		require.NoError(t, err)
		require.Len(t, rows, 1)

		assert.Equal(t, te.siteID, rows[0].SiteID)
		assert.Equal(t, cluster.ID, rows[0].ClusterID)
		assert.Equal(t, "cluster-a", rows[0].ClusterName)
		assert.True(t, rows[0].ClusterEnabled)
		assert.Equal(t, np.ID, rows[0].NodePoolID)
		assert.Equal(t, "pool-1", rows[0].NodePoolName)
		assert.True(t, rows[0].NodePoolEnabled)
		assert.Equal(t, 0, rows[0].NodesEligible)
		assert.Equal(t, 0, rows[0].NodesRegistered)
	})

	t.Run("counts eligible and registered nodes correctly", func(t *testing.T) {
		te := setupCollectorTestEnv(ctx, t)
		cluster := te.createCluster(ctx, t, "cluster-a", true)
		np := te.createNodePool(ctx, t, cluster, "pool-1")
		ns := te.createNodeSet(ctx, t, np, true)

		// 3 eligible nodes, 1 registered
		te.createEligibleNode(ctx, t, np, ns, "node-1")
		node2 := te.createEligibleNode(ctx, t, np, ns, "node-2")
		te.createEligibleNode(ctx, t, np, ns, "node-3")
		te.markNodeRegistered(ctx, t, node2.ID)

		rows, err := te.database.ListTeleportRolloutStats(ctx)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Equal(t, 3, rows[0].NodesEligible)
		assert.Equal(t, 1, rows[0].NodesRegistered)
	})

	t.Run("excludes deleting nodes from counts", func(t *testing.T) {
		te := setupCollectorTestEnv(ctx, t)
		cluster := te.createCluster(ctx, t, "cluster-a", true)
		np := te.createNodePool(ctx, t, cluster, "pool-1")
		ns := te.createNodeSet(ctx, t, np, true)

		te.createEligibleNode(ctx, t, np, ns, "node-alive")
		deletingNode := te.createEligibleNode(ctx, t, np, ns, "node-deleting")
		te.markNodeDeleting(ctx, t, deletingNode.ID)

		rows, err := te.database.ListTeleportRolloutStats(ctx)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Equal(t, 1, rows[0].NodesEligible, "deleting node should be excluded")
	})

	t.Run("non-teleport node sets do not contribute to node counts", func(t *testing.T) {
		te := setupCollectorTestEnv(ctx, t)
		cluster := te.createCluster(ctx, t, "cluster-a", true)
		np := te.createNodePool(ctx, t, cluster, "pool-1")
		teleportNS := te.createNodeSet(ctx, t, np, true)
		noTeleportNS := te.createNodeSet(ctx, t, np, false)

		te.createEligibleNode(ctx, t, np, teleportNS, "teleport-node")
		te.createEligibleNode(ctx, t, np, noTeleportNS, "no-teleport-node")

		rows, err := te.database.ListTeleportRolloutStats(ctx)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Equal(t, 1, rows[0].NodesEligible, "only nodes in teleport-enabled node sets counted")
		assert.True(t, rows[0].NodePoolEnabled, "pool has at least one teleport-enabled node set")
	})

	t.Run("node pool enabled is false when no teleport node sets", func(t *testing.T) {
		te := setupCollectorTestEnv(ctx, t)
		cluster := te.createCluster(ctx, t, "cluster-a", true)
		np := te.createNodePool(ctx, t, cluster, "pool-1")
		te.createNodeSet(ctx, t, np, false)

		rows, err := te.database.ListTeleportRolloutStats(ctx)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.False(t, rows[0].NodePoolEnabled)
	})

	t.Run("cluster enabled reflects cluster.teleport_enabled", func(t *testing.T) {
		te := setupCollectorTestEnv(ctx, t)
		enabledCluster := te.createCluster(ctx, t, "enabled", true)
		disabledCluster := te.createCluster(ctx, t, "disabled", false)
		te.createNodePool(ctx, t, enabledCluster, "pool-e")
		te.createNodePool(ctx, t, disabledCluster, "pool-d")

		rows, err := te.database.ListTeleportRolloutStats(ctx)
		require.NoError(t, err)
		require.Len(t, rows, 2)

		rowByCluster := make(map[string]db.TeleportRolloutRow)
		for _, r := range rows {
			rowByCluster[r.ClusterName] = r
		}
		assert.True(t, rowByCluster["enabled"].ClusterEnabled)
		assert.False(t, rowByCluster["disabled"].ClusterEnabled)
	})

	t.Run("multiple clusters and pools produce correct rows", func(t *testing.T) {
		te := setupCollectorTestEnv(ctx, t)
		c1 := te.createCluster(ctx, t, "cluster-1", true)
		c2 := te.createCluster(ctx, t, "cluster-2", false)
		np1a := te.createNodePool(ctx, t, c1, "pool-a")
		np1b := te.createNodePool(ctx, t, c1, "pool-b")
		np2a := te.createNodePool(ctx, t, c2, "pool-a")

		ns1a := te.createNodeSet(ctx, t, np1a, true)
		te.createNodeSet(ctx, t, np1b, false)
		te.createNodeSet(ctx, t, np2a, false)

		te.createEligibleNode(ctx, t, np1a, ns1a, "node-1")
		node2 := te.createEligibleNode(ctx, t, np1a, ns1a, "node-2")
		te.markNodeRegistered(ctx, t, node2.ID)

		rows, err := te.database.ListTeleportRolloutStats(ctx)
		require.NoError(t, err)
		assert.Len(t, rows, 3, "one row per node pool")

		rowByKey := make(map[string]db.TeleportRolloutRow)
		for _, r := range rows {
			rowByKey[r.ClusterName+"/"+r.NodePoolName] = r
		}

		r1a := rowByKey["cluster-1/pool-a"]
		assert.True(t, r1a.ClusterEnabled)
		assert.True(t, r1a.NodePoolEnabled)
		assert.Equal(t, 2, r1a.NodesEligible)
		assert.Equal(t, 1, r1a.NodesRegistered)

		r1b := rowByKey["cluster-1/pool-b"]
		assert.True(t, r1b.ClusterEnabled)
		assert.False(t, r1b.NodePoolEnabled)
		assert.Equal(t, 0, r1b.NodesEligible)

		r2a := rowByKey["cluster-2/pool-a"]
		assert.False(t, r2a.ClusterEnabled)
		assert.False(t, r2a.NodePoolEnabled)
	})

	t.Run("teleport-scoped nodes are eligible regardless of provider", func(t *testing.T) {
		te := setupCollectorTestEnv(ctx, t)
		cluster := te.createCluster(ctx, t, "cluster-a", true)
		np := te.createNodePool(ctx, t, cluster, "pool-1")
		ns := te.createNodeSet(ctx, t, np, true)

		// Eligible node
		te.createEligibleNode(ctx, t, np, ns, "good-node")

		// CAPNICo node without a Forge instance ID is eligible.
		noForge := db.Node{
			SpireAttestationMethod: db.SpireAttestationMethodTPM,
			ID:                     uuid.New(), NodePoolID: np.ID, NodeSetID: ns.ID,
			Name: "no-forge",
			Nico: &db.NicoNodeProvider{ForgeInstanceID: "", ForgeMachineID: "m"},
			IP:   "10.0.0.1",
		}
		_, err := te.database.CreateNode(ctx, &noForge)
		require.NoError(t, err)

		pendingHash := db.Node{
			SpireAttestationMethod: db.SpireAttestationMethodTPM,
			ID:                     uuid.New(), NodePoolID: np.ID, NodeSetID: ns.ID,
			Name: "pending-hash",
			Nico: &db.NicoNodeProvider{ForgeInstanceID: "forge-x", ForgeMachineID: "m"},
			IP:   "10.0.0.1",
		}
		_, err = te.database.CreateNode(ctx, &pendingHash)
		require.NoError(t, err)

		rows, err := te.database.ListTeleportRolloutStats(ctx)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Equal(t, 3, rows[0].NodesEligible, "teleport-scoped nodes are eligible regardless of provider")
	})
}

// --- Tests for TeleportCollector (Prometheus output) ---

func TestTeleportCollector(t *testing.T) {
	ctx := context.Background()

	t.Run("emits no metrics for empty database", func(t *testing.T) {
		te := setupCollectorTestEnv(ctx, t)
		collector := metrics.NewTeleportCollector(testLogger(), te.database)

		reg := prometheus.NewPedanticRegistry()
		require.NoError(t, reg.Register(collector))

		families, err := reg.Gather()
		require.NoError(t, err)
		assert.Empty(t, families, "no clusters means no metrics")
	})

	t.Run("emits correct metrics for mixed state", func(t *testing.T) {
		te := setupCollectorTestEnv(ctx, t)
		cluster := te.createCluster(ctx, t, "prod-cluster", true)
		np := te.createNodePool(ctx, t, cluster, "gpu-pool")
		ns := te.createNodeSet(ctx, t, np, true)

		te.createEligibleNode(ctx, t, np, ns, "node-a")
		nodeB := te.createEligibleNode(ctx, t, np, ns, "node-b")
		te.createEligibleNode(ctx, t, np, ns, "node-c")
		te.markNodeRegistered(ctx, t, nodeB.ID)

		collector := metrics.NewTeleportCollector(testLogger(), te.database)
		reg := prometheus.NewPedanticRegistry()
		require.NoError(t, reg.Register(collector))

		families, err := reg.Gather()
		require.NoError(t, err)

		metricMap := make(map[string]float64)
		for _, f := range families {
			for _, m := range f.GetMetric() {
				metricMap[f.GetName()] = m.GetGauge().GetValue()
			}
		}

		assert.InDelta(t, 1, metricMap["nvke_controller_teleport_cluster_enabled"], 0.001)
		assert.InDelta(t, 1, metricMap["nvke_controller_teleport_node_pool_enabled"], 0.001)
		assert.InDelta(t, 3, metricMap["nvke_controller_teleport_nodes_eligible"], 0.001)
		assert.InDelta(t, 1, metricMap["nvke_controller_teleport_nodes_registered"], 0.001)
		assert.InDelta(t, 2, metricMap["nvke_controller_teleport_nodes_pending"], 0.001)
	})

	t.Run("deduplicates cluster metrics across node pools", func(t *testing.T) {
		te := setupCollectorTestEnv(ctx, t)
		cluster := te.createCluster(ctx, t, "shared-cluster", true)
		te.createNodePool(ctx, t, cluster, "pool-1")
		te.createNodePool(ctx, t, cluster, "pool-2")

		collector := metrics.NewTeleportCollector(testLogger(), te.database)
		reg := prometheus.NewPedanticRegistry()
		require.NoError(t, reg.Register(collector))

		families, err := reg.Gather()
		require.NoError(t, err)

		var clusterEnabledCount int
		var nodePoolEnabledCount int
		for _, f := range families {
			switch f.GetName() {
			case "nvke_controller_teleport_cluster_enabled":
				clusterEnabledCount = len(f.GetMetric())
			case "nvke_controller_teleport_node_pool_enabled":
				nodePoolEnabledCount = len(f.GetMetric())
			}
		}
		assert.Equal(t, 1, clusterEnabledCount, "one cluster metric despite two pools")
		assert.Equal(t, 2, nodePoolEnabledCount, "one metric per pool")
	})

	t.Run("emits correct labels", func(t *testing.T) {
		te := setupCollectorTestEnv(ctx, t)
		cluster := te.createCluster(ctx, t, "label-test", true)
		np := te.createNodePool(ctx, t, cluster, "label-pool")
		te.createNodeSet(ctx, t, np, true)

		collector := metrics.NewTeleportCollector(testLogger(), te.database)
		reg := prometheus.NewPedanticRegistry()
		require.NoError(t, reg.Register(collector))

		families, err := reg.Gather()
		require.NoError(t, err)

		for _, f := range families {
			for _, m := range f.GetMetric() {
				labelMap := make(map[string]string)
				for _, lp := range m.GetLabel() {
					labelMap[lp.GetName()] = lp.GetValue()
				}

				assert.Equal(t, te.siteID.String(), labelMap["site_id"], "metric %s should have site_id", f.GetName())

				switch f.GetName() {
				case "nvke_controller_teleport_cluster_enabled":
					assert.Equal(t, cluster.ID.String(), labelMap["cluster_id"])
					assert.Equal(t, "label-test", labelMap["cluster_name"])
				case "nvke_controller_teleport_node_pool_enabled":
					assert.Equal(t, cluster.ID.String(), labelMap["cluster_id"])
					assert.Equal(t, np.ID.String(), labelMap["node_pool_id"])
					assert.Equal(t, "label-pool", labelMap["node_pool_name"])
				case "nvke_controller_teleport_nodes_eligible", "nvke_controller_teleport_nodes_registered", "nvke_controller_teleport_nodes_pending":
					assert.Equal(t, cluster.ID.String(), labelMap["cluster_id"])
					assert.Equal(t, np.ID.String(), labelMap["node_pool_id"])
				}
			}
		}
	})

	t.Run("disabled cluster emits 0", func(t *testing.T) {
		te := setupCollectorTestEnv(ctx, t)
		te.createCluster(ctx, t, "disabled-cluster", false)
		// Need at least one node pool for a row to appear
		// but cluster has no pools -> no rows. Add a pool.
		cluster := te.createCluster(ctx, t, "has-pool", false)
		te.createNodePool(ctx, t, cluster, "pool")

		collector := metrics.NewTeleportCollector(testLogger(), te.database)
		reg := prometheus.NewPedanticRegistry()
		require.NoError(t, reg.Register(collector))

		families, err := reg.Gather()
		require.NoError(t, err)

		for _, f := range families {
			if f.GetName() == "nvke_controller_teleport_cluster_enabled" {
				for _, m := range f.GetMetric() {
					labelMap := make(map[string]string)
					for _, lp := range m.GetLabel() {
						labelMap[lp.GetName()] = lp.GetValue()
					}
					if labelMap["cluster_name"] == "has-pool" {
						assert.InDelta(t, 0, m.GetGauge().GetValue(), 0.001)
					}
				}
			}
		}
	})
}
