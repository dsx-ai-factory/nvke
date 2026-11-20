// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodeagentreadiness

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent"
	"github.com/dsx-ai-factory/nvke/controller/internal/agent/agenttest"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile/dbwrapper"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

type logLine struct {
	level   slog.Level
	message string
	attrs   map[string]any
}

// captureHandler records the lines logged by the readiness controller.
type captureHandler struct {
	mu    *sync.Mutex
	lines *[]logLine
}

func newCaptureHandler() captureHandler {
	return captureHandler{mu: &sync.Mutex{}, lines: &[]logLine{}}
}

func (h captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h captureHandler) Handle(_ context.Context, record slog.Record) error {
	line := logLine{level: record.Level, message: record.Message, attrs: map[string]any{}}
	record.Attrs(func(attr slog.Attr) bool {
		line.attrs[attr.Key] = attr.Value.Any()
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.lines = append(*h.lines, line)
	return nil
}

func (h captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h captureHandler) WithGroup(string) slog.Handler { return h }

// take returns the lines logged since the last call.
func (h captureHandler) take() []logLine {
	h.mu.Lock()
	defer h.mu.Unlock()
	lines := *h.lines
	*h.lines = nil
	return lines
}

func linesAt(lines []logLine, level slog.Level) []logLine {
	var matched []logLine
	for _, line := range lines {
		if line.level == level {
			matched = append(matched, line)
		}
	}
	return matched
}

type transitionHarness struct {
	t          *testing.T
	controller *AgentReadinessController
	agent      *agenttest.FakeClient
	logs       captureHandler
	node       db.Node
	commitErr  error
}

func newTransitionHarness(t *testing.T, node db.Node) *transitionHarness {
	t.Helper()

	logs := newCaptureHandler()
	fakeAgent := &agenttest.FakeClient{}
	return &transitionHarness{
		t:          t,
		controller: NewAgentReadinessController(config.Config{}, slog.New(logs), nil, fakeAgent),
		agent:      fakeAgent,
		logs:       logs,
		node:       node,
	}
}

// probe reconciles the node once with the probe returning err, keeps the
// row the controller wrote, and returns the lines it logged.
func (h *transitionHarness) probe(err error) []logLine {
	h.t.Helper()

	h.agent.AgentVersionErr = err
	h.agent.AgentVersion = ""
	if err == nil {
		h.agent.AgentVersion = "v1.0.0"
	}

	written := h.node
	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, h.node.ID).Return(h.node, nil).Once()
	database.On("RunInTx", mock.Anything, mock.Anything).Return(h.commitErr).Maybe()
	tx := &testutil.MockTx{}
	database.MockTx = tx
	tx.On("GetNodeForUpdate", mock.Anything, h.node.ID).Return(h.node, nil).Maybe()
	tx.On("GetNode", mock.Anything, h.node.ID).Return(h.node, nil).Maybe()
	tx.On("UpdateNode", mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
		written = *args.Get(1).(*db.Node)
	}).Return(h.node, nil).Maybe()
	h.controller.db = dbwrapper.New(database, slog.New(slog.DiscardHandler))

	_, reconcileErr := h.controller.Reconcile(h.t.Context(), dbreconcile.NodeKey(h.node.ID))
	if h.commitErr != nil {
		require.ErrorIs(h.t, reconcileErr, h.commitErr)
	} else {
		require.NoError(h.t, reconcileErr)
		h.node = written
	}
	return h.logs.take()
}

func probeErr(cause agent.ProbeCause, text string) error {
	return &agent.ProbeError{Cause: cause, Err: errors.New(text)}
}

func readyNode(since time.Time) db.Node {
	return db.Node{
		ID:            uuid.New(),
		ClusterID:     uuid.New(),
		Name:          "worker-1",
		IP:            "198.51.100.20",
		SpiffeEnabled: true,
		Conditions: db.NodeConditions{
			NodeAgentReady:  db.Condition{Status: db.ConditionStatusTrue, Message: "Node agent is reachable", LastTransitionTime: since},
			KubernetesReady: db.Condition{Status: db.ConditionStatusTrue, LastTransitionTime: since},
			Rebooting:       db.Condition{Status: db.ConditionStatusFalse, LastTransitionTime: since},
			Provisioned:     db.Condition{Status: db.ConditionStatusTrue, LastTransitionTime: since},
		},
	}
}

