// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dbmetrics_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	io_prometheus_client "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/dbmetrics"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

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

func (te *collectorTestEnv) createCluster(ctx context.Context, t *testing.T, name string) db.Cluster {
	t.Helper()
	cluster := db.Cluster{
		ID:                uuid.New(),
		Name:              name,
		KubernetesVersion: "1.31.0",
	}
	cluster.SetNicoProvider(db.NicoClusterProvider{OrgID: "test-org", SiteID: te.siteID, VpcID: uuid.New()})
	created, err := te.database.CreateCluster(ctx, &cluster)
	require.NoError(t, err)
	return created
}

func (te *collectorTestEnv) createNodePool(ctx context.Context, t *testing.T, cluster db.Cluster, name string) db.NodePool {
	t.Helper()
	np := db.NodePool{
		ID:                 uuid.New(),
		ClusterID:          cluster.ID,
		Name:               name,
		NodeNamePrefix:     "n",
		ControlPlane:       false,
		KubeReservedMemory: "250Mi",
		NodeLabels:         map[string]string{},
		Nico: &db.NicoNodePoolProvider{
			SubnetIDs:      []uuid.UUID{uuid.New()},
			VpcPrefixIDs:   []uuid.UUID{uuid.New()},
			InstanceTypeID: uuid.New(),
			SSHKeyGroupIDs: []uuid.UUID{},
			OSProfile:      "test-os",
			ConfigProfile:  "test-config",
		},
	}
	created, err := te.database.CreateNodePool(ctx, &np)
	require.NoError(t, err)
	return created
}

