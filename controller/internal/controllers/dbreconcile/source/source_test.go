// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package source

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/util/workqueue"
)

func TestUnit_SourceEnqueue(t *testing.T) {
	src := NewSource[string](1)
	key := "node-pool/first"

	require.NoError(t, src.Enqueue(context.Background(), key))

	select {
	case got := <-src.events:
		assert.Equal(t, key, got.Object)
	default:
		t.Fatal("expected enqueue to publish a generic event")
	}
}

func TestUnit_SourceEnqueueReturnsErrorWhenContextCancelsBeforeBufferSpace(t *testing.T) {
	src := NewSource[string](1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	require.NoError(t, src.Enqueue(context.Background(), "node-pool/first"))
	err := src.Enqueue(ctx, "node-pool/second")
	require.Error(t, err)
	assert.ErrorContains(t, err, "failed to enqueue DB reconcile event")
}

func TestUnit_SourceQueuesEnqueuedKey(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	src := NewSource[string](1)
	queue := newQueue()
	defer queue.ShutDown()

	require.NoError(t, src.Start(ctx, queue))

	key := "node-pool/first"
	require.NoError(t, src.Enqueue(ctx, key))

	got := getQueueItem(t, queue)
	assert.Equal(t, key, got)
}

func TestUnit_SourceStartsEventSources(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	first := "cluster/first"
	second := "node/second"
	started := []string{}
	src := NewSource[string](
		2,
		eventSourceFunc[string](func(ctx context.Context, enqueue func(context.Context, string) error) error {
			started = append(started, "first")
			require.NoError(t, enqueue(ctx, first))
			return nil
		}),
		eventSourceFunc[string](func(ctx context.Context, enqueue func(context.Context, string) error) error {
			started = append(started, "second")
			require.NoError(t, enqueue(ctx, second))
			return nil
		}),
	)
	queue := newQueue()
	defer queue.ShutDown()

	require.NoError(t, src.Start(ctx, queue))

	assert.Equal(t, []string{"first", "second"}, started)
	assert.ElementsMatch(t, []string{first, second}, []string{
		getQueueItem(t, queue),
		getQueueItem(t, queue),
	})
}

func TestUnit_SourceReturnsEventSourceError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wantErr := errors.New("seed failed")
	src := NewSource[string](1, eventSourceFunc[string](func(_ context.Context, _ func(context.Context, string) error) error {
		return wantErr
	}))
	queue := newQueue()
	defer queue.ShutDown()

	err := src.Start(ctx, queue)
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
}

func TestUnit_SourcePassesContextToEventSources(t *testing.T) {
	type contextKey string

	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), contextKey("source"), "startup"))
	defer cancel()

	var got string
	src := NewSource[string](1, eventSourceFunc[string](func(ctx context.Context, _ func(context.Context, string) error) error {
		got, _ = ctx.Value(contextKey("source")).(string)
		return nil
	}))
	queue := newQueue()
	defer queue.ShutDown()

	require.NoError(t, src.Start(ctx, queue))
	assert.Equal(t, "startup", got)
}

type eventSourceFunc[T comparable] func(ctx context.Context, enqueue func(context.Context, T) error) error

func (f eventSourceFunc[T]) Start(ctx context.Context, enqueue func(context.Context, T) error) error {
	return f(ctx, enqueue)
}

func newQueue() workqueue.TypedRateLimitingInterface[string] {
	return workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]())
}

func getQueueItem(t *testing.T, queue workqueue.TypedRateLimitingInterface[string]) string {
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
