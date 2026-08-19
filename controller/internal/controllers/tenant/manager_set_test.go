// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tenant

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
)

const (
	testEventuallyTimeout  = time.Second
	testEventuallyInterval = time.Millisecond
)

type runtimeFactoryFunc func(context.Context, Target) (Runtime, error)

func (f runtimeFactoryFunc) Start(ctx context.Context, target Target) (Runtime, error) {
	return f(ctx, target)
}

type fakeRuntime struct {
	done     chan error
	stopOnce sync.Once
	onStop   func()
}

func newFakeRuntime(onStop func()) *fakeRuntime {
	return &fakeRuntime{done: make(chan error, 1), onStop: onStop}
}

func (r *fakeRuntime) Done() <-chan error {
	return r.done
}

func (r *fakeRuntime) Stop() {
	r.stopOnce.Do(func() {
		if r.onStop != nil {
			r.onStop()
		}
		close(r.done)
	})
}

func TestManagerSetStartsDeduplicatesAndStopsRuntime(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	starts := 0
	stops := 0
	factory := runtimeFactoryFunc(func(_ context.Context, _ Target) (Runtime, error) {
		mu.Lock()
		starts++
		mu.Unlock()
		return newFakeRuntime(func() {
			mu.Lock()
			stops++
			mu.Unlock()
		}), nil
	})

	managerSet := newTestManagerSet(t, ctx, factory, 2)
	t.Cleanup(managerSet.Shutdown)
	target := testTarget(uuid.New(), "cluster-a")

	require.NoError(t, managerSet.Upsert(ctx, target))
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return starts == 1
	}, time.Second, time.Millisecond)

	require.NoError(t, managerSet.Upsert(ctx, target))
	time.Sleep(10 * time.Millisecond)
	mu.Lock()
	require.Equal(t, 1, starts)
	mu.Unlock()

	managerSet.RemoveCluster(target.ClusterID)
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return stops == 1
	}, time.Second, time.Millisecond)
}

func TestManagerSetRestartsOnlyChangedKubeconfigTarget(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	starts := map[uuid.UUID]int{}
	stops := map[uuid.UUID]int{}
	factory := runtimeFactoryFunc(func(_ context.Context, target Target) (Runtime, error) {
		mu.Lock()
		starts[target.ClusterID]++
		mu.Unlock()
		return newFakeRuntime(func() {
			mu.Lock()
			stops[target.ClusterID]++
			mu.Unlock()
		}), nil
	})

	managerSet := newTestManagerSet(t, ctx, factory, 2)
	t.Cleanup(managerSet.Shutdown)
	clusterA := uuid.New()
	clusterB := uuid.New()
	targetA := testTarget(clusterA, "cluster-a")
	targetB := testTarget(clusterB, "cluster-b")
	require.NoError(t, managerSet.Upsert(ctx, targetA))
	require.NoError(t, managerSet.Upsert(ctx, targetB))
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return starts[clusterA] == 1 && starts[clusterB] == 1
	}, time.Second, time.Millisecond)

	targetA.Kubeconfig = []byte("rotated-kubeconfig")
	require.NoError(t, managerSet.Upsert(ctx, targetA))
	require.NoError(t, managerSet.Upsert(ctx, targetB))
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return starts[clusterA] == 2 && starts[clusterB] == 1 && stops[clusterA] == 1 && stops[clusterB] == 0
	}, time.Second, time.Millisecond)

	targetA.KubeconfigSecretUID = types.UID("replacement-secret")
	require.NoError(t, managerSet.Upsert(ctx, targetA))
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return starts[clusterA] == 3 && starts[clusterB] == 1 && stops[clusterA] == 2 && stops[clusterB] == 0
	}, time.Second, time.Millisecond)
}

