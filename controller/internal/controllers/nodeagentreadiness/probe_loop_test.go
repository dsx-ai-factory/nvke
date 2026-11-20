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

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile/dbwrapper"
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

type sequenceAgent struct {
	agent.Client
	calls  atomic.Int32
	varied bool
}

func (a *sequenceAgent) GetAgentVersion(_ context.Context, _ db.Node) (string, error) {
	n := a.calls.Add(1)
	if a.varied {
		return "", fmt.Errorf("read tcp 192.0.2.10:%d->198.51.100.20:15005: read: connection reset by peer", 40000+n)
	}
	return "", fmt.Errorf("dial tcp 198.51.100.20:15005: connect: connection refused")
}

// A probe failure writes the False condition once. Probes that produce the
// same failure text after normalization must not write again: the row write
// is what requeues the node on the readiness source, so a message that
// changes every attempt turns the 2m interval into a ~1s probe loop.
func TestUnit_ProbeErrorNormalizationStopsRepeatWrites(t *testing.T) {
	for _, tc := range []struct {
		name   string
		varied bool
	}{
		{"constant error text", false},
		{"error text with local port", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodeID := uuid.New()
			mem := &memDB{node: db.Node{ID: nodeID, Name: "w1", IP: "198.51.100.20", Conditions: db.NodeConditions{
				NodeAgentReady: db.NewCondition(true, "Node agent is reachable"),
			}}}
			wrapped := dbwrapper.New(mem, slog.New(slog.DiscardHandler))
			ag := &sequenceAgent{varied: tc.varied}
			c := NewAgentReadinessController(config.Config{}, slog.New(slog.DiscardHandler), wrapped, ag)

			for range 3 {
				_, err := c.Reconcile(context.Background(), dbreconcile.NodeKey(nodeID))
				require.NoError(t, err)
			}

			require.Equal(t, int32(3), ag.calls.Load(), "every Reconcile probes once")
			require.Equal(t, int32(1), mem.writes.Load(), "only the True->False transition may write")

			if tc.varied {
				require.Equal(t,
					"read tcp ->198.51.100.20:15005: read: connection reset by peer",
					mem.node.Conditions.NodeAgentReady.Message)
			}
		})
	}
}
