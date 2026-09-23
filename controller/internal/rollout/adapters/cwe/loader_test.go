// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cwe_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/rollout/adapters/cwe"
)

// fakeDB is a minimal db.DB double for the Loader: only GetCluster and
// ListNodes are exercised. The embedded db.DB satisfies the rest of the
// interface and panics if any un-stubbed method is called.
type fakeDB struct {
	db.DB

	cluster    db.Cluster
	clusterErr error

	cpNodes  []db.Node
	allNodes []db.Node
	nodesErr error

	gotClusterID    uuid.UUID
	listCPFilter    *bool
	listAllFilter   *bool
	listNodesCalled int
}

func (f *fakeDB) GetCluster(_ context.Context, id uuid.UUID) (db.Cluster, error) {
	f.gotClusterID = id
	return f.cluster, f.clusterErr
}

func (f *fakeDB) ListNodes(_ context.Context, filters db.NodeFilters) ([]db.Node, error) {
	f.listNodesCalled++
	if f.nodesErr != nil {
		return nil, f.nodesErr
	}
	if filters.ControlPlane != nil && *filters.ControlPlane {
		f.listCPFilter = filters.ControlPlane
		return f.cpNodes, nil
	}
	f.listAllFilter = filters.ControlPlane // nil for the "all" query
	return f.allNodes, nil
}

func TestLoader_LoadCluster(t *testing.T) {
	clusterID := uuid.New()
	cp := db.Node{ID: uuid.New(), Name: "cp-0", IP: "10.0.0.11", ControlPlane: true, ClusterID: clusterID}
	worker := db.Node{ID: uuid.New(), Name: "worker-0", IP: "10.0.0.20", ClusterID: clusterID}

	fake := &fakeDB{
		cluster: db.Cluster{
			ID:                clusterID,
			Name:              "test",
			KubernetesVersion: "v1.30.0",
			ControlPlaneVIP:   "10.0.0.5",
		},
		cpNodes:  []db.Node{cp},
		allNodes: []db.Node{worker, cp},
	}

	loader := cwe.NewLoader(fake, config.Config{NodeAgentPort: "15005"})
	cluster, err := loader.LoadCluster(context.Background(), clusterID)
	require.NoError(t, err)

	assert.Equal(t, clusterID, fake.gotClusterID)
	assert.Equal(t, 2, fake.listNodesCalled, "expected one CP-filtered and one all-nodes query")
	require.NotNil(t, fake.listCPFilter)
	assert.True(t, *fake.listCPFilter, "control-plane query must filter ControlPlane=true")
	assert.Nil(t, fake.listAllFilter, "all-nodes query must not filter on ControlPlane")

	assert.Equal(t, clusterID, cluster.ID())
	assert.Equal(t, "test", cluster.Name())
	assert.Equal(t, "v1.30.0", cluster.KubernetesVersion())
	assert.Equal(t, "10.0.0.5", cluster.ControlPlaneVIP())

	require.Len(t, cluster.ControlPlaneNodes(), 1)
	assert.Equal(t, "cp-0", cluster.ControlPlaneNodes()[0].Name())
	assert.Equal(t, "10.0.0.11:15005", cluster.ControlPlaneNodes()[0].AgentEndpoint())
	assert.Len(t, cluster.Nodes(), 2)
}

func TestLoader_LoadCluster_GetClusterError(t *testing.T) {
	fake := &fakeDB{clusterErr: db.ErrNotFound}
	loader := cwe.NewLoader(fake, config.Config{NodeAgentPort: "15005"})

	cluster, err := loader.LoadCluster(context.Background(), uuid.New())
	require.Error(t, err)
	assert.Nil(t, cluster)
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestLoader_LoadCluster_ListNodesError(t *testing.T) {
	sentinel := errors.New("boom")
	fake := &fakeDB{nodesErr: sentinel}
	loader := cwe.NewLoader(fake, config.Config{NodeAgentPort: "15005"})

	cluster, err := loader.LoadCluster(context.Background(), uuid.New())
	require.Error(t, err)
	assert.Nil(t, cluster)
	assert.ErrorIs(t, err, sentinel)
}
