// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

// tenantTree is one cluster with a node pool, node set and node in it.
type tenantTree struct {
	cluster db.Cluster
	pool    db.NodePool
	set     db.NodeSet
	node    db.Node
}

// seedTenantTree inserts a tenantTree that tenant owns. An empty tenant leaves
// the cluster unowned.
func seedTenantTree(ctx context.Context, t *testing.T, bunDB bun.IDB, tenant string) tenantTree {
	t.Helper()
	cluster := newValidCluster(func(c *db.Cluster) { c.Tenant = tenant })
	tree := tenantTree{
		cluster: cluster,
		pool:    db.NodePool{ID: uuid.New(), ClusterID: cluster.ID, Name: "workers", NodeNamePrefix: "w", KubernetesVersion: "1.32.1"},
	}
	tree.set = db.NodeSet{ID: uuid.New(), NodePoolID: tree.pool.ID}
	tree.node = db.Node{ID: uuid.New(), Name: "node-" + uuid.NewString(), ClusterID: cluster.ID, NodePoolID: tree.pool.ID, NodeSetID: tree.set.ID, SpireAttestationMethod: db.SpireAttestationMethodTPM}
	for _, model := range []any{&tree.cluster, &tree.pool, &tree.set, &tree.node} {
		_, err := bunDB.NewInsert().Model(model).Exec(ctx)
		require.NoError(t, err)
	}
	return tree
}

func setupTenantDB(ctx context.Context, t *testing.T) (*bun.DB, db.DB) {
	t.Helper()
	bunDB := testutil.SetupDB(ctx, t, (*db.Cluster)(nil), (*db.NodePool)(nil), (*db.NodeSet)(nil), (*db.Node)(nil))
	return bunDB, db.NewDB(bunDB)
}

func TestTenantLookups(t *testing.T) {
	ctx := t.Context()
	bunDB, q := setupTenantDB(ctx, t)
	a := seedTenantTree(ctx, t, bunDB, "nca-a")
	unowned := seedTenantTree(ctx, t, bunDB, "")

	tests := []struct {
		name           string
		lookup         func(context.Context, uuid.UUID) (string, error)
		owned, unowned uuid.UUID
	}{
		{name: "cluster", lookup: q.GetClusterTenant, owned: a.cluster.ID, unowned: unowned.cluster.ID},
		{name: "node pool", lookup: q.GetNodePoolTenant, owned: a.pool.ID, unowned: unowned.pool.ID},
		{name: "node set", lookup: q.GetNodeSetTenant, owned: a.set.ID, unowned: unowned.set.ID},
		{name: "node", lookup: q.GetNodeTenant, owned: a.node.ID, unowned: unowned.node.ID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tenant, err := tt.lookup(ctx, tt.owned)
			require.NoError(t, err)
			assert.Equal(t, "nca-a", tenant)

			tenant, err = tt.lookup(ctx, tt.unowned)
			require.NoError(t, err)
			assert.Empty(t, tenant, "an unowned cluster has no tenant")

			_, err = tt.lookup(ctx, uuid.New())
			require.ErrorIs(t, err, db.ErrNotFound)
		})
	}
}

