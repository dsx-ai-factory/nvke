// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dbwrapper

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	crsource "sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile"
	dbfilters "github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile/filters"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func TestUnit_DBEnqueuesNodeAfterUpdate(t *testing.T) {
	node := db.Node{ID: uuid.New()}
	database := &testutil.MockDB{}
	database.On("ListNodes", mock.Anything, db.NodeFilters{}).Return([]db.Node{}, nil)
	database.On("GetNode", mock.Anything, node.ID).Return(node, nil)
	database.On("UpdateNode", mock.Anything, &node).Return(node, nil)
	wrapper := New(database, slog.Default())
	queue := startNodeSource(t, wrapper, dbfilters.Node{})

	updated, err := wrapper.UpdateNode(context.Background(), &node)

	require.NoError(t, err)
	assert.Equal(t, node, updated)
	assert.Equal(t, dbreconcile.NodeKey(node.ID), getQueueItem(t, queue))
	database.AssertExpectations(t)
}

func TestUnit_DBEnqueuesNodeAfterCreate(t *testing.T) {
	node := db.Node{ID: uuid.New()}
	database := &testutil.MockDB{}
	database.On("ListNodes", mock.Anything, db.NodeFilters{}).Return([]db.Node{}, nil)
	database.On("CreateNode", mock.Anything, &node).Return(node, nil)
	wrapper := New(database, slog.Default())
	queue := startNodeSource(t, wrapper, dbfilters.Node{})

	created, err := wrapper.CreateNode(context.Background(), &node)

	require.NoError(t, err)
	assert.Equal(t, node, created)
	assert.Equal(t, dbreconcile.NodeKey(node.ID), getQueueItem(t, queue))
	database.AssertExpectations(t)
}

func TestUnit_DBEnqueuesNodeAfterTeleportRegisteredUpdate(t *testing.T) {
	node := db.Node{ID: uuid.New()}
	condition := db.NewCondition(true, "Registered with Teleport")
	updatedNode := node
	updatedNode.Conditions.TeleportRegistered = condition
	database := &testutil.MockDB{}
	database.On("ListNodes", mock.Anything, db.NodeFilters{}).Return([]db.Node{}, nil)
	database.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
	database.On("UpdateNodeTeleportRegistered", mock.Anything, node.ID, condition).Return(nil).Once()
	database.On("GetNode", mock.Anything, node.ID).Return(updatedNode, nil).Once()
	wrapper := New(database, slog.Default())
	queue := startNodeSource(t, wrapper, dbfilters.Node{})

	err := wrapper.UpdateNodeTeleportRegistered(context.Background(), node.ID, condition)

	require.NoError(t, err)
	assert.Equal(t, dbreconcile.NodeKey(node.ID), getQueueItem(t, queue))
	database.AssertExpectations(t)
}

func TestUnit_DBNodeEnqueueFansOutToEachSource(t *testing.T) {
	node := db.Node{ID: uuid.New()}
	database := &testutil.MockDB{}
	database.On("ListNodes", mock.Anything, db.NodeFilters{}).Return([]db.Node{}, nil)
	database.On("CreateNode", mock.Anything, &node).Return(node, nil)
	wrapper := New(database, slog.Default())
	firstQueue := startNodeSource(t, wrapper, dbfilters.Node{})
	secondQueue := startNodeSource(t, wrapper, dbfilters.Node{})

	created, err := wrapper.CreateNode(context.Background(), &node)

	require.NoError(t, err)
	assert.Equal(t, node, created)
	assert.Equal(t, dbreconcile.NodeKey(node.ID), getQueueItem(t, firstQueue))
	assert.Equal(t, dbreconcile.NodeKey(node.ID), getQueueItem(t, secondQueue))
	database.AssertExpectations(t)
}

func TestUnit_DBTypedNodeSourceEnqueuesMappedKey(t *testing.T) {
	node := db.Node{ID: uuid.New(), Name: "worker-1"}
	database := &testutil.MockDB{}
	database.On("ListNodes", mock.Anything, db.NodeFilters{}).Return([]db.Node{}, nil)
	database.On("CreateNode", mock.Anything, &node).Return(node, nil)
	wrapper := New(database, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	source := NewMappedNodeSource[string](
		wrapper,
		dbfilters.Node{},
		func(_ context.Context, node db.Node) ([]string, error) {
			return []string{node.Name}, nil
		},
	)
	queue := startStringSource(ctx, t, source)

	created, err := wrapper.CreateNode(ctx, &node)

	require.NoError(t, err)
	assert.Equal(t, node, created)
	assert.Equal(t, "worker-1", getStringQueueItem(t, queue))
	database.AssertExpectations(t)
}

func TestUnit_DBTypedNodeSourceUnregistersWhenContextCancels(t *testing.T) {
	node := db.Node{ID: uuid.New(), Name: "worker-1"}
	database := &testutil.MockDB{}
	database.On("ListNodes", mock.Anything, db.NodeFilters{}).Return([]db.Node{}, nil)
	database.On("CreateNode", mock.Anything, &node).Return(node, nil)
	wrapper := New(database, slog.Default())
	sourceCtx, cancelSource := context.WithCancel(context.Background())
	source := NewMappedNodeSource[string](
		wrapper,
		dbfilters.Node{},
		func(_ context.Context, node db.Node) ([]string, error) {
			return []string{node.Name}, nil
		},
	)
	queue := startStringSource(sourceCtx, t, source)

	cancelSource()
	require.Eventually(t, func() bool {
		wrapper.lock.RLock()
		defer wrapper.lock.RUnlock()
		return len(wrapper.nodeSources) == 0
	}, time.Second, 10*time.Millisecond)

	created, err := wrapper.CreateNode(context.Background(), &node)

	require.NoError(t, err)
	assert.Equal(t, node, created)
	assertStringQueueEmpty(t, queue)
	database.AssertExpectations(t)
}

func TestUnit_DBTypedSourceUnregistersWhenContextCancels(t *testing.T) {
	type parameters struct {
		setup    func(*testutil.MockDB, *DB, context.Context) workqueue.TypedRateLimitingInterface[string]
		canceled func(*DB) bool
		create   func(context.Context, *testutil.MockDB, *DB) error
	}

	tests := map[string]parameters{
		"cluster": {
			setup: func(database *testutil.MockDB, wrapper *DB, ctx context.Context) workqueue.TypedRateLimitingInterface[string] {
				database.On("ListClusters", mock.Anything, db.ClusterFilters{}).Return([]db.Cluster{}, nil)
				source := NewMappedClusterSource[string](wrapper, dbfilters.Cluster{}, func(_ context.Context, cluster db.Cluster) ([]string, error) {
					return []string{cluster.Name}, nil
				})
				return startStringSource(ctx, t, source)
			},
			canceled: func(wrapper *DB) bool {
				wrapper.lock.RLock()
				defer wrapper.lock.RUnlock()
				return len(wrapper.clusterSources) == 0
			},
			create: func(ctx context.Context, database *testutil.MockDB, wrapper *DB) error {
				cluster := db.Cluster{ID: uuid.New(), Name: "tenant-1"}
				database.On("CreateCluster", mock.Anything, &cluster).Return(cluster, nil)
				_, err := wrapper.CreateCluster(ctx, &cluster)
				return err
			},
		},
		"node pool": {
			setup: func(database *testutil.MockDB, wrapper *DB, ctx context.Context) workqueue.TypedRateLimitingInterface[string] {
				database.On("ListNodePools", mock.Anything, db.NodePoolFilters{}).Return([]db.NodePool{}, nil)
				source := NewMappedNodePoolSource[string](wrapper, dbfilters.NodePool{}, func(_ context.Context, nodePool db.NodePool) ([]string, error) {
					return []string{nodePool.Name}, nil
				})
				return startStringSource(ctx, t, source)
			},
			canceled: func(wrapper *DB) bool {
				wrapper.lock.RLock()
				defer wrapper.lock.RUnlock()
				return len(wrapper.nodePoolSources) == 0
			},
			create: func(ctx context.Context, database *testutil.MockDB, wrapper *DB) error {
				nodePool := db.NodePool{ID: uuid.New(), Name: "pool-1"}
				database.On("CreateNodePool", mock.Anything, &nodePool).Return(nodePool, nil)
				_, err := wrapper.CreateNodePool(ctx, &nodePool)
				return err
			},
		},
		"node set": {
			setup: func(database *testutil.MockDB, wrapper *DB, ctx context.Context) workqueue.TypedRateLimitingInterface[string] {
				database.On("ListNodeSets", mock.Anything, db.NodeSetFilters{}).Return([]db.NodeSet{}, nil)
				source := NewMappedNodeSetSource[string](wrapper, dbfilters.NodeSet{}, func(_ context.Context, nodeSet db.NodeSet) ([]string, error) {
					return []string{nodeSet.OSProfile}, nil
				})
				return startStringSource(ctx, t, source)
			},
			canceled: func(wrapper *DB) bool {
				wrapper.lock.RLock()
				defer wrapper.lock.RUnlock()
				return len(wrapper.nodeSetSources) == 0
			},
			create: func(ctx context.Context, database *testutil.MockDB, wrapper *DB) error {
				nodeSet := db.NodeSet{ID: uuid.New(), OSProfile: "ubuntu-22.04"}
				database.On("CreateNodeSet", mock.Anything, &nodeSet).Return(nodeSet, nil)
				_, err := wrapper.CreateNodeSet(ctx, &nodeSet)
				return err
			},
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			database := &testutil.MockDB{}
			wrapper := New(database, slog.Default())
			sourceCtx, cancelSource := context.WithCancel(context.Background())
			queue := params.setup(database, wrapper, sourceCtx)

			cancelSource()
			require.Eventually(t, func() bool {
				return params.canceled(wrapper)
			}, time.Second, 10*time.Millisecond)

			require.NoError(t, params.create(context.Background(), database, wrapper))
			assertStringQueueEmpty(t, queue)
			database.AssertExpectations(t)
		})
	}
}