func TestManagerSetRetriesOneClusterWithoutBlockingAnother(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clusterA := uuid.New()
	clusterB := uuid.New()
	var mu sync.Mutex
	attempts := map[uuid.UUID]int{}
	factory := runtimeFactoryFunc(func(_ context.Context, target Target) (Runtime, error) {
		mu.Lock()
		attempts[target.ClusterID]++
		attempt := attempts[target.ClusterID]
		mu.Unlock()
		if target.ClusterID == clusterA && attempt <= 2 {
			return nil, errors.New("tenant API unavailable")
		}
		return newFakeRuntime(nil), nil
	})

	managerSet := newTestManagerSet(t, ctx, factory, 1)
	t.Cleanup(managerSet.Shutdown)
	require.NoError(t, managerSet.Upsert(ctx, testTarget(clusterA, "cluster-a")))
	require.NoError(t, managerSet.Upsert(ctx, testTarget(clusterB, "cluster-b")))

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return attempts[clusterA] >= 3 && attempts[clusterB] == 1
	}, time.Second, time.Millisecond)
}

func TestManagerSetBoundsTwentyConcurrentStarts(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	release := make(chan struct{})
	var mu sync.Mutex
	activeStarts := 0
	maximumStarts := 0
	completedStarts := 0
	factory := runtimeFactoryFunc(func(ctx context.Context, _ Target) (Runtime, error) {
		mu.Lock()
		activeStarts++
		if activeStarts > maximumStarts {
			maximumStarts = activeStarts
		}
		mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
		}

		mu.Lock()
		activeStarts--
		completedStarts++
		mu.Unlock()
		return newFakeRuntime(nil), nil
	})

	managerSet := newTestManagerSet(t, ctx, factory, 3)
	t.Cleanup(managerSet.Shutdown)
	for range 20 {
		clusterID := uuid.New()
		require.NoError(t, managerSet.Upsert(ctx, testTarget(clusterID, clusterID.String())))
	}
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return activeStarts == 3 && maximumStarts == 3
	}, time.Second, time.Millisecond)

	close(release)
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return completedStarts == 20 && maximumStarts == 3
	}, time.Second, time.Millisecond)
}

func TestManagerSetShutdownStopsAllRuntimes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	var mu sync.Mutex
	starts := 0
	stops := 0
	factory := runtimeFactoryFunc(func(_ context.Context, _ Target) (Runtime, error) {
		mu.Lock()
		starts++
		mu.Unlock()
		return newFakeRuntime(func() {
			mu.Lock()
			stops++
			mu.Unlock()
		}), nil
	})

	managerSet := newTestManagerSet(t, ctx, factory, 2)
	require.NoError(t, managerSet.Upsert(ctx, testTarget(uuid.New(), "cluster-a")))
	require.NoError(t, managerSet.Upsert(ctx, testTarget(uuid.New(), "cluster-b")))
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return starts == 2
	}, time.Second, time.Millisecond)

	managerSet.Shutdown()
	mu.Lock()
	require.Equal(t, 2, stops)
	mu.Unlock()
}

func newTestManagerSet(t *testing.T, ctx context.Context, factory RuntimeFactory, maxConcurrentStarts int) *ManagerSet {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	managerSet, err := NewManagerSet(ctx, logger, factory, ManagerSetOptions{
		MaxConcurrentStarts: maxConcurrentStarts,
		StartupJitter:       0,
		InitialRetryBackoff: time.Millisecond,
		MaxRetryBackoff:     5 * time.Millisecond,
	})
	require.NoError(t, err)
	return managerSet
}

func testTarget(clusterID uuid.UUID, clusterName string) Target {
	return Target{
		ClusterID:   clusterID,
		ClusterName: clusterName,
		KubeconfigSecretRef: types.NamespacedName{
			Namespace: "nke-system",
			Name:      clusterName + "-kubeconfig",
		},
		KubeconfigSecretUID: types.UID("uid-" + clusterName),
		Kubeconfig:          []byte("kubeconfig"),
	}
}