func TestListsFilterByTenant(t *testing.T) {
	ctx := t.Context()
	bunDB, q := setupTenantDB(ctx, t)
	a := seedTenantTree(ctx, t, bunDB, "nca-a")
	b := seedTenantTree(ctx, t, bunDB, "nca-b")
	unowned := seedTenantTree(ctx, t, bunDB, "")
	// mixed sits in a's cluster but in b's node pool.
	mixed := db.Node{ID: uuid.New(), Name: "node-" + uuid.NewString(), ClusterID: a.cluster.ID, NodePoolID: b.pool.ID, NodeSetID: b.set.ID, SpireAttestationMethod: db.SpireAttestationMethodTPM}
	_, err := bunDB.NewInsert().Model(&mixed).Exec(ctx)
	require.NoError(t, err)
	tenant := func(tenant string) *string { return &tenant }
	clusters := func(f db.ClusterFilters) ([]uuid.UUID, error) {
		rows, err := q.ListClusters(ctx, f)
		return idsOf(rows, func(c db.Cluster) uuid.UUID { return c.ID }), err
	}
	pools := func(f db.NodePoolFilters) ([]uuid.UUID, error) {
		rows, err := q.ListNodePools(ctx, f)
		return idsOf(rows, func(p db.NodePool) uuid.UUID { return p.ID }), err
	}
	sets := func(f db.NodeSetFilters) ([]uuid.UUID, error) {
		rows, err := q.ListNodeSets(ctx, f)
		return idsOf(rows, func(s db.NodeSet) uuid.UUID { return s.ID }), err
	}
	nodes := func(f db.NodeFilters) ([]uuid.UUID, error) {
		rows, err := q.ListNodes(ctx, f)
		return idsOf(rows, func(n db.Node) uuid.UUID { return n.ID }), err
	}

	tests := []struct {
		name string
		list func() ([]uuid.UUID, error)
		want []uuid.UUID
	}{
		{name: "clusters, no tenant filter", list: func() ([]uuid.UUID, error) { return clusters(db.ClusterFilters{}) }, want: []uuid.UUID{a.cluster.ID, b.cluster.ID, unowned.cluster.ID}},
		{name: "clusters, tenant", list: func() ([]uuid.UUID, error) { return clusters(db.ClusterFilters{Tenant: tenant("nca-a")}) }, want: []uuid.UUID{a.cluster.ID}},
		{name: "node pools, tenant", list: func() ([]uuid.UUID, error) { return pools(db.NodePoolFilters{Tenant: tenant("nca-a")}) }, want: []uuid.UUID{a.pool.ID}},
		{name: "node sets, tenant", list: func() ([]uuid.UUID, error) { return sets(db.NodeSetFilters{Tenant: tenant("nca-a")}) }, want: []uuid.UUID{a.set.ID}},
		{name: "nodes, tenant follows node.cluster_id", list: func() ([]uuid.UUID, error) { return nodes(db.NodeFilters{Tenant: tenant("nca-a")}) }, want: []uuid.UUID{a.node.ID, mixed.ID}},
		{name: "nodes, other tenant's ClusterID", list: func() ([]uuid.UUID, error) {
			return nodes(db.NodeFilters{Tenant: tenant("nca-a"), ClusterID: &b.cluster.ID})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.list()
			require.NoError(t, err)
			assert.ElementsMatch(t, tt.want, got)
		})
	}
}

func TestCreateClusterRecordsTenant(t *testing.T) {
	ctx := t.Context()
	bunDB, q := setupTenantDB(ctx, t)

	owned := newValidCluster(func(c *db.Cluster) { c.Tenant = "nca-a" })
	_, err := q.CreateCluster(ctx, &owned)
	require.NoError(t, err)
	got, err := q.GetCluster(ctx, owned.ID)
	require.NoError(t, err)
	assert.Equal(t, "nca-a", got.Tenant)

	unowned := newValidCluster()
	_, err = q.CreateCluster(ctx, &unowned)
	require.NoError(t, err)
	got, err = q.GetCluster(ctx, unowned.ID)
	require.NoError(t, err)
	assert.Empty(t, got.Tenant, "an empty tenant leaves the cluster unowned")
	var isNull bool
	require.NoError(t, bunDB.NewRaw("SELECT tenant IS NULL FROM clusters WHERE id = ?", unowned.ID).Scan(ctx, &isNull))
	assert.True(t, isNull, "an unowned cluster stores tenant as NULL")
}

// TestUpdateClusterKeepsTenant checks that UpdateCluster never writes tenant,
// so no update moves a cluster to another tenant.
func TestUpdateClusterKeepsTenant(t *testing.T) {
	ctx := t.Context()
	bunDB, q := setupTenantDB(ctx, t)
	a := seedTenantTree(ctx, t, bunDB, "nca-a")

	stale := a.cluster
	stale.Tenant = "nca-b"
	stale.KubernetesVersion = "1.35"
	updated, err := q.UpdateCluster(ctx, &stale)
	require.NoError(t, err)
	assert.Equal(t, "nca-a", updated.Tenant)

	got, err := q.GetCluster(ctx, a.cluster.ID)
	require.NoError(t, err)
	assert.Equal(t, "1.35", got.KubernetesVersion)
	assert.Equal(t, "nca-a", got.Tenant)
}

func idsOf[T any](rows []T, id func(T) uuid.UUID) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, id(row))
	}
	return ids
}