func TestUnit_DBSourceRegistersWhenStarted(t *testing.T) {
	type parameters struct {
		setup      func(*testutil.MockDB, *DB) crsource.TypedSource[string]
		registered func(*DB) bool
	}

	tests := map[string]parameters{
		"cluster": {
			setup: func(database *testutil.MockDB, wrapper *DB) crsource.TypedSource[string] {
				database.On("ListClusters", mock.Anything, db.ClusterFilters{}).Return([]db.Cluster{}, nil)
				return NewMappedClusterSource[string](wrapper, dbfilters.Cluster{}, func(_ context.Context, cluster db.Cluster) ([]string, error) {
					return []string{cluster.Name}, nil
				})
			},
			registered: func(wrapper *DB) bool {
				wrapper.lock.RLock()
				defer wrapper.lock.RUnlock()
				return len(wrapper.clusterSources) == 1
			},
		},
		"node pool": {
			setup: func(database *testutil.MockDB, wrapper *DB) crsource.TypedSource[string] {
				database.On("ListNodePools", mock.Anything, db.NodePoolFilters{}).Return([]db.NodePool{}, nil)
				return NewMappedNodePoolSource[string](wrapper, dbfilters.NodePool{}, func(_ context.Context, nodePool db.NodePool) ([]string, error) {
					return []string{nodePool.Name}, nil
				})
			},
			registered: func(wrapper *DB) bool {
				wrapper.lock.RLock()
				defer wrapper.lock.RUnlock()
				return len(wrapper.nodePoolSources) == 1
			},
		},
		"node set": {
			setup: func(database *testutil.MockDB, wrapper *DB) crsource.TypedSource[string] {
				database.On("ListNodeSets", mock.Anything, db.NodeSetFilters{}).Return([]db.NodeSet{}, nil)
				return NewMappedNodeSetSource[string](wrapper, dbfilters.NodeSet{}, func(_ context.Context, nodeSet db.NodeSet) ([]string, error) {
					return []string{nodeSet.OSProfile}, nil
				})
			},
			registered: func(wrapper *DB) bool {
				wrapper.lock.RLock()
				defer wrapper.lock.RUnlock()
				return len(wrapper.nodeSetSources) == 1
			},
		},
		"node": {
			setup: func(database *testutil.MockDB, wrapper *DB) crsource.TypedSource[string] {
				database.On("ListNodes", mock.Anything, db.NodeFilters{}).Return([]db.Node{}, nil)
				return NewMappedNodeSource[string](wrapper, dbfilters.Node{}, func(_ context.Context, node db.Node) ([]string, error) {
					return []string{node.Name}, nil
				})
			},
			registered: func(wrapper *DB) bool {
				wrapper.lock.RLock()
				defer wrapper.lock.RUnlock()
				return len(wrapper.nodeSources) == 1
			},
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			database := &testutil.MockDB{}
			wrapper := New(database, slog.Default())
			source := params.setup(database, wrapper)

			assert.False(t, params.registered(wrapper))

			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			queue := startStringSource(ctx, t, source)

			assert.True(t, params.registered(wrapper))
			assertStringQueueEmpty(t, queue)
			database.AssertExpectations(t)
		})
	}
}