func TestUnit_NodeAgentReadinessTransitionsAreLogged(t *testing.T) {
	h := newTransitionHarness(t, readyNode(time.Now().Add(-10*time.Minute)))
	refused := probeErr(agent.ProbeCauseConnectionRefused, "dial tcp 198.51.100.20:15005: connect: connection refused")

	// True to False: one Info line with the node, its profile, the cause and the error.
	lines := h.probe(refused)
	info := linesAt(lines, slog.LevelInfo)
	require.Len(t, info, 1, "lines: %v", lines)
	assert.Equal(t, "Node agent readiness changed", info[0].message)
	assert.Equal(t, h.node.ID, info[0].attrs["nodeID"])
	assert.Equal(t, "worker-1", info[0].attrs["nodeName"])
	assert.Equal(t, h.node.ClusterID, info[0].attrs["clusterID"])
	assert.Equal(t, "198.51.100.20", info[0].attrs["nodeIP"])
	assert.Equal(t, "spiffe", info[0].attrs["profile"])
	assert.Equal(t, db.ConditionStatusTrue, info[0].attrs["previousStatus"])
	assert.Equal(t, db.ConditionStatusFalse, info[0].attrs["status"])
	assert.Equal(t, agent.ProbeCause(""), info[0].attrs["previousCause"])
	assert.Equal(t, agent.ProbeCauseConnectionRefused, info[0].attrs["cause"])
	assert.Equal(t, refused, info[0].attrs["error"])
	assert.Equal(t, 10*time.Minute, info[0].attrs["previousStatusDuration"])
	assert.Equal(t, db.ConditionStatusTrue, info[0].attrs["kubernetesReady"])
	assert.Equal(t, db.ConditionStatusFalse, info[0].attrs["rebooting"])
	assert.Equal(t, db.ConditionStatusTrue, info[0].attrs["provisioned"])

	// The same failure again logs nothing.
	assert.Empty(t, h.probe(refused))

	// The same cause with a new message logs at Debug only.
	lines = h.probe(probeErr(agent.ProbeCauseConnectionRefused, "dial tcp 198.51.100.20:15005: connect: connection refused (retry)"))
	assert.Empty(t, linesAt(lines, slog.LevelInfo))
	require.Len(t, linesAt(lines, slog.LevelDebug), 1)

	// A new cause while False: one Info line naming both causes.
	lines = h.probe(probeErr(agent.ProbeCauseUnreachable, "dial tcp 198.51.100.20:15005: i/o timeout"))
	info = linesAt(lines, slog.LevelInfo)
	require.Len(t, info, 1, "lines: %v", lines)
	assert.Equal(t, agent.ProbeCauseConnectionRefused, info[0].attrs["previousCause"])
	assert.Equal(t, agent.ProbeCauseUnreachable, info[0].attrs["cause"])
	assert.NotContains(t, info[0].attrs, "previousStatusDuration", "the status did not change")

	// False to True: one Info line with the time spent False.
	lines = h.probe(nil)
	info = linesAt(lines, slog.LevelInfo)
	require.Len(t, info, 1, "lines: %v", lines)
	assert.Equal(t, db.ConditionStatusFalse, info[0].attrs["previousStatus"])
	assert.Equal(t, db.ConditionStatusTrue, info[0].attrs["status"])
	assert.Equal(t, agent.ProbeCauseUnreachable, info[0].attrs["previousCause"])
	assert.Equal(t, agent.ProbeCause(""), info[0].attrs["cause"])
	assert.Contains(t, info[0].attrs, "previousStatusDuration")
	assert.NotContains(t, info[0].attrs, "error")

	// Staying True logs nothing.
	assert.Empty(t, h.probe(nil))
}

func TestUnit_NodeAgentReadinessTransitionCauses(t *testing.T) {
	tests := map[string]struct {
		node        func(db.Node) db.Node
		err         error
		wantCause   agent.ProbeCause
		wantMessage string
	}{
		"a node without an IP has no address, whatever the dial returned": {
			node:        func(node db.Node) db.Node { node.IP = ""; return node },
			err:         probeErr(agent.ProbeCauseConnectionRefused, "dial tcp :15005: connect: connection refused"),
			wantCause:   agent.ProbeCauseNoAddress,
			wantMessage: "Node agent readiness changed",
		},
		"an error without a recorded cause is classified by its text": {
			node:        func(node db.Node) db.Node { return node },
			err:         errors.New("failed to get agent version: connection reset by peer"),
			wantCause:   agent.ProbeCauseConnectionLost,
			wantMessage: "Node agent readiness changed",
		},
		"a credential failure says it affects every node with the profile": {
			node:      func(node db.Node) db.Node { return node },
			err:       probeErr(agent.ProbeCauseTransportSetup, "failed to create SPIFFE X509 source: context deadline exceeded"),
			wantCause: agent.ProbeCauseTransportSetup,
			wantMessage: "Node agent readiness changed: the controller could not build the transport credentials, " +
				"so every node with this profile fails the same way",
		},
	}
	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			h := newTransitionHarness(t, params.node(readyNode(time.Now())))
			info := linesAt(h.probe(params.err), slog.LevelInfo)
			require.Len(t, info, 1)
			assert.Equal(t, params.wantMessage, info[0].message)
			assert.Equal(t, params.wantCause, info[0].attrs["cause"])
		})
	}
}

// After a restart the previous cause is unknown, so a node that is already
// failing logs its cause once, then stays quiet.
func TestUnit_NodeAgentReadinessCauseLoggedOnceAfterRestart(t *testing.T) {
	node := readyNode(time.Now())
	node.Conditions.NodeAgentReady = db.Condition{
		Status:             db.ConditionStatusFalse,
		Message:            "dial tcp 198.51.100.20:15005: connect: connection refused",
		LastTransitionTime: time.Now().Add(-time.Hour),
	}
	h := newTransitionHarness(t, node)
	refused := probeErr(agent.ProbeCauseConnectionRefused, node.Conditions.NodeAgentReady.Message)

	info := linesAt(h.probe(refused), slog.LevelInfo)
	require.Len(t, info, 1)
	assert.Equal(t, agent.ProbeCause(""), info[0].attrs["previousCause"])
	assert.Equal(t, agent.ProbeCauseConnectionRefused, info[0].attrs["cause"])

	assert.Empty(t, h.probe(refused))
}

// A change whose write does not commit is not logged.
func TestUnit_NodeAgentReadinessTransitionNotLoggedWithoutCommit(t *testing.T) {
	h := newTransitionHarness(t, readyNode(time.Now()))
	h.commitErr = errors.New("database unavailable")

	lines := h.probe(probeErr(agent.ProbeCauseConnectionRefused, "connection refused"))
	assert.Empty(t, linesAt(lines, slog.LevelInfo))
}
