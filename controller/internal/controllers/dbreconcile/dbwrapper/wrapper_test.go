// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

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

	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func TestUnit_DBEnqueuesNodeAfterUpdate(t *testing.T) {
	node := db.Node{ID: uuid.New()}
	liveFilters := db.NodeFilters{ID: &node.ID}
	database := &testutil.MockDB{}
	database.On("ListNodes", mock.Anything, db.NodeFilters{}).Return([]db.Node{}, nil)
	database.On("ListNodes", mock.Anything, liveFilters).Return([]db.Node{node}, nil)
	database.On("UpdateNode", mock.Anything, &node).Return(node, nil)
	wrapper := New(database, slog.Default())
	queue := startNodeSource(t, wrapper, db.NodeFilters{})

	updated, err := wrapper.UpdateNode(context.Background(), &node)

	require.NoError(t, err)
	assert.Equal(t, node, updated)
	assert.Equal(t, dbreconcile.NodeKey(node.ID), getQueueItem(t, queue))
	database.AssertExpectations(t)
}

func TestUnit_DBEnqueuesNodeAfterCreate(t *testing.T) {
	node := db.Node{ID: uuid.New()}
	liveFilters := db.NodeFilters{ID: &node.ID}
	database := &testutil.MockDB{}
	database.On("ListNodes", mock.Anything, db.NodeFilters{}).Return([]db.Node{}, nil)
	database.On("ListNodes", mock.Anything, liveFilters).Return([]db.Node{node}, nil)
	database.On("CreateNode", mock.Anything, &node).Return(node, nil)
	wrapper := New(database, slog.Default())
	queue := startNodeSource(t, wrapper, db.NodeFilters{})

	created, err := wrapper.CreateNode(context.Background(), &node)

	require.NoError(t, err)
	assert.Equal(t, node, created)
	assert.Equal(t, dbreconcile.NodeKey(node.ID), getQueueItem(t, queue))
	database.AssertExpectations(t)
}

func TestUnit_DBDoesNotEnqueueNodeUpdateWhenSourceFiltersDoNotMatch(t *testing.T) {
	node := db.Node{ID: uuid.New()}
	filters := db.NodeFilters{
		Provisioning: ptr.To(true),
		Deleting:     ptr.To(false),
	}
	liveFilters := filters
	liveFilters.ID = &node.ID
	database := &testutil.MockDB{}
	database.On("ListNodes", mock.Anything, filters).Return([]db.Node{}, nil)
	database.On("ListNodes", mock.Anything, liveFilters).Return([]db.Node{}, nil)
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
	node := db.Node{ID: uuid.New()}
	filters := db.NodeFilters{
		Provisioning: ptr.To(true),
		Deleting:     ptr.To(false),
	}
	liveFilters := filters
	liveFilters.ID = &node.ID
	database := &testutil.MockDB{}
	database.On("ListNodes", mock.Anything, filters).Return([]db.Node{}, nil).Once()
	database.On("ListNodes", mock.Anything, liveFilters).Return([]db.Node{node}, nil).Once()
	database.On("UpdateNode", mock.Anything, &node).Return(node, nil)
	wrapper := New(database, slog.Default())
	queue := startNodeSource(t, wrapper, filters)

	updated, err := wrapper.UpdateNode(context.Background(), &node)

	require.NoError(t, err)
	assert.Equal(t, node, updated)
	assert.Equal(t, dbreconcile.NodeKey(node.ID), getQueueItem(t, queue))
	database.AssertExpectations(t)
}

func TestUnit_DBDoesNotEnqueueNodeAfterUpdateError(t *testing.T) {
	node := db.Node{ID: uuid.New()}
	wantErr := errors.New("update failed")
	database := &testutil.MockDB{}
	database.On("ListNodes", mock.Anything, db.NodeFilters{}).Return([]db.Node{}, nil)
	database.On("UpdateNode", mock.Anything, &node).Return(db.Node{}, wantErr)
	wrapper := New(database, slog.Default())
	queue := startNodeSource(t, wrapper, db.NodeFilters{})

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
	queue := startNodeSource(t, wrapper, db.NodeFilters{})

	_, err := wrapper.CreateNode(context.Background(), &node)

	require.ErrorIs(t, err, wantErr)
	assertQueueEmpty(t, queue)
	database.AssertExpectations(t)
}