func TestUnit_DBTypedClusterSourceEnqueuesMappedKey(t *testing.T) {
	cluster := db.Cluster{ID: uuid.New(), Name: "tenant-1"}
	database := &testutil.MockDB{}
	database.On("ListClusters", mock.Anything, db.ClusterFilters{}).Return([]db.Cluster{}, nil)
	database.On("CreateCluster", mock.Anything, &cluster).Return(cluster, nil)
	wrapper := New(database, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	source := NewMappedClusterSource[string](
		wrapper,
		dbfilters.Cluster{},
		func(_ context.Context, cluster db.Cluster) ([]string, error) {
			return []string{cluster.Name}, nil
		},
	)
	queue := startStringSource(ctx, t, source)

	created, err := wrapper.CreateCluster(ctx, &cluster)

	require.NoError(t, err)
	assert.Equal(t, cluster, created)
	assert.Equal(t, "tenant-1", getStringQueueItem(t, queue))
	database.AssertExpectations(t)
}

func TestUnit_DBTypedNodePoolSourceEnqueuesMappedKey(t *testing.T) {
	nodePool := db.NodePool{ID: uuid.New(), Name: "pool-1"}
	database := &testutil.MockDB{}
	database.On("ListNodePools", mock.Anything, db.NodePoolFilters{}).Return([]db.NodePool{}, nil)
	database.On("CreateNodePool", mock.Anything, &nodePool).Return(nodePool, nil)
	wrapper := New(database, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	source := NewMappedNodePoolSource[string](
		wrapper,
		dbfilters.NodePool{},
		func(_ context.Context, nodePool db.NodePool) ([]string, error) {
			return []string{nodePool.Name}, nil
		},
	)
	queue := startStringSource(ctx, t, source)

	created, err := wrapper.CreateNodePool(ctx, &nodePool)

	require.NoError(t, err)
	assert.Equal(t, nodePool, created)
	assert.Equal(t, "pool-1", getStringQueueItem(t, queue))
	database.AssertExpectations(t)
}

func TestUnit_DBTypedNodeSetSourceEnqueuesMappedKey(t *testing.T) {
	nodeSet := db.NodeSet{ID: uuid.New(), OSProfile: "ubuntu-22.04"}
	database := &testutil.MockDB{}
	database.On("ListNodeSets", mock.Anything, db.NodeSetFilters{}).Return([]db.NodeSet{}, nil)
	database.On("CreateNodeSet", mock.Anything, &nodeSet).Return(nodeSet, nil)
	wrapper := New(database, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	source := NewMappedNodeSetSource[string](
		wrapper,
		dbfilters.NodeSet{},
		func(_ context.Context, nodeSet db.NodeSet) ([]string, error) {
			return []string{nodeSet.OSProfile}, nil
		},
	)
	queue := startStringSource(ctx, t, source)

	created, err := wrapper.CreateNodeSet(ctx, &nodeSet)

	require.NoError(t, err)
	assert.Equal(t, nodeSet, created)
	assert.Equal(t, "ubuntu-22.04", getStringQueueItem(t, queue))
	database.AssertExpectations(t)
}

func TestUnit_DBDoesNotEnqueueNodeUpdateWhenSourceFiltersDoNotMatch(t *testing.T) {
	node := db.Node{ID: uuid.New()}
	filters := dbfilters.Node{
		Provisioning: ptr.To(true),
		Deleting:     ptr.To(false),
	}
	database := &testutil.MockDB{}
	database.On("ListNodes", mock.Anything, db.NodeFilters{}).Return([]db.Node{}, nil)
	database.On("GetNode", mock.Anything, node.ID).Return(node, nil)
	database.On("UpdateNode", mock.Anything, &node).Return(node, nil)
	wrapper := New(database, slog.Default())
	queue := startNodeSource(t, wrapper, filters)

	updated, err := wrapper.UpdateNode(context.Background(), &node)

	require.NoError(t, err)
	assert.Equal(t, node, updated)
	assertQueueEmpty(t, queue)
	database.AssertExpectations(t)
}

func TestUnit_DBEnqueuesNodeUpdateWhenSourceFiltersMatch(t *testing.T) {
	node := db.Node{
		ID: uuid.New(),
		Conditions: db.NodeConditions{
			Provisioning: db.NewCondition(true, ""),
		},
	}
	filters := dbfilters.Node{
		Provisioning: ptr.To(true),
		Deleting:     ptr.To(false),
	}
	database := &testutil.MockDB{}
	database.On("ListNodes", mock.Anything, db.NodeFilters{}).Return([]db.Node{}, nil).Once()
	database.On("GetNode", mock.Anything, node.ID).Return(node, nil)
	database.On("UpdateNode", mock.Anything, &node).Return(node, nil)
	wrapper := New(database, slog.Default())
	queue := startNodeSource(t, wrapper, filters)

	updated, err := wrapper.UpdateNode(context.Background(), &node)

	require.NoError(t, err)
	assert.Equal(t, node, updated)
	assert.Equal(t, dbreconcile.NodeKey(node.ID), getQueueItem(t, queue))
	database.AssertExpectations(t)
}

func TestUnit_DBEnqueuesNodeUpdateWhenPreviousStateMatchesSourceFilters(t *testing.T) {
	nodeID := uuid.New()
	previous := db.Node{ID: nodeID}
	updated := db.Node{
		ID: nodeID,
		Conditions: db.NodeConditions{
			Deleting: db.NewCondition(true, "deleting"),
		},
	}
	filters := dbfilters.Node{Deleting: ptr.To(false)}
	database := &testutil.MockDB{}
	database.On("ListNodes", mock.Anything, db.NodeFilters{}).Return([]db.Node{}, nil).Once()
	database.On("GetNode", mock.Anything, nodeID).Return(previous, nil)
	database.On("UpdateNode", mock.Anything, &updated).Return(updated, nil)
	wrapper := New(database, slog.Default())
	queue := startNodeSource(t, wrapper, filters)

	got, err := wrapper.UpdateNode(context.Background(), &updated)

	require.NoError(t, err)
	assert.Equal(t, updated, got)
	assert.Equal(t, dbreconcile.NodeKey(nodeID), getQueueItem(t, queue))
	database.AssertExpectations(t)
}

func TestUnit_DBEnqueuesNodeAfterUpdateNodeForgeDetails(t *testing.T) {
	nodeID := uuid.New()
	previous := db.Node{ID: nodeID}
	updated := db.Node{ID: nodeID, ForgeInstanceID: "instance-1"}
	forgeInstanceID := "instance-1"
	database := &testutil.MockDB{}
	database.On("ListNodes", mock.Anything, db.NodeFilters{}).Return([]db.Node{}, nil)
	database.On("GetNode", mock.Anything, nodeID).Return(previous, nil).Once()
	database.On("UpdateNodeForgeDetails", mock.Anything, nodeID, &forgeInstanceID, (*string)(nil), (*string)(nil)).Return(nil)
	database.On("GetNode", mock.Anything, nodeID).Return(updated, nil).Once()
	wrapper := New(database, slog.Default())
	queue := startNodeSource(t, wrapper, dbfilters.Node{})

	err := wrapper.UpdateNodeForgeDetails(context.Background(), nodeID, &forgeInstanceID, nil, nil)

	require.NoError(t, err)
	assert.Equal(t, dbreconcile.NodeKey(nodeID), getQueueItem(t, queue))
	database.AssertExpectations(t)
}

func TestUnit_DBDoesNotEnqueueNodeAfterUpdateNodeForgeDetailsNoop(t *testing.T) {
	nodeID := uuid.New()
	database := &testutil.MockDB{}
	database.On("ListNodes", mock.Anything, db.NodeFilters{}).Return([]db.Node{}, nil)
	database.On("UpdateNodeForgeDetails", mock.Anything, nodeID, (*string)(nil), (*string)(nil), (*string)(nil)).Return(nil)
	wrapper := New(database, slog.Default())
	queue := startNodeSource(t, wrapper, dbfilters.Node{})

	err := wrapper.UpdateNodeForgeDetails(context.Background(), nodeID, nil, nil, nil)

	require.NoError(t, err)
	assertQueueEmpty(t, queue)
	database.AssertExpectations(t)
}

func TestUnit_DBDoesNotEnqueueNodeAfterUpdateNodeForgeDetailsFilterMismatch(t *testing.T) {
	nodeID := uuid.New()
	previous := db.Node{ID: nodeID, ControlPlane: true}
	updated := db.Node{ID: nodeID, ControlPlane: true, ForgeInstanceID: "instance-1"}
	forgeInstanceID := "instance-1"
	database := &testutil.MockDB{}
	database.On("ListNodes", mock.Anything, db.NodeFilters{}).Return([]db.Node{}, nil)
	database.On("GetNode", mock.Anything, nodeID).Return(previous, nil).Once()
	database.On("UpdateNodeForgeDetails", mock.Anything, nodeID, &forgeInstanceID, (*string)(nil), (*string)(nil)).Return(nil)
	database.On("GetNode", mock.Anything, nodeID).Return(updated, nil).Once()
	wrapper := New(database, slog.Default())
	queue := startNodeSource(t, wrapper, dbfilters.Node{ControlPlane: ptr.To(false)})

	err := wrapper.UpdateNodeForgeDetails(context.Background(), nodeID, &forgeInstanceID, nil, nil)

	require.NoError(t, err)
	assertQueueEmpty(t, queue)
	database.AssertExpectations(t)
}

func TestUnit_DBEnqueuesNodeUpdateWhenPendingSourceFilterMatches(t *testing.T) {
	node := db.Node{
		ID: uuid.New(),
		Conditions: db.NodeConditions{
			Pending: db.NewCondition(true, ""),
		},
	}
	filters := dbfilters.Node{
		Pending:  ptr.To(true),
		Deleting: ptr.To(false),
	}
	database := &testutil.MockDB{}
	database.On("ListNodes", mock.Anything, db.NodeFilters{}).Return([]db.Node{}, nil).Once()
	database.On("GetNode", mock.Anything, node.ID).Return(node, nil)
	database.On("UpdateNode", mock.Anything, &node).Return(node, nil)
	wrapper := New(database, slog.Default())
	queue := startNodeSource(t, wrapper, filters)

	updated, err := wrapper.UpdateNode(context.Background(), &node)

	require.NoError(t, err)
	assert.Equal(t, node, updated)
	assert.Equal(t, dbreconcile.NodeKey(node.ID), getQueueItem(t, queue))
	database.AssertExpectations(t)
}

func TestUnit_DBNodeSourceEnqueuesOwningClusterAfterUpdate(t *testing.T) {
	node := db.Node{
		ID:           uuid.New(),
		ClusterID:    uuid.New(),
		ControlPlane: true,
		Conditions:   db.NodeConditions{Joined: db.NewCondition(true, "")},
	}
	filters := dbfilters.Node{ControlPlane: ptr.To(true), Joined: ptr.To(true)}
	database := &testutil.MockDB{}
	database.On("ListNodes", mock.Anything, db.NodeFilters{}).Return([]db.Node{}, nil).Once()
	database.On("GetNode", mock.Anything, node.ID).Return(node, nil)
	database.On("UpdateNode", mock.Anything, &node).Return(node, nil)
	wrapper := New(database, slog.Default())
	queue := startNodeClusterSource(t, wrapper, filters)

	updated, err := wrapper.UpdateNode(context.Background(), &node)

	require.NoError(t, err)
	assert.Equal(t, node, updated)
	assert.Equal(t, dbreconcile.ClusterKey(node.ClusterID), getQueueItem(t, queue))
	database.AssertExpectations(t)
}

func TestUnit_DBEnqueuesNodeAfterSuccessfulTxUpdateNodeForgeDetails(t *testing.T) {
	nodeID := uuid.New()
	previous := db.Node{ID: nodeID}
	updated := db.Node{ID: nodeID, ForgeInstanceID: "instance-1"}
	forgeInstanceID := "instance-1"
	database := &testutil.MockDB{MockTx: &testutil.MockTx{}}
	database.On("ListNodes", mock.Anything, db.NodeFilters{}).Return([]db.Node{}, nil)
	database.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
	database.MockTx.On("GetNode", mock.Anything, nodeID).Return(previous, nil).Once()
	database.MockTx.On("UpdateNodeForgeDetails", mock.Anything, nodeID, &forgeInstanceID, (*string)(nil), (*string)(nil)).Return(nil)
	database.MockTx.On("GetNode", mock.Anything, nodeID).Return(updated, nil).Once()
	wrapper := New(database, slog.Default())
	queue := startNodeSource(t, wrapper, dbfilters.Node{})

	err := wrapper.RunInTx(context.Background(), func(ctx context.Context, tx db.Tx) error {
		return tx.UpdateNodeForgeDetails(ctx, nodeID, &forgeInstanceID, nil, nil)
	})

	require.NoError(t, err)
	assert.Equal(t, dbreconcile.NodeKey(nodeID), getQueueItem(t, queue))
	database.AssertExpectations(t)
	database.MockTx.AssertExpectations(t)
}

func TestUnit_DBDoesNotEnqueueNodeAfterFailedTxUpdateNodeForgeDetails(t *testing.T) {
	nodeID := uuid.New()
	previous := db.Node{ID: nodeID}
	forgeInstanceID := "instance-1"
	wantErr := errors.New("tx failed")
	database := &testutil.MockDB{MockTx: &testutil.MockTx{}}
	database.On("ListNodes", mock.Anything, db.NodeFilters{}).Return([]db.Node{}, nil)
	database.On("RunInTx", mock.Anything, mock.Anything).Return(wantErr)
	database.MockTx.On("GetNode", mock.Anything, nodeID).Return(previous, nil).Once()
	database.MockTx.On("UpdateNodeForgeDetails", mock.Anything, nodeID, &forgeInstanceID, (*string)(nil), (*string)(nil)).Return(nil)
	database.MockTx.On("GetNode", mock.Anything, nodeID).Return(db.Node{ID: nodeID, ForgeInstanceID: "instance-1"}, nil).Once()
	wrapper := New(database, slog.Default())
	queue := startNodeSource(t, wrapper, dbfilters.Node{})

	err := wrapper.RunInTx(context.Background(), func(ctx context.Context, tx db.Tx) error {
		return tx.UpdateNodeForgeDetails(ctx, nodeID, &forgeInstanceID, nil, nil)
	})

	require.ErrorIs(t, err, wantErr)
	assertQueueEmpty(t, queue)
	database.AssertExpectations(t)
	database.MockTx.AssertExpectations(t)
}

func TestUnit_DBDoesNotEnqueueNodeAfterUpdateError(t *testing.T) {
	node := db.Node{ID: uuid.New()}
	wantErr := errors.New("update failed")
	database := &testutil.MockDB{}
	database.On("ListNodes", mock.Anything, db.NodeFilters{}).Return([]db.Node{}, nil)
	database.On("GetNode", mock.Anything, node.ID).Return(node, nil)
	database.On("UpdateNode", mock.Anything, &node).Return(db.Node{}, wantErr)
	wrapper := New(database, slog.Default())
	queue := startNodeSource(t, wrapper, dbfilters.Node{})

	_, err := wrapper.UpdateNode(context.Background(), &node)

	require.ErrorIs(t, err, wantErr)
	assertQueueEmpty(t, queue)
	database.AssertExpectations(t)
}

func TestUnit_DBDoesNotEnqueueNodeAfterCreateError(t *testing.T) {
	node := db.Node{ID: uuid.New()}
	wantErr := errors.New("create failed")
	database := &testutil.MockDB{}
	database.On("ListNodes", mock.Anything, db.NodeFilters{}).Return([]db.Node{}, nil)
	database.On("CreateNode", mock.Anything, &node).Return(db.Node{}, wantErr)
	wrapper := New(database, slog.Default())
	queue := startNodeSource(t, wrapper, dbfilters.Node{})

	_, err := wrapper.CreateNode(context.Background(), &node)

	require.ErrorIs(t, err, wantErr)
	assertQueueEmpty(t, queue)
	database.AssertExpectations(t)
}

func TestUnit_DBEnqueuesClusterAfterUpdate(t *testing.T) {
	cluster := db.Cluster{ID: uuid.New()}
	database := &testutil.MockDB{}
	database.On("ListClusters", mock.Anything, db.ClusterFilters{}).Return([]db.Cluster{}, nil)
	database.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil)
	database.On("UpdateCluster", mock.Anything, &cluster).Return(cluster, nil)
	wrapper := New(database, slog.Default())
	queue := startClusterSource(t, wrapper, dbfilters.Cluster{})

	updated, err := wrapper.UpdateCluster(context.Background(), &cluster)

	require.NoError(t, err)
	assert.Equal(t, cluster, updated)
	assert.Equal(t, dbreconcile.ClusterKey(cluster.ID), getQueueItem(t, queue))
	database.AssertExpectations(t)
}

func TestUnit_DBClusterEnqueueFansOutToEachSource(t *testing.T) {
	cluster := db.Cluster{ID: uuid.New()}
	database := &testutil.MockDB{}
	database.On("ListClusters", mock.Anything, db.ClusterFilters{}).Return([]db.Cluster{}, nil)
	database.On("CreateCluster", mock.Anything, &cluster).Return(cluster, nil)
	wrapper := New(database, slog.Default())
	firstQueue := startClusterSource(t, wrapper, dbfilters.Cluster{})
	secondQueue := startClusterSource(t, wrapper, dbfilters.Cluster{})

	created, err := wrapper.CreateCluster(context.Background(), &cluster)

	require.NoError(t, err)
	assert.Equal(t, cluster, created)
	assert.Equal(t, dbreconcile.ClusterKey(cluster.ID), getQueueItem(t, firstQueue))
	assert.Equal(t, dbreconcile.ClusterKey(cluster.ID), getQueueItem(t, secondQueue))
	database.AssertExpectations(t)
}

func TestUnit_DBEnqueuesClusterAfterControlPlaneNodePoolUpdateFromOldState(t *testing.T) {
	clusterID := uuid.New()
	nodePoolID := uuid.New()
	previous := db.NodePool{ID: nodePoolID, ClusterID: clusterID, ControlPlane: true}
	updated := db.NodePool{ID: nodePoolID, ClusterID: clusterID, ControlPlane: false}
	filters := dbfilters.NodePool{ControlPlane: ptr.To(true)}
	database := &testutil.MockDB{}
	database.On("ListNodePools", mock.Anything, db.NodePoolFilters{}).Return([]db.NodePool{}, nil)
	database.On("GetNodePool", mock.Anything, nodePoolID).Return(previous, nil)
	database.On("UpdateNodePool", mock.Anything, &updated).Return(updated, nil)
	wrapper := New(database, slog.Default())
	queue := startNodePoolClusterSource(t, wrapper, filters)

	got, err := wrapper.UpdateNodePool(context.Background(), &updated)

	require.NoError(t, err)
	assert.Equal(t, updated, got)
	assert.Equal(t, dbreconcile.ClusterKey(clusterID), getQueueItem(t, queue))
	database.AssertExpectations(t)
}

func TestUnit_DBEnqueuesClusterAfterControlPlaneNodeDeleteFromOwningPool(t *testing.T) {
	clusterID := uuid.New()
	nodePoolID := uuid.New()
	node := db.Node{ID: uuid.New(), ClusterID: clusterID, NodePoolID: nodePoolID, ControlPlane: true}
	filters := dbfilters.Node{ControlPlane: ptr.To(true)}
	database := &testutil.MockDB{}
	database.On("ListNodes", mock.Anything, db.NodeFilters{}).Return([]db.Node{}, nil)
	database.On("GetNodeForUpdate", mock.Anything, node.ID).Return(node, nil)
	database.On("DeleteNode", mock.Anything, node.ID).Return(nil)
	wrapper := New(database, slog.Default())
	queue := startNodeClusterSource(t, wrapper, filters)

	err := wrapper.DeleteNode(context.Background(), node.ID)

	require.NoError(t, err)
	assert.Equal(t, dbreconcile.ClusterKey(clusterID), getQueueItem(t, queue))
	database.AssertExpectations(t)
}

func TestUnit_DBEnqueuesNodeAfterSuccessfulTx(t *testing.T) {
	node := db.Node{ID: uuid.New()}
	database := &testutil.MockDB{MockTx: &testutil.MockTx{}}
	database.On("ListNodes", mock.Anything, db.NodeFilters{}).Return([]db.Node{}, nil)
	database.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
	database.MockTx.On("GetNode", mock.Anything, node.ID).Return(node, nil)
	database.MockTx.On("UpdateNode", mock.Anything, &node).Return(node, nil)
	wrapper := New(database, slog.Default())
	queue := startNodeSource(t, wrapper, dbfilters.Node{})

	err := wrapper.RunInTx(context.Background(), func(ctx context.Context, tx db.Tx) error {
		_, err := tx.UpdateNode(ctx, &node)
		return err
	})

	require.NoError(t, err)
	assert.Equal(t, dbreconcile.NodeKey(node.ID), getQueueItem(t, queue))
	database.AssertExpectations(t)
	database.MockTx.AssertExpectations(t)
}

func TestUnit_DBEnqueuesNodeAfterSuccessfulTxCreate(t *testing.T) {
	node := db.Node{ID: uuid.New()}
	database := &testutil.MockDB{MockTx: &testutil.MockTx{}}
	database.On("ListNodes", mock.Anything, db.NodeFilters{}).Return([]db.Node{}, nil)
	database.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
	database.MockTx.On("CreateNode", mock.Anything, &node).Return(node, nil)
	wrapper := New(database, slog.Default())
	queue := startNodeSource(t, wrapper, dbfilters.Node{})

	err := wrapper.RunInTx(context.Background(), func(ctx context.Context, tx db.Tx) error {
		_, err := tx.CreateNode(ctx, &node)
		return err
	})

	require.NoError(t, err)
	assert.Equal(t, dbreconcile.NodeKey(node.ID), getQueueItem(t, queue))
	database.AssertExpectations(t)
	database.MockTx.AssertExpectations(t)
}

func TestUnit_DBEnqueuesNodeAfterSuccessfulTxTeleportRegisteredUpdate(t *testing.T) {
	node := db.Node{ID: uuid.New()}
	condition := db.NewCondition(true, "Registered with Teleport")
	updatedNode := node
	updatedNode.Conditions.TeleportRegistered = condition
	database := &testutil.MockDB{MockTx: &testutil.MockTx{}}
	database.On("ListNodes", mock.Anything, db.NodeFilters{}).Return([]db.Node{}, nil)
	database.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
	database.MockTx.On("GetNode", mock.Anything, node.ID).Return(node, nil).Once()
	database.MockTx.On("UpdateNodeTeleportRegistered", mock.Anything, node.ID, condition).Return(nil).Once()
	database.MockTx.On("GetNode", mock.Anything, node.ID).Return(updatedNode, nil).Once()
	wrapper := New(database, slog.Default())
	queue := startNodeSource(t, wrapper, dbfilters.Node{})

	err := wrapper.RunInTx(context.Background(), func(ctx context.Context, tx db.Tx) error {
		return tx.UpdateNodeTeleportRegistered(ctx, node.ID, condition)
	})

	require.NoError(t, err)
	assert.Equal(t, dbreconcile.NodeKey(node.ID), getQueueItem(t, queue))
	database.AssertExpectations(t)
	database.MockTx.AssertExpectations(t)
}

func TestUnit_DBEnqueuesClusterAfterSuccessfulTxUpdate(t *testing.T) {
	cluster := db.Cluster{ID: uuid.New()}
	database := &testutil.MockDB{MockTx: &testutil.MockTx{}}
	database.On("ListClusters", mock.Anything, db.ClusterFilters{}).Return([]db.Cluster{}, nil)
	database.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
	database.MockTx.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil)
	database.MockTx.On("UpdateCluster", mock.Anything, &cluster).Return(cluster, nil)
	wrapper := New(database, slog.Default())
	queue := startClusterSource(t, wrapper, dbfilters.Cluster{})

	err := wrapper.RunInTx(context.Background(), func(ctx context.Context, tx db.Tx) error {
		_, err := tx.UpdateCluster(ctx, &cluster)
		return err
	})

	require.NoError(t, err)
	assert.Equal(t, dbreconcile.ClusterKey(cluster.ID), getQueueItem(t, queue))
	database.AssertExpectations(t)
	database.MockTx.AssertExpectations(t)
}

func TestUnit_DBDoesNotFailSuccessfulTxWhenPostCommitNodeEnqueueFails(t *testing.T) {
	node := db.Node{ID: uuid.New()}
	wantErr := errors.New("enqueue failed")
	database := &testutil.MockDB{MockTx: &testutil.MockTx{}}
	database.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
	database.MockTx.On("GetNode", mock.Anything, node.ID).Return(node, nil)
	database.MockTx.On("UpdateNode", mock.Anything, &node).Return(node, nil)
	wrapper := New(database, slog.Default())
	sourceID := uuid.New()
	wrapper.nodeSources[sourceID] = nodeSourceRegistration{
		id: sourceID,
		enqueue: func(context.Context, ...db.Node) error {
			return wantErr
		},
	}

	err := wrapper.RunInTx(context.Background(), func(ctx context.Context, tx db.Tx) error {
		_, err := tx.UpdateNode(ctx, &node)
		return err
	})

	require.NoError(t, err)
	database.AssertExpectations(t)
	database.MockTx.AssertExpectations(t)
}

func TestUnit_DBPostCommitClusterForNodeEnqueueContinuesAfterNodeEnqueueFails(t *testing.T) {
	node := db.Node{ID: uuid.New(), ClusterID: uuid.New()}
	wantErr := errors.New("enqueue failed")
	database := &testutil.MockDB{MockTx: &testutil.MockTx{}}
	database.On("ListNodes", mock.Anything, db.NodeFilters{}).Return([]db.Node{}, nil).Once()
	database.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
	database.MockTx.On("GetNode", mock.Anything, node.ID).Return(node, nil)
	database.MockTx.On("UpdateNode", mock.Anything, &node).Return(node, nil)
	wrapper := New(database, slog.New(slog.DiscardHandler))
	sourceID := uuid.New()
	wrapper.nodeSources[sourceID] = nodeSourceRegistration{
		id: sourceID,
		enqueue: func(context.Context, ...db.Node) error {
			return wantErr
		},
	}
	queue := startNodeClusterSource(t, wrapper, dbfilters.Node{})

	err := wrapper.RunInTx(context.Background(), func(ctx context.Context, tx db.Tx) error {
		_, err := tx.UpdateNode(ctx, &node)
		return err
	})

	require.NoError(t, err)
	assert.Equal(t, dbreconcile.ClusterKey(node.ClusterID), getQueueItem(t, queue))
	database.AssertExpectations(t)
	database.MockTx.AssertExpectations(t)
}

func TestUnit_DBDoesNotEnqueueNodeAfterFailedTx(t *testing.T) {
	node := db.Node{ID: uuid.New()}
	wantErr := errors.New("tx failed")
	database := &testutil.MockDB{MockTx: &testutil.MockTx{}}
	database.On("ListNodes", mock.Anything, db.NodeFilters{}).Return([]db.Node{}, nil)
	database.On("RunInTx", mock.Anything, mock.Anything).Return(wantErr)
	database.MockTx.On("GetNode", mock.Anything, node.ID).Return(node, nil)
	database.MockTx.On("UpdateNode", mock.Anything, &node).Return(node, nil)
	wrapper := New(database, slog.Default())
	queue := startNodeSource(t, wrapper, dbfilters.Node{})

	err := wrapper.RunInTx(context.Background(), func(ctx context.Context, tx db.Tx) error {
		_, err := tx.UpdateNode(ctx, &node)
		return err
	})

	require.ErrorIs(t, err, wantErr)
	assertQueueEmpty(t, queue)
	database.AssertExpectations(t)
	database.MockTx.AssertExpectations(t)
}

func TestUnit_DBEnqueuesNodePool(t *testing.T) {
	t.Run("after_create", func(t *testing.T) {
		nodePool := db.NodePool{ID: uuid.New()}
		database := &testutil.MockDB{}
		database.On("ListNodePools", mock.Anything, db.NodePoolFilters{}).Return([]db.NodePool{}, nil).Once()
		database.On("CreateNodePool", mock.Anything, &nodePool).Return(nodePool, nil)
		wrapper := New(database, slog.Default())
		queue := startNodePoolSource(t, wrapper)

		created, err := wrapper.CreateNodePool(context.Background(), &nodePool)

		require.NoError(t, err)
		assert.Equal(t, nodePool, created)
		assert.Equal(t, dbreconcile.NodePoolKey(nodePool.ID), getQueueItem(t, queue))
		database.AssertExpectations(t)
	})

	t.Run("after_update", func(t *testing.T) {
		nodePool := db.NodePool{ID: uuid.New()}
		database := &testutil.MockDB{}
		database.On("ListNodePools", mock.Anything, db.NodePoolFilters{}).Return([]db.NodePool{}, nil).Once()
		database.On("GetNodePool", mock.Anything, nodePool.ID).Return(nodePool, nil)
		database.On("UpdateNodePool", mock.Anything, &nodePool).Return(nodePool, nil)
		wrapper := New(database, slog.Default())
		queue := startNodePoolSource(t, wrapper)

		updated, err := wrapper.UpdateNodePool(context.Background(), &nodePool)

		require.NoError(t, err)
		assert.Equal(t, nodePool, updated)
		assert.Equal(t, dbreconcile.NodePoolKey(nodePool.ID), getQueueItem(t, queue))
		database.AssertExpectations(t)
	})

	t.Run("after_delete", func(t *testing.T) {
		nodePoolID := uuid.New()
		nodePool := db.NodePool{ID: nodePoolID}
		database := &testutil.MockDB{}
		database.On("ListNodePools", mock.Anything, db.NodePoolFilters{}).Return([]db.NodePool{}, nil)
		database.On("GetNodePool", mock.Anything, nodePoolID).Return(nodePool, nil)
		database.On("DeleteNodePool", mock.Anything, nodePoolID).Return(nil)
		wrapper := New(database, slog.Default())
		queue := startNodePoolSource(t, wrapper)

		err := wrapper.DeleteNodePool(context.Background(), nodePoolID)

		require.NoError(t, err)
		assert.Equal(t, dbreconcile.NodePoolKey(nodePoolID), getQueueItem(t, queue))
		database.AssertExpectations(t)
	})
}

func TestUnit_DBNodePoolEnqueueFansOutToEachSource(t *testing.T) {
	nodePool := db.NodePool{ID: uuid.New()}
	database := &testutil.MockDB{}
	database.On("ListNodePools", mock.Anything, db.NodePoolFilters{}).Return([]db.NodePool{}, nil)
	database.On("CreateNodePool", mock.Anything, &nodePool).Return(nodePool, nil)
	wrapper := New(database, slog.Default())
	firstQueue := startNodePoolSource(t, wrapper)
	secondQueue := startNodePoolSource(t, wrapper)

	created, err := wrapper.CreateNodePool(context.Background(), &nodePool)

	require.NoError(t, err)
	assert.Equal(t, nodePool, created)
	assert.Equal(t, dbreconcile.NodePoolKey(nodePool.ID), getQueueItem(t, firstQueue))
	assert.Equal(t, dbreconcile.NodePoolKey(nodePool.ID), getQueueItem(t, secondQueue))
	database.AssertExpectations(t)
}

func TestUnit_DBEnqueuesNodePoolAfterCommittedTransaction(t *testing.T) {
	t.Run("after_create", func(t *testing.T) {
		nodePool := db.NodePool{ID: uuid.New()}
		database := &testutil.MockDB{MockTx: &testutil.MockTx{}}
		database.On("ListNodePools", mock.Anything, db.NodePoolFilters{}).Return([]db.NodePool{}, nil).Once()
		database.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
		database.MockTx.On("CreateNodePool", mock.Anything, &nodePool).Return(nodePool, nil)
		wrapper := New(database, slog.Default())
		queue := startNodePoolSource(t, wrapper)

		err := wrapper.RunInTx(context.Background(), func(ctx context.Context, tx db.Tx) error {
			_, err := tx.CreateNodePool(ctx, &nodePool)
			return err
		})

		require.NoError(t, err)
		assert.Equal(t, dbreconcile.NodePoolKey(nodePool.ID), getQueueItem(t, queue))
		database.AssertExpectations(t)
		database.MockTx.AssertExpectations(t)
	})

	t.Run("after_update", func(t *testing.T) {
		nodePool := db.NodePool{ID: uuid.New()}
		database := &testutil.MockDB{MockTx: &testutil.MockTx{}}
		database.On("ListNodePools", mock.Anything, db.NodePoolFilters{}).Return([]db.NodePool{}, nil).Once()
		database.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
		database.MockTx.On("GetNodePool", mock.Anything, nodePool.ID).Return(nodePool, nil)
		database.MockTx.On("UpdateNodePool", mock.Anything, &nodePool).Return(nodePool, nil)
		wrapper := New(database, slog.Default())
		queue := startNodePoolSource(t, wrapper)

		err := wrapper.RunInTx(context.Background(), func(ctx context.Context, tx db.Tx) error {
			_, err := tx.UpdateNodePool(ctx, &nodePool)
			return err
		})

		require.NoError(t, err)
		assert.Equal(t, dbreconcile.NodePoolKey(nodePool.ID), getQueueItem(t, queue))
		database.AssertExpectations(t)
		database.MockTx.AssertExpectations(t)
	})

	t.Run("after_delete", func(t *testing.T) {
		nodePoolID := uuid.New()
		nodePool := db.NodePool{ID: nodePoolID}
		database := &testutil.MockDB{MockTx: &testutil.MockTx{}}
		database.On("ListNodePools", mock.Anything, db.NodePoolFilters{}).Return([]db.NodePool{}, nil)
		database.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
		database.MockTx.On("GetNodePool", mock.Anything, nodePoolID).Return(nodePool, nil)
		database.MockTx.On("DeleteNodePool", mock.Anything, nodePoolID).Return(nil)
		wrapper := New(database, slog.Default())
		queue := startNodePoolSource(t, wrapper)

		err := wrapper.RunInTx(context.Background(), func(ctx context.Context, tx db.Tx) error {
			return tx.DeleteNodePool(ctx, nodePoolID)
		})

		require.NoError(t, err)
		assert.Equal(t, dbreconcile.NodePoolKey(nodePoolID), getQueueItem(t, queue))
		database.AssertExpectations(t)
		database.MockTx.AssertExpectations(t)
	})
}

func TestUnit_DBDoesNotEnqueueNodePool(t *testing.T) {
	t.Run("after_failed_transaction", func(t *testing.T) {
		nodePool := db.NodePool{ID: uuid.New()}
		wantErr := errors.New("tx failed")
		database := &testutil.MockDB{MockTx: &testutil.MockTx{}}
		database.On("ListNodePools", mock.Anything, db.NodePoolFilters{}).Return([]db.NodePool{}, nil)
		database.On("RunInTx", mock.Anything, mock.Anything).Return(wantErr)
		database.MockTx.On("GetNodePool", mock.Anything, nodePool.ID).Return(nodePool, nil)
		database.MockTx.On("UpdateNodePool", mock.Anything, &nodePool).Return(nodePool, nil)
		wrapper := New(database, slog.Default())
		queue := startNodePoolSource(t, wrapper)

		err := wrapper.RunInTx(context.Background(), func(ctx context.Context, tx db.Tx) error {
			_, err := tx.UpdateNodePool(ctx, &nodePool)
			return err
		})

		require.ErrorIs(t, err, wantErr)
		assertQueueEmpty(t, queue)
		database.AssertExpectations(t)
		database.MockTx.AssertExpectations(t)
	})

	t.Run("after_deleting_node_without_node_pool_id", func(t *testing.T) {
		node := db.Node{ID: uuid.New()}
		database := &testutil.MockDB{}
		database.On("ListNodes", mock.Anything, db.NodeFilters{}).Return([]db.Node{}, nil).Once()
		database.On("GetNodeForUpdate", mock.Anything, node.ID).Return(node, nil)
		database.On("DeleteNode", mock.Anything, node.ID).Return(nil)
		wrapper := New(database, slog.Default())
		queue := startNodePoolSourceForNode(t, wrapper)

		err := wrapper.DeleteNode(context.Background(), node.ID)

		require.NoError(t, err)
		assertQueueEmpty(t, queue)
		database.AssertExpectations(t)
	})
}

func TestUnit_DBEnqueuesParentNodePoolAfterNodeDelete(t *testing.T) {
	t.Run("top_level", func(t *testing.T) {
		nodePool := db.NodePool{ID: uuid.New()}
		node := db.Node{ID: uuid.New(), NodePoolID: nodePool.ID}
		database := &testutil.MockDB{}
		database.On("ListNodes", mock.Anything, db.NodeFilters{}).Return([]db.Node{}, nil).Once()
		database.On("GetNodeForUpdate", mock.Anything, node.ID).Return(node, nil)
		database.On("DeleteNode", mock.Anything, node.ID).Return(nil)
		wrapper := New(database, slog.Default())
		queue := startNodePoolSourceForNode(t, wrapper)

		err := wrapper.DeleteNode(context.Background(), node.ID)

		require.NoError(t, err)
		assert.Equal(t, dbreconcile.NodePoolKey(nodePool.ID), getQueueItem(t, queue))
		database.AssertExpectations(t)
	})

	t.Run("transactional", func(t *testing.T) {
		nodePool := db.NodePool{ID: uuid.New()}
		node := db.Node{ID: uuid.New(), NodePoolID: nodePool.ID}
		database := &testutil.MockDB{MockTx: &testutil.MockTx{}}
		database.On("ListNodes", mock.Anything, db.NodeFilters{}).Return([]db.Node{}, nil).Once()
		database.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
		database.MockTx.On("GetNodeForUpdate", mock.Anything, node.ID).Return(node, nil)
		database.MockTx.On("DeleteNode", mock.Anything, node.ID).Return(nil)
		wrapper := New(database, slog.Default())
		queue := startNodePoolSourceForNode(t, wrapper)

		err := wrapper.RunInTx(context.Background(), func(ctx context.Context, tx db.Tx) error {
			return tx.DeleteNode(ctx, node.ID)
		})

		require.NoError(t, err)
		assert.Equal(t, dbreconcile.NodePoolKey(nodePool.ID), getQueueItem(t, queue))
		database.AssertExpectations(t)
		database.MockTx.AssertExpectations(t)
	})
}

func TestUnit_DBNodeSourceSeedsNodes(t *testing.T) {
	first := db.Node{ID: uuid.New(), Conditions: db.NodeConditions{Provisioning: db.NewCondition(true, "")}}
	second := db.Node{ID: uuid.New(), Conditions: db.NodeConditions{Provisioning: db.NewCondition(true, "")}}
	deleting := db.Node{ID: uuid.New(), Conditions: db.NodeConditions{Provisioning: db.NewCondition(true, ""), Deleting: db.NewCondition(true, "")}}
	unmatched := db.Node{ID: uuid.New()}
	filters := dbfilters.Node{
		Provisioning: ptr.To(true),
		Deleting:     ptr.To(false),
	}
	database := &testutil.MockDB{}
	database.On("ListNodes", mock.Anything, db.NodeFilters{}).Return([]db.Node{first, unmatched, deleting, second}, nil)
	wrapper := New(database, slog.Default())
	queue := startNodeSource(t, wrapper, filters)

	assert.ElementsMatch(t, []dbreconcile.Key{
		dbreconcile.NodeKey(first.ID),
		dbreconcile.NodeKey(second.ID),
	}, []dbreconcile.Key{
		getQueueItem(t, queue),
		getQueueItem(t, queue),
	})
	database.AssertExpectations(t)
}

func TestUnit_DBNodePoolSourceSeedsNodePools(t *testing.T) {
	first := db.NodePool{ID: uuid.New()}
	second := db.NodePool{ID: uuid.New()}
	database := &testutil.MockDB{}
	database.On("ListNodePools", mock.Anything, db.NodePoolFilters{}).Return([]db.NodePool{first, second}, nil)
	wrapper := New(database, slog.Default())
	queue := startNodePoolSource(t, wrapper)

	assert.ElementsMatch(t, []dbreconcile.Key{
		dbreconcile.NodePoolKey(first.ID),
		dbreconcile.NodePoolKey(second.ID),
	}, []dbreconcile.Key{
		getQueueItem(t, queue),
		getQueueItem(t, queue),
	})
	database.AssertExpectations(t)
}

func TestUnit_DBClusterSourceResyncsClusters(t *testing.T) {
	cluster := db.Cluster{ID: uuid.New()}
	database := &testutil.MockDB{}
	database.On("ListClusters", mock.Anything, db.ClusterFilters{}).Return([]db.Cluster{cluster}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	wrapper := New(database, slog.New(slog.DiscardHandler))
	source := NewClusterSource(wrapper, dbfilters.Cluster{})
	source.resyncInterval = 10 * time.Millisecond
	source.coalesce.window = 0 // this test measures resync, not coalescing
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[dbreconcile.Key]())
	t.Cleanup(queue.ShutDown)

	require.NoError(t, source.Start(ctx, queue))

	for range 2 {
		assert.Equal(t, dbreconcile.ClusterKey(cluster.ID), getQueueItem(t, queue))
	}
	cancel()
	database.AssertExpectations(t)
}

func TestUnit_DBNodePoolSourceResyncsNodePools(t *testing.T) {
	nodePool := db.NodePool{ID: uuid.New()}
	database := &testutil.MockDB{}
	database.On("ListNodePools", mock.Anything, db.NodePoolFilters{}).Return([]db.NodePool{nodePool}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	wrapper := New(database, slog.New(slog.DiscardHandler))
	source := NewNodePoolSource(wrapper, dbfilters.NodePool{})
	source.resyncInterval = 10 * time.Millisecond
	source.coalesce.window = 0 // this test measures resync, not coalescing
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[dbreconcile.Key]())
	t.Cleanup(queue.ShutDown)

	require.NoError(t, source.Start(ctx, queue))

	for range 2 {
		assert.Equal(t, dbreconcile.NodePoolKey(nodePool.ID), getQueueItem(t, queue))
	}
	cancel()
	database.AssertExpectations(t)
}

func TestUnit_DBNodeSetSourceResyncsNodeSets(t *testing.T) {
	nodeSet := db.NodeSet{ID: uuid.New()}
	database := &testutil.MockDB{}
	database.On("ListNodeSets", mock.Anything, db.NodeSetFilters{}).Return([]db.NodeSet{nodeSet}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	wrapper := New(database, slog.New(slog.DiscardHandler))
	source := NewNodeSetSource(wrapper, dbfilters.NodeSet{})
	source.resyncInterval = 10 * time.Millisecond
	source.coalesce.window = 0 // this test measures resync, not coalescing
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[dbreconcile.Key]())
	t.Cleanup(queue.ShutDown)

	require.NoError(t, source.Start(ctx, queue))

	for range 2 {
		assert.Equal(t, dbreconcile.NodeSetKey(nodeSet.ID), getQueueItem(t, queue))
	}
	cancel()
	database.AssertExpectations(t)
}

func TestUnit_DBNodeSourceResyncsNodes(t *testing.T) {
	node := db.Node{ID: uuid.New()}
	database := &testutil.MockDB{}
	database.On("ListNodes", mock.Anything, db.NodeFilters{}).Return([]db.Node{node}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	wrapper := New(database, slog.New(slog.DiscardHandler))
	source := NewNodeSource(wrapper, dbfilters.Node{})
	source.resyncInterval = 10 * time.Millisecond
	source.coalesce.window = 0 // this test measures resync, not coalescing
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[dbreconcile.Key]())
	t.Cleanup(queue.ShutDown)

	require.NoError(t, source.Start(ctx, queue))

	for range 2 {
		assert.Equal(t, dbreconcile.NodeKey(node.ID), getQueueItem(t, queue))
	}
	cancel()
	database.AssertExpectations(t)
}

func TestUnit_DBClusterSourceSeedsClusters(t *testing.T) {
	first := db.Cluster{ID: uuid.New()}
	second := db.Cluster{ID: uuid.New()}
	database := &testutil.MockDB{}
	database.On("ListClusters", mock.Anything, db.ClusterFilters{}).Return([]db.Cluster{first, second}, nil)
	wrapper := New(database, slog.Default())
	queue := startClusterSource(t, wrapper, dbfilters.Cluster{})

	assert.ElementsMatch(t, []dbreconcile.Key{
		dbreconcile.ClusterKey(first.ID),
		dbreconcile.ClusterKey(second.ID),
	}, []dbreconcile.Key{
		getQueueItem(t, queue),
		getQueueItem(t, queue),
	})
	database.AssertExpectations(t)
}

func TestUnit_DBNodeSourceReturnsErrorWhenEnqueueContextCancels(t *testing.T) {
	node := db.Node{ID: uuid.New()}
	database := &testutil.MockDB{}
	database.On("ListNodes", mock.Anything, db.NodeFilters{}).Return([]db.Node{node}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	wrapper := New(database, slog.New(slog.DiscardHandler))
	source := NewNodeSource(wrapper, dbfilters.Node{})
	cancel()
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[dbreconcile.Key]())
	t.Cleanup(queue.ShutDown)

	err := source.Start(ctx, queue)

	require.Error(t, err)
	require.ErrorContains(t, err, "failed to enqueue node")
	database.AssertExpectations(t)
}

func TestUnit_DBNodeSetSource(t *testing.T) {
	first := db.NodeSet{ID: uuid.New()}
	second := db.NodeSet{ID: uuid.New()}
	database := &testutil.MockDB{}
	database.On("ListNodeSets", mock.Anything, db.NodeSetFilters{}).Return([]db.NodeSet{first, second}, nil)
	wrapper := New(database, slog.Default())
	queue := startNodeSetSource(t, wrapper)

	assert.ElementsMatch(t, []dbreconcile.Key{
		dbreconcile.NodeSetKey(first.ID),
		dbreconcile.NodeSetKey(second.ID),
	}, []dbreconcile.Key{
		getQueueItem(t, queue),
		getQueueItem(t, queue),
	})
	database.AssertExpectations(t)
}

func TestUnit_DBNodeSetSourceReturnsErrorWhenEnqueueContextCancels(t *testing.T) {
	nodeSet := db.NodeSet{ID: uuid.New()}
	database := &testutil.MockDB{}
	database.On("ListNodeSets", mock.Anything, db.NodeSetFilters{}).Return([]db.NodeSet{nodeSet}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	wrapper := New(database, slog.New(slog.DiscardHandler))
	source := NewNodeSetSource(wrapper, dbfilters.NodeSet{})
	cancel()
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[dbreconcile.Key]())
	t.Cleanup(queue.ShutDown)

	err := source.Start(ctx, queue)

	require.Error(t, err)
	require.ErrorContains(t, err, "failed to enqueue node set")
	database.AssertExpectations(t)
}

func TestUnit_DBNodeSetEnqueue(t *testing.T) {
	type parameters struct {
		setup       func(*testutil.MockDB, db.NodeSet, db.Node)
		run         func(*testing.T, context.Context, *DB, db.NodeSet, db.Node) error
		wantEnqueue bool
	}

	tests := map[string]parameters{
		"node set create enqueues node set": {
			setup: func(database *testutil.MockDB, nodeSet db.NodeSet, _ db.Node) {
				database.On("CreateNodeSet", mock.Anything, &nodeSet).Return(nodeSet, nil)
			},
			run: func(t *testing.T, ctx context.Context, wrapper *DB, nodeSet db.NodeSet, _ db.Node) error {
				t.Helper()

				created, err := wrapper.CreateNodeSet(ctx, &nodeSet)
				assert.Equal(t, nodeSet, created)
				return err
			},
			wantEnqueue: true,
		},
		"node set update enqueues node set": {
			setup: func(database *testutil.MockDB, nodeSet db.NodeSet, _ db.Node) {
				database.On("GetNodeSet", mock.Anything, nodeSet.ID).Return(nodeSet, nil)
				database.On("UpdateNodeSet", mock.Anything, &nodeSet).Return(nodeSet, nil)
			},
			run: func(t *testing.T, ctx context.Context, wrapper *DB, nodeSet db.NodeSet, _ db.Node) error {
				t.Helper()

				updated, err := wrapper.UpdateNodeSet(ctx, &nodeSet)
				assert.Equal(t, nodeSet, updated)
				return err
			},
			wantEnqueue: true,
		},
		"node delete enqueues node set": {
			setup: func(database *testutil.MockDB, nodeSet db.NodeSet, node db.Node) {
				_ = nodeSet
				database.On("GetNodeForUpdate", mock.Anything, node.ID).Return(node, nil)
				database.On("DeleteNode", mock.Anything, node.ID).Return(nil)
			},
			run: func(_ *testing.T, ctx context.Context, wrapper *DB, _ db.NodeSet, node db.Node) error {
				return wrapper.DeleteNode(ctx, node.ID)
			},
			wantEnqueue: true,
		},
		"node update enqueues node set": {
			setup: func(database *testutil.MockDB, _ db.NodeSet, node db.Node) {
				database.On("GetNode", mock.Anything, node.ID).Return(node, nil)
				database.On("UpdateNode", mock.Anything, &node).Return(node, nil)
			},
			run: func(t *testing.T, ctx context.Context, wrapper *DB, _ db.NodeSet, node db.Node) error {
				t.Helper()

				updated, err := wrapper.UpdateNode(ctx, &node)
				assert.Equal(t, node, updated)
				return err
			},
			wantEnqueue: true,
		},
		"node set delete enqueues node set": {
			setup: func(database *testutil.MockDB, nodeSet db.NodeSet, _ db.Node) {
				database.On("GetNodeSet", mock.Anything, nodeSet.ID).Return(nodeSet, nil)
				database.On("DeleteNodeSet", mock.Anything, nodeSet.ID).Return(nil)
			},
			run: func(_ *testing.T, ctx context.Context, wrapper *DB, nodeSet db.NodeSet, _ db.Node) error {
				return wrapper.DeleteNodeSet(ctx, nodeSet.ID)
			},
			wantEnqueue: true,
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			nodeSet := db.NodeSet{ID: uuid.New()}
			node := db.Node{ID: uuid.New(), NodeSetID: nodeSet.ID}
			database := &testutil.MockDB{}
			database.On("ListNodeSets", mock.Anything, db.NodeSetFilters{}).Return([]db.NodeSet{}, nil)
			database.On("ListNodes", mock.Anything, db.NodeFilters{}).Return([]db.Node{}, nil)
			params.setup(database, nodeSet, node)
			wrapper := New(database, slog.Default())
			queue := startNodeSetControllerSources(t, wrapper)

			err := params.run(t, context.Background(), wrapper, nodeSet, node)

			require.NoError(t, err)
			if params.wantEnqueue {
				assert.Equal(t, dbreconcile.NodeSetKey(nodeSet.ID), getQueueItem(t, queue))
			} else {
				assertQueueEmpty(t, queue)
			}
			database.AssertExpectations(t)
		})
	}
}

