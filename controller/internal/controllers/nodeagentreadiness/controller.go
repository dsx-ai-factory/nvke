// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodeagentreadiness

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile/dbwrapper"
	dbfilters "github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile/filters"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/metrics"
)

// AgentReadinessController reconciles node-agent reachability for controller-managed nodes.
type AgentReadinessController struct {
	cfg             config.Config
	db              *dbwrapper.DB
	nodeAgentClient agent.Client
	logger          *slog.Logger
	causes          causeTracker
}

// NewAgentReadinessController constructs the node-agent reachability reconciler.
func NewAgentReadinessController(
	cfg config.Config,
	logger *slog.Logger,
	dbInstance *dbwrapper.DB,
	nodeAgentClient agent.Client,
) *AgentReadinessController {
	if logger == nil {
		logger = slog.Default()
	}
	return &AgentReadinessController{
		cfg:             cfg,
		db:              dbInstance,
		nodeAgentClient: nodeAgentClient,
		logger:          logger.With("controller", "node-agent-health"),
	}
}

// SetupWithManager registers the reconciler with its configured worker limit.
func (c *AgentReadinessController) SetupWithManager(mgr manager.Manager) error {
	if err := c.cfg.AgentReadiness.Validate(); err != nil {
		return err
	}
	return dbreconcile.AddController(mgr, dbreconcile.Controller{
		Name:       "node-agent-health",
		Reconciler: c,
		Workers:    c.cfg.AgentReadiness.WithDefaults().Workers,
		Sources:    []dbreconcile.Source{dbwrapper.NewNodeSource(c.db, dbfilters.Node{Deleting: ptr.To(false)})},
	})
}

// Reconcile probes a node agent and updates its readiness and reported version.
func (c *AgentReadinessController) Reconcile(ctx context.Context, key dbreconcile.Key) (reconcile.Result, error) {
	node, err := c.db.GetNode(ctx, key.ID)
	if errors.Is(err, db.ErrNotFound) {
		c.causes.forget(key.ID)
		return reconcile.Result{}, nil
	}
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("getting node %s: %w", key.ID, err)
	}
	if node.Conditions.Deleting.Status == db.ConditionStatusTrue {
		c.causes.forget(node.ID)
		return reconcile.Result{}, nil
	}

	probe, err := c.probeAgent(ctx, node)
	if err != nil {
		return reconcile.Result{}, err
	}
	probe.cause = probeCause(node, probe.err)
	previous := node.Conditions.NodeAgentReady
	if !applyNodeAgentReadiness(&node, probe) {
		c.logTransition(node, previous, probe)
		return reconcile.Result{RequeueAfter: c.requeueAfter()}, nil
	}

	// The transition is logged once the transaction commits, from the row
	// it was applied to.
	var applied *db.Node
	if err := c.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		applied = nil
		lockedNode, err := tx.GetNodeForUpdate(ctx, node.ID)
		if err != nil {
			return err
		}
		if lockedNode.Conditions.Deleting.Status == db.ConditionStatusTrue {
			return nil
		}

		previous = lockedNode.Conditions.NodeAgentReady
		changed := applyNodeAgentReadiness(&lockedNode, probe)
		applied = &lockedNode
		if !changed {
			return nil
		}

		_, err = tx.UpdateNode(ctx, &lockedNode)
		return err
	}); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			c.causes.forget(node.ID)
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("updating node agent health for node %s: %w", node.ID, err)
	}
	if applied == nil {
		c.causes.forget(node.ID)
		return reconcile.Result{}, nil
	}
	c.logTransition(*applied, previous, probe)

	return reconcile.Result{RequeueAfter: c.requeueAfter()}, nil
}

// probeAgent checks node-agent reachability and reports the outcome as a
// nodeAgentProbe. A node with no IP address has nothing to dial: it is
// reported unready without probing, so a pending node never dials localhost
// and the probe metrics count real dials only.
func (c *AgentReadinessController) probeAgent(ctx context.Context, node db.Node) (nodeAgentProbe, error) {
	if node.IP == "" {
		return nodeAgentProbe{err: agent.ErrNoAddress}, nil
	}

	probeCtx, cancel := context.WithTimeout(ctx, c.cfg.AgentReadiness.WithDefaults().Timeout)
	defer cancel()

	started := time.Now()
	metrics.NodeAgentProbeStarted()
	agentVersion, err := c.nodeAgentClient.GetAgentVersion(probeCtx, node)
	metrics.NodeAgentProbeFinished(time.Since(started), err)
	// Manager shutdown must not turn cancellation into a node-health failure.
	if ctx.Err() != nil {
		return nodeAgentProbe{}, ctx.Err()
	}
	return nodeAgentProbe{agentVersion: agentVersion, err: err}, nil
}

func (c *AgentReadinessController) requeueAfter() time.Duration {
	settings := c.cfg.AgentReadiness.WithDefaults()
	if settings.Jitter == 0 {
		return settings.Interval
	}
	// Scheduling jitter does not need cryptographic randomness.
	factor := 1 + settings.Jitter*(2*rand.Float64()-1) //nolint:gosec // Non-security scheduling jitter.
	return time.Duration(float64(settings.Interval) * factor)
}
