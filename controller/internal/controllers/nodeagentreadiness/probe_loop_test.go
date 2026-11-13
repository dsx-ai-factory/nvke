// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodeagentreadiness

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile/dbwrapper"
	dbfilters "github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile/filters"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// memDB stores one node in memory. Unimplemented methods panic via the nil embedded interface.
type memDB struct {
	db.DB
	mu     sync.Mutex
	node   db.Node
	writes atomic.Int32
}

func (m *memDB) GetNode(_ context.Context, _ uuid.UUID) (db.Node, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.node, nil
}

func (m *memDB) ListNodes(_ context.Context, _ db.NodeFilters) ([]db.Node, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return []db.Node{m.node}, nil
}

func (m *memDB) RunInTx(ctx context.Context, fn func(context.Context, db.Tx) error) error {
	return fn(ctx, &memTx{m: m})
}

type memTx struct {
	db.Tx
	m *memDB
}

func (t *memTx) GetNode(ctx context.Context, id uuid.UUID) (db.Node, error) {
	return t.m.GetNode(ctx, id)
}

func (t *memTx) GetNodeForUpdate(ctx context.Context, id uuid.UUID) (db.Node, error) {
	return t.m.GetNode(ctx, id)
}

func (t *memTx) UpdateNode(_ context.Context, n *db.Node) (db.Node, error) {
	t.m.mu.Lock()
	defer t.m.mu.Unlock()
	t.m.node = *n
	t.m.writes.Add(1)
	return *n, nil
}

type countingAgent struct {
	agent.Client
	calls  atomic.Int32
	varied bool
}

func (a *countingAgent) GetAgentVersion(_ context.Context, _ db.Node) (string, error) {
	n := a.calls.Add(1)
	if a.varied {
		return "", fmt.Errorf("read tcp 192.0.2.10:%d->198.51.100.20:15005: read: connection reset by peer", 40000+n)
	}
	return "", fmt.Errorf("dial tcp 198.51.100.20:15005: connect: connection refused")
}

func runLoop(t *testing.T, varied bool, d time.Duration) (int32, int32) {
	t.Helper()
	nodeID := uuid.New()
	mem := &memDB{node: db.Node{ID: nodeID, Name: "w1", IP: "198.51.100.20", Conditions: db.NodeConditions{
		NodeAgentReady: db.NewCondition(true, "Node agent is reachable"),
	}}}
	wrapped := dbwrapper.New(mem, slog.New(slog.DiscardHandler))
	ag := &countingAgent{varied: varied}
	c := NewAgentReadinessController(config.Config{}, slog.New(slog.DiscardHandler), wrapped, ag)

	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[dbreconcile.Key]())
	defer q.ShutDown()
	src := dbwrapper.NewNodeSource(wrapped, dbfilters.Node{Deleting: ptr.To(false)})
	if err := src.Start(ctx, q); err != nil {
		t.Fatal(err)
	}
	go func() { <-ctx.Done(); q.ShutDown() }()
	for {
		key, shutdown := q.Get()
		if shutdown {
			break
		}
		res, err := c.Reconcile(ctx, key)
		if err == nil && res.RequeueAfter > 0 {
			q.AddAfter(key, res.RequeueAfter)
		}
		q.Done(key)
	}
	return ag.calls.Load(), mem.writes.Load()
}

// With the default 2m interval, a failing node should be probed at most twice in 6s:
// the first probe, and the confirming re-probe after the False write.
func TestUnit_AgentReadinessErrorTextDoesNotDriveReprobes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		varied bool
	}{{"constant error text", false}, {"error text with local port", true}} {
		t.Run(tc.name, func(t *testing.T) {
			probes, writes := runLoop(t, tc.varied, 6*time.Second)
			t.Logf("probes=%d writes=%d in 6s", probes, writes)
			if probes > 2 {
				t.Fatalf("got %d probes in 6s, want at most 2", probes)
			}
		})
	}
}