func TestUnit_DBNodeSetEnqueueFansOutToEachSource(t *testing.T) {
	nodeSet := db.NodeSet{ID: uuid.New()}
	database := &testutil.MockDB{}
	database.On("ListNodeSets", mock.Anything, db.NodeSetFilters{}).Return([]db.NodeSet{}, nil)
	database.On("CreateNodeSet", mock.Anything, &nodeSet).Return(nodeSet, nil)
	wrapper := New(database, slog.Default())
	firstQueue := startNodeSetSource(t, wrapper)
	secondQueue := startNodeSetSource(t, wrapper)

	created, err := wrapper.CreateNodeSet(context.Background(), &nodeSet)

	require.NoError(t, err)
	assert.Equal(t, nodeSet, created)
	assert.Equal(t, dbreconcile.NodeSetKey(nodeSet.ID), getQueueItem(t, firstQueue))
	assert.Equal(t, dbreconcile.NodeSetKey(nodeSet.ID), getQueueItem(t, secondQueue))
	database.AssertExpectations(t)
}

func TestUnit_DBNodeSetEnqueueAfterTransaction(t *testing.T) {
	type parameters struct {
		run         func(context.Context, db.Tx, db.NodeSet) error
		txErr       error
		wantEnqueue bool
	}

	tests := map[string]parameters{
		"successful node set update enqueues node set": {
			run: func(ctx context.Context, tx db.Tx, nodeSet db.NodeSet) error {
				_, err := tx.UpdateNodeSet(ctx, &nodeSet)
				return err
			},
			wantEnqueue: true,
		},
		"failed transaction does not enqueue node set": {
			run: func(ctx context.Context, tx db.Tx, nodeSet db.NodeSet) error {
				_, err := tx.UpdateNodeSet(ctx, &nodeSet)
				return err
			},
			txErr: errors.New("tx failed"),
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			nodeSet := db.NodeSet{ID: uuid.New()}
			database := &testutil.MockDB{MockTx: &testutil.MockTx{}}
			database.On("ListNodeSets", mock.Anything, db.NodeSetFilters{}).Return([]db.NodeSet{}, nil)
			database.On("RunInTx", mock.Anything, mock.Anything).Return(params.txErr)
			database.MockTx.On("GetNodeSet", mock.Anything, nodeSet.ID).Return(nodeSet, nil)
			database.MockTx.On("UpdateNodeSet", mock.Anything, &nodeSet).Return(nodeSet, nil)
			wrapper := New(database, slog.Default())
			queue := startNodeSetSource(t, wrapper)

			err := wrapper.RunInTx(context.Background(), func(ctx context.Context, tx db.Tx) error {
				return params.run(ctx, tx, nodeSet)
			})

			if params.txErr != nil {
				require.ErrorIs(t, err, params.txErr)
				assertQueueEmpty(t, queue)
			} else {
				require.NoError(t, err)
				assert.Equal(t, dbreconcile.NodeSetKey(nodeSet.ID), getQueueItem(t, queue))
			}
			database.AssertExpectations(t)
			database.MockTx.AssertExpectations(t)
		})
	}
}