func TestUnit_DBEnqueuesNodeAfterSuccessfulTx(t *testing.T) {
	node := db.Node{ID: uuid.New()}
	liveFilters := db.NodeFilters{ID: &node.ID}
	database := &testutil.MockDB{MockTx: &testutil.MockTx{}}
	database.On("ListNodes", mock.Anything, db.NodeFilters{}).Return([]db.Node{}, nil)
	database.On("ListNodes", mock.Anything, liveFilters).Return([]db.Node{node}, nil)
	database.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
	database.MockTx.On("UpdateNode", mock.Anything, &node).Return(node, nil)
	wrapper := New(database, slog.Default())
	queue := startNodeSource(t, wrapper, db.NodeFilters{})

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
	liveFilters := db.NodeFilters{ID: &node.ID}
	database := &testutil.MockDB{MockTx: &testutil.MockTx{}}
	database.On("ListNodes", mock.Anything, db.NodeFilters{}).Return([]db.Node{}, nil)
	database.On("ListNodes", mock.Anything, liveFilters).Return([]db.Node{node}, nil)
	database.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
	database.MockTx.On("CreateNode", mock.Anything, &node).Return(node, nil)
	wrapper := New(database, slog.Default())
	queue := startNodeSource(t, wrapper, db.NodeFilters{})

	err := wrapper.RunInTx(context.Background(), func(ctx context.Context, tx db.Tx) error {
		_, err := tx.CreateNode(ctx, &node)
		return err
	})

	require.NoError(t, err)
	assert.Equal(t, dbreconcile.NodeKey(node.ID), getQueueItem(t, queue))
	database.AssertExpectations(t)
	database.MockTx.AssertExpectations(t)
}

func TestUnit_DBDoesNotFailSuccessfulTxWhenPostCommitNodeEnqueueFails(t *testing.T) {
	node := db.Node{ID: uuid.New()}
	liveFilters := db.NodeFilters{ID: &node.ID}
	wantErr := errors.New("enqueue failed")
	database := &testutil.MockDB{MockTx: &testutil.MockTx{}}
	database.On("ListNodes", mock.Anything, liveFilters).Return([]db.Node{node}, nil)
	database.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
	database.MockTx.On("UpdateNode", mock.Anything, &node).Return(node, nil)
	wrapper := New(database, slog.Default())
	wrapper.nodeSources = append(wrapper.nodeSources, nodeSourceRegistration{
		enqueuer: errorEnqueuer{err: wantErr},
	})

	err := wrapper.RunInTx(context.Background(), func(ctx context.Context, tx db.Tx) error {
		_, err := tx.UpdateNode(ctx, &node)
		return err
	})

	require.NoError(t, err)
	database.AssertExpectations(t)
	database.MockTx.AssertExpectations(t)
}

func TestUnit_DBDoesNotEnqueueNodeAfterFailedTx(t *testing.T) {
	node := db.Node{ID: uuid.New()}
	wantErr := errors.New("tx failed")
	database := &testutil.MockDB{MockTx: &testutil.MockTx{}}
	database.On("ListNodes", mock.Anything, db.NodeFilters{}).Return([]db.Node{}, nil)
	database.On("RunInTx", mock.Anything, mock.Anything).Return(wantErr)
	database.MockTx.On("UpdateNode", mock.Anything, &node).Return(node, nil)
	wrapper := New(database, slog.Default())
	queue := startNodeSource(t, wrapper, db.NodeFilters{})

	err := wrapper.RunInTx(context.Background(), func(ctx context.Context, tx db.Tx) error {
		_, err := tx.UpdateNode(ctx, &node)
		return err
	})

	require.ErrorIs(t, err, wantErr)
	assertQueueEmpty(t, queue)
	database.AssertExpectations(t)
	database.MockTx.AssertExpectations(t)
}

func TestUnit_DBNodeSourceSeedsNodes(t *testing.T) {
	first := db.Node{ID: uuid.New()}
	second := db.Node{ID: uuid.New()}
	filters := db.NodeFilters{
		Provisioning: ptr.To(true),
		Deleting:     ptr.To(false),
	}
	database := &testutil.MockDB{}
	database.On("ListNodes", mock.Anything, filters).Return([]db.Node{first, second}, nil)
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

func TestUnit_DBNodeSourceReturnsErrorWhenEnqueueContextCancels(t *testing.T) {
	node := db.Node{ID: uuid.New()}
	database := &testutil.MockDB{}
	database.On("ListNodes", mock.Anything, db.NodeFilters{}).Return([]db.Node{node}, nil)
	source := nodeEventSource{queries: database}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := source.Start(ctx, func(context.Context, dbreconcile.Key) error {
		return ctx.Err()
	})

	require.Error(t, err)
	require.ErrorContains(t, err, "failed to enqueue node")
	database.AssertExpectations(t)
}

func startNodeSource(t *testing.T, wrapper *DB, filters db.NodeFilters) workqueue.TypedRateLimitingInterface[dbreconcile.Key] {
	t.Helper()

	source := wrapper.NodeSource(filters)
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[dbreconcile.Key]())
	t.Cleanup(queue.ShutDown)
	require.NoError(t, source.Start(context.Background(), queue))
	return queue
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

type errorEnqueuer struct {
	err error
}

func (e errorEnqueuer) Enqueue(context.Context, dbreconcile.Key) error {
	return e.err
}