func (te *collectorTestEnv) createNodeSet(ctx context.Context, t *testing.T, np db.NodePool) db.NodeSet {
	t.Helper()
	ns := db.NodeSet{
		ID:                   uuid.New(),
		NodePoolID:           np.ID,
		SystemReservedMemory: "16Gi",
		KubeReservedMemory:   "250Mi",
		Nico: &db.NicoNodePoolProvider{
			OSProfile:     "test-os",
			ConfigProfile: "test-config",
			SubnetIDs:     []uuid.UUID{uuid.New()},
			VpcPrefixIDs:  []uuid.UUID{uuid.New()},
		},
	}
	created, err := te.database.CreateNodeSet(ctx, &ns)
	require.NoError(t, err)
	return created
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func findMetricFamily(families []*io_prometheus_client.MetricFamily, name string) *io_prometheus_client.MetricFamily {
	for _, f := range families {
		if f.GetName() == name {
			return f
		}
	}
	return nil
}

func labelsAsMap(m *io_prometheus_client.Metric) map[string]string {
	out := make(map[string]string)
	for _, lp := range m.GetLabel() {
		out[lp.GetName()] = lp.GetValue()
	}
	return out
}

// TestUnit_Collector_ClusterNodePoolNodeSetInfo tests the nvke_controller_cluster_info, nvke_controller_nodepool_info,
// and nvke_controller_nodeset_info metrics introduced for the combined cluster/node pool/node set info collector.
func TestUnit_Collector_ClusterNodePoolNodeSetInfo(t *testing.T) {
	ctx := context.Background()

	t.Run("emits no info metrics for empty database", func(t *testing.T) {
		te := setupCollectorTestEnv(ctx, t)
		collector := dbmetrics.NewCollector(testLogger(), te.database)

		reg := prometheus.NewPedanticRegistry()
		require.NoError(t, reg.Register(collector))

		families, err := reg.Gather()
		require.NoError(t, err)

		clusterInfo := findMetricFamily(families, "nvke_controller_cluster_info")
		nodePoolInfo := findMetricFamily(families, "nvke_controller_nodepool_info")
		nodeSetInfo := findMetricFamily(families, "nvke_controller_nodeset_info")

		assert.Nil(t, clusterInfo, "no clusters so no nvke_controller_cluster_info")
		assert.Nil(t, nodePoolInfo, "no node pools so no nvke_controller_nodepool_info")
		assert.Nil(t, nodeSetInfo, "no node sets so no nvke_controller_nodeset_info")
	})

	t.Run("emits one nvke_controller_cluster_info per cluster with expected labels", func(t *testing.T) {
		te := setupCollectorTestEnv(ctx, t)
		cluster := te.createCluster(ctx, t, "info-cluster")

		collector := dbmetrics.NewCollector(testLogger(), te.database)
		reg := prometheus.NewPedanticRegistry()
		require.NoError(t, reg.Register(collector))

		families, err := reg.Gather()
		require.NoError(t, err)

		f := findMetricFamily(families, "nvke_controller_cluster_info")
		require.NotNil(t, f)
		require.Len(t, f.GetMetric(), 1)

		labels := labelsAsMap(f.GetMetric()[0])
		nicoProvider, err := cluster.NicoProvider()
		require.NoError(t, err)
		assert.Equal(t, "info-cluster", labels["name"])
		assert.Equal(t, nicoProvider.OrgID, labels["org_id"])
		assert.Equal(t, nicoProvider.SiteID.String(), labels["site_id"])
		assert.Equal(t, nicoProvider.VpcID.String(), labels["vpc_id"])
	})

	t.Run("emits one nvke_controller_nodepool_info per node pool with cluster and pool labels", func(t *testing.T) {
		te := setupCollectorTestEnv(ctx, t)
		cluster := te.createCluster(ctx, t, "np-cluster")
		np := te.createNodePool(ctx, t, cluster, "pool-a")

		collector := dbmetrics.NewCollector(testLogger(), te.database)
		reg := prometheus.NewPedanticRegistry()
		require.NoError(t, reg.Register(collector))

		families, err := reg.Gather()
		require.NoError(t, err)

		f := findMetricFamily(families, "nvke_controller_nodepool_info")
		require.NotNil(t, f)
		require.Len(t, f.GetMetric(), 1)

		labels := labelsAsMap(f.GetMetric()[0])
		assert.Equal(t, "pool-a", labels["name"])
		assert.Equal(t, "np-cluster", labels["tenant_cluster"])
		assert.Equal(t, np.ID.String(), labels["id"])
		assert.Equal(t, cluster.ID.String(), labels["cluster_id"])
		nicoProvider, err := cluster.NicoProvider()
		require.NoError(t, err)
		assert.Equal(t, nicoProvider.OrgID, labels["org_id"])
		assert.Equal(t, nicoProvider.SiteID.String(), labels["site_id"])
		assert.Equal(t, nicoProvider.VpcID.String(), labels["vpc_id"])
		assert.Equal(t, "250Mi", labels["kube_reserved_memory"])
	})

	t.Run("emits one nvke_controller_nodeset_info per node set with cluster and node pool names", func(t *testing.T) {
		te := setupCollectorTestEnv(ctx, t)
		cluster := te.createCluster(ctx, t, "ns-cluster")
		np := te.createNodePool(ctx, t, cluster, "ns-pool")
		ns := te.createNodeSet(ctx, t, np)

		collector := dbmetrics.NewCollector(testLogger(), te.database)
		reg := prometheus.NewPedanticRegistry()
		require.NoError(t, reg.Register(collector))

		families, err := reg.Gather()
		require.NoError(t, err)

		f := findMetricFamily(families, "nvke_controller_nodeset_info")
		require.NotNil(t, f)
		require.Len(t, f.GetMetric(), 1)

		labels := labelsAsMap(f.GetMetric()[0])
		assert.Equal(t, "ns-cluster", labels["tenant_cluster"])
		assert.Equal(t, "ns-pool", labels["node_pool_name"])
		assert.Equal(t, ns.ID.String(), labels["id"])
		assert.Equal(t, np.ID.String(), labels["node_pool_id"])
		assert.Equal(t, "250Mi", labels["kube_reserved_memory"])
	})

	t.Run("emits correct count of info metrics for multiple clusters pools and sets", func(t *testing.T) {
		te := setupCollectorTestEnv(ctx, t)
		c1 := te.createCluster(ctx, t, "c1")
		c2 := te.createCluster(ctx, t, "c2")
		te.createNodePool(ctx, t, c1, "p1")
		np2a := te.createNodePool(ctx, t, c2, "p2a")
		np2b := te.createNodePool(ctx, t, c2, "p2b")
		te.createNodeSet(ctx, t, np2a)
		te.createNodeSet(ctx, t, np2a)
		te.createNodeSet(ctx, t, np2b)

		collector := dbmetrics.NewCollector(testLogger(), te.database)
		reg := prometheus.NewPedanticRegistry()
		require.NoError(t, reg.Register(collector))

		families, err := reg.Gather()
		require.NoError(t, err)

		clusterF := findMetricFamily(families, "nvke_controller_cluster_info")
		nodePoolF := findMetricFamily(families, "nvke_controller_nodepool_info")
		nodeSetF := findMetricFamily(families, "nvke_controller_nodeset_info")

		require.NotNil(t, clusterF)
		require.NotNil(t, nodePoolF)
		require.NotNil(t, nodeSetF)
		assert.Len(t, clusterF.GetMetric(), 2, "one per cluster")
		assert.Len(t, nodePoolF.GetMetric(), 3, "one per node pool")
		assert.Len(t, nodeSetF.GetMetric(), 3, "one per node set")
	})

	t.Run("loads all pools and node sets with one bulk query each", func(t *testing.T) {
		cluster1 := db.Cluster{ID: uuid.New(), Name: "c1", ProviderType: db.ProviderTypeCAPI}
		cluster2 := db.Cluster{ID: uuid.New(), Name: "c2", ProviderType: db.ProviderTypeCAPI}
		pool1 := db.NodePool{ID: uuid.New(), ClusterID: cluster1.ID, Name: "p1"}
		pool2 := db.NodePool{ID: uuid.New(), ClusterID: cluster2.ID, Name: "p2"}
		nodeSet1 := db.NodeSet{ID: uuid.New(), NodePoolID: pool1.ID}
		nodeSet2 := db.NodeSet{ID: uuid.New(), NodePoolID: pool2.ID}

		mdb := &testutil.MockDB{}
		mdb.On("ListClusters", mock.Anything, db.ClusterFilters{}).Return([]db.Cluster{cluster1, cluster2}, nil).Once()
		mdb.On("ListNodePools", mock.Anything, db.NodePoolFilters{}).Return([]db.NodePool{pool1, pool2}, nil).Once()
		mdb.On("ListNodeSets", mock.Anything, db.NodeSetFilters{}).Return([]db.NodeSet{nodeSet1, nodeSet2}, nil).Once()
		mdb.On("ListNodeConditions", mock.Anything).Return([]db.NodeConditionStatus{}, nil).Once()
		mdb.On("ListNodePoolMetrics", mock.Anything).Return([]db.NodePoolMetrics{}, nil).Once()
		mdb.On("ListNodeInfo", mock.Anything).Return([]db.NodeInfo{}, nil).Once()
		mdb.On("ListNodeLifecycleElapsed", mock.Anything).Return([]db.NodeLifecycleElapsed{}, nil).Once()

		collector := dbmetrics.NewCollector(testLogger(), mdb)
		reg := prometheus.NewPedanticRegistry()
		require.NoError(t, reg.Register(collector))

		families, err := reg.Gather()
		require.NoError(t, err)
		require.Len(t, findMetricFamily(families, "nvke_controller_cluster_info").GetMetric(), 2)
		require.Len(t, findMetricFamily(families, "nvke_controller_nodepool_info").GetMetric(), 2)
		require.Len(t, findMetricFamily(families, "nvke_controller_nodeset_info").GetMetric(), 2)
		mdb.AssertExpectations(t)
	})
}

// newMockCollector builds a Collector backed by a MockDB that returns the given rows
// from ListNodeLifecycleElapsed and an empty result set from everything else.
func newMockCollector(t *testing.T, rows []db.NodeLifecycleElapsed, queryErr error) *prometheus.Registry {
	t.Helper()

	mdb := &testutil.MockDB{}

	// Stub all other collectors to return empty results so they don't interfere.
	// ListClusters returns empty → collectClusterNodePoolAndNodeSetInfo short-circuits
	// (so ListNodePools/ListNodeSets are never called in tests).
	mdb.On("ListClusters", mock.Anything, mock.Anything).Return([]db.Cluster{}, nil)
	mdb.On("ListNodeConditions", mock.Anything).Return([]db.NodeConditionStatus{}, nil)
	mdb.On("ListNodePoolMetrics", mock.Anything).Return([]db.NodePoolMetrics{}, nil)
	mdb.On("ListNodeInfo", mock.Anything).Return([]db.NodeInfo{}, nil)

	if queryErr != nil {
		mdb.On("ListNodeLifecycleElapsed", mock.Anything).Return([]db.NodeLifecycleElapsed(nil), queryErr)
	} else {
		mdb.On("ListNodeLifecycleElapsed", mock.Anything).Return(rows, nil)
	}

	collector := dbmetrics.NewCollector(testLogger(), mdb)
	reg := prometheus.NewPedanticRegistry()
	require.NoError(t, reg.Register(collector))
	return reg
}

func findLifecycleMetric(families []*io_prometheus_client.MetricFamily, clusterName, state string) *io_prometheus_client.Metric {
	f := findMetricFamily(families, "nvke_controller_node_lifecycle_elapsed_seconds")
	if f == nil {
		return nil
	}
	for _, m := range f.GetMetric() {
		labels := labelsAsMap(m)
		if labels["tenant_cluster"] == clusterName && labels["state"] == state {
			return m
		}
	}
	return nil
}

// TestUnit_Collector_NodeLifecycleElapsed tests nvke_controller_node_lifecycle_elapsed_seconds.
// It uses a MockDB so results are independent of the SQLite-incompatible PostgreSQL
// JSONB/EXTRACT query used in production.
func TestUnit_Collector_NodeLifecycleElapsed(t *testing.T) {
	t.Run("emits no series when query returns empty", func(t *testing.T) {
		reg := newMockCollector(t, []db.NodeLifecycleElapsed{}, nil)
		families, err := reg.Gather()
		require.NoError(t, err)
		assert.Nil(t, findMetricFamily(families, "nvke_controller_node_lifecycle_elapsed_seconds"))
	})

	t.Run("emits no series when query errors", func(t *testing.T) {
		reg := newMockCollector(t, nil, errors.New("db unavailable"))
		families, err := reg.Gather()
		require.NoError(t, err) // Collect errors are logged, not propagated
		assert.Nil(t, findMetricFamily(families, "nvke_controller_node_lifecycle_elapsed_seconds"))
	})

	t.Run("emits correct labels for a pending worker node", func(t *testing.T) {
		rows := []db.NodeLifecycleElapsed{
			{ClusterName: "my-cluster", NodeName: "node-1", NodeType: "worker", State: "pending", ElapsedSecs: 42.5},
		}
		reg := newMockCollector(t, rows, nil)
		families, err := reg.Gather()
		require.NoError(t, err)

		m := findLifecycleMetric(families, "my-cluster", "pending")
		require.NotNil(t, m, "expected gauge series for pending worker")
		assert.InDelta(t, 42.5, m.GetGauge().GetValue(), 0.001)

		labels := labelsAsMap(m)
		assert.Equal(t, "node-1", labels["node"])
		assert.Equal(t, "worker", labels["node_type"])
	})

	t.Run("emits correct labels for a provisioning cp node", func(t *testing.T) {
		rows := []db.NodeLifecycleElapsed{
			{ClusterName: "cp-cluster", NodeName: "cp-0", NodeType: "cp", State: "provisioning", ElapsedSecs: 120.0},
		}
		reg := newMockCollector(t, rows, nil)
		families, err := reg.Gather()
		require.NoError(t, err)

		m := findLifecycleMetric(families, "cp-cluster", "provisioning")
		require.NotNil(t, m)
		assert.Equal(t, "cp", labelsAsMap(m)["node_type"])
		assert.InDelta(t, 120.0, m.GetGauge().GetValue(), 0.001)
	})

	t.Run("emits correct labels for a deleting node", func(t *testing.T) {
		rows := []db.NodeLifecycleElapsed{
			{ClusterName: "del-cluster", NodeName: "node-x", NodeType: "worker", State: "deleting", ElapsedSecs: 300.0},
		}
		reg := newMockCollector(t, rows, nil)
		families, err := reg.Gather()
		require.NoError(t, err)

		m := findLifecycleMetric(families, "del-cluster", "deleting")
		require.NotNil(t, m)
		assert.InDelta(t, 300.0, m.GetGauge().GetValue(), 0.001)
	})

	t.Run("emits correct labels for a node marked for recycling", func(t *testing.T) {
		rows := []db.NodeLifecycleElapsed{
			{ClusterName: "my-cluster", NodeName: "node-7", NodeType: "worker", State: "markedForRecycling", ElapsedSecs: 86400.0},
		}
		reg := newMockCollector(t, rows, nil)
		families, err := reg.Gather()
		require.NoError(t, err)

		m := findLifecycleMetric(families, "my-cluster", "markedForRecycling")
		require.NotNil(t, m, "expected gauge series for markedForRecycling worker")
		assert.InDelta(t, 86400.0, m.GetGauge().GetValue(), 0.001)

		labels := labelsAsMap(m)
		assert.Equal(t, "node-7", labels["node"])
		assert.Equal(t, "worker", labels["node_type"])
	})

	t.Run("emits one series per row across multiple clusters", func(t *testing.T) {
		rows := []db.NodeLifecycleElapsed{
			{ClusterName: "c1", NodeName: "n1", NodeType: "worker", State: "pending", ElapsedSecs: 10},
			{ClusterName: "c1", NodeName: "n2", NodeType: "worker", State: "provisioning", ElapsedSecs: 20},
			{ClusterName: "c2", NodeName: "n3", NodeType: "cp", State: "deleting", ElapsedSecs: 30},
		}
		reg := newMockCollector(t, rows, nil)
		families, err := reg.Gather()
		require.NoError(t, err)

		f := findMetricFamily(families, "nvke_controller_node_lifecycle_elapsed_seconds")
		require.NotNil(t, f)
		assert.Len(t, f.GetMetric(), 3, "one series per row")
	})
}