func startClusterSource(t *testing.T, wrapper *DB, filters dbfilters.Cluster) workqueue.TypedRateLimitingInterface[dbreconcile.Key] {
	t.Helper()

	source := NewClusterSource(wrapper, filters)
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[dbreconcile.Key]())
	t.Cleanup(queue.ShutDown)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, source.Start(ctx, queue))
	return queue
}

func startNodePoolClusterSource(t *testing.T, wrapper *DB, filters dbfilters.NodePool) workqueue.TypedRateLimitingInterface[dbreconcile.Key] {
	t.Helper()

	source := NewNodePoolSource(wrapper, filters, testNodePoolClusterKeys)
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[dbreconcile.Key]())
	t.Cleanup(queue.ShutDown)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, source.Start(ctx, queue))
	return queue
}

func startNodeClusterSource(t *testing.T, wrapper *DB, filters dbfilters.Node) workqueue.TypedRateLimitingInterface[dbreconcile.Key] {
	t.Helper()

	source := NewNodeSource(wrapper, filters, testNodeClusterKeys)
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[dbreconcile.Key]())
	t.Cleanup(queue.ShutDown)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, source.Start(ctx, queue))
	return queue
}

func testNodePoolClusterKeys(_ context.Context, nodePool db.NodePool) ([]dbreconcile.Key, error) {
	if nodePool.ClusterID == uuid.Nil {
		return nil, nil
	}
	return []dbreconcile.Key{dbreconcile.ClusterKey(nodePool.ClusterID)}, nil
}

func testNodeClusterKeys(_ context.Context, node db.Node) ([]dbreconcile.Key, error) {
	if node.ClusterID == uuid.Nil {
		return nil, nil
	}
	return []dbreconcile.Key{dbreconcile.ClusterKey(node.ClusterID)}, nil
}

func startNodeSource(t *testing.T, wrapper *DB, filters dbfilters.Node) workqueue.TypedRateLimitingInterface[dbreconcile.Key] {
	t.Helper()

	return startSource(t, NewNodeSource(wrapper, filters))
}

func startSource(t *testing.T, source crsource.TypedSource[dbreconcile.Key]) workqueue.TypedRateLimitingInterface[dbreconcile.Key] {
	t.Helper()

	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[dbreconcile.Key]())
	t.Cleanup(queue.ShutDown)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, source.Start(ctx, queue))
	return queue
}

func startStringSource(
	ctx context.Context,
	t *testing.T,
	source crsource.TypedSource[string],
) workqueue.TypedRateLimitingInterface[string] {
	t.Helper()

	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]())
	t.Cleanup(queue.ShutDown)
	require.NoError(t, source.Start(ctx, queue))
	return queue
}

func startNodePoolSource(t *testing.T, wrapper *DB) workqueue.TypedRateLimitingInterface[dbreconcile.Key] {
	t.Helper()

	source := NewNodePoolSource(wrapper, dbfilters.NodePool{})
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[dbreconcile.Key]())
	t.Cleanup(queue.ShutDown)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, source.Start(ctx, queue))
	return queue
}

func startNodeSetSource(t *testing.T, wrapper *DB) workqueue.TypedRateLimitingInterface[dbreconcile.Key] {
	t.Helper()

	source := NewNodeSetSource(wrapper, dbfilters.NodeSet{})
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[dbreconcile.Key]())
	t.Cleanup(queue.ShutDown)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, source.Start(ctx, queue))
	return queue
}

func startNodeSetControllerSources(t *testing.T, wrapper *DB) workqueue.TypedRateLimitingInterface[dbreconcile.Key] {
	t.Helper()

	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[dbreconcile.Key]())
	t.Cleanup(queue.ShutDown)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, NewNodeSetSource(wrapper, dbfilters.NodeSet{}).Start(ctx, queue))
	require.NoError(t, NewNodeSource(wrapper, dbfilters.Node{}, testNodeSetKeysForNode).Start(ctx, queue))
	return queue
}

func startNodePoolSourceForNode(t *testing.T, wrapper *DB) workqueue.TypedRateLimitingInterface[dbreconcile.Key] {
	t.Helper()

	source := NewNodeSource(wrapper, dbfilters.Node{}, testNodePoolKeysForNode)
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[dbreconcile.Key]())
	t.Cleanup(queue.ShutDown)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, source.Start(ctx, queue))
	return queue
}

func testNodePoolKeysForNode(_ context.Context, node db.Node) ([]dbreconcile.Key, error) {
	if node.NodePoolID == uuid.Nil {
		return nil, nil
	}
	return []dbreconcile.Key{dbreconcile.NodePoolKey(node.NodePoolID)}, nil
}

func testNodeSetKeysForNode(_ context.Context, node db.Node) ([]dbreconcile.Key, error) {
	if node.NodeSetID == uuid.Nil {
		return nil, nil
	}
	return []dbreconcile.Key{dbreconcile.NodeSetKey(node.NodeSetID)}, nil
}

func getQueueItem(t *testing.T, queue workqueue.TypedRateLimitingInterface[dbreconcile.Key]) dbreconcile.Key {
	t.Helper()

	items := make(chan dbreconcile.Key, 1)
	go func() {
		item, shutdown := queue.Get()
		if shutdown {
			return
		}
		queue.Done(item)
		items <- item
	}()

	select {
	case item := <-items:
		return item
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for queue item")
		return dbreconcile.Key{}
	}
}

func getStringQueueItem(t *testing.T, queue workqueue.TypedRateLimitingInterface[string]) string {
	t.Helper()

	items := make(chan string, 1)
	go func() {
		item, shutdown := queue.Get()
		if shutdown {
			return
		}
		queue.Done(item)
		items <- item
	}()

	select {
	case item := <-items:
		return item
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for queue item")
		return ""
	}
}

func assertQueueEmpty(t *testing.T, queue workqueue.TypedRateLimitingInterface[dbreconcile.Key]) {
	t.Helper()

	items := make(chan dbreconcile.Key, 1)
	go func() {
		item, shutdown := queue.Get()
		if shutdown {
			return
		}
		queue.Done(item)
		items <- item
	}()

	select {
	case item := <-items:
		t.Fatalf("expected queue to be empty, got %s", item)
	case <-time.After(10 * time.Millisecond):
	}
}

func assertStringQueueEmpty(t *testing.T, queue workqueue.TypedRateLimitingInterface[string]) {
	t.Helper()

	items := make(chan string, 1)
	go func() {
		item, shutdown := queue.Get()
		if shutdown {
			return
		}
		queue.Done(item)
		items <- item
	}()

	select {
	case item := <-items:
		t.Fatalf("expected queue to be empty, got %s", item)
	case <-time.After(10 * time.Millisecond):
	}
}
