// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodeagentreadiness

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/dsx-ai-factory/nke/controller/internal/agent"
	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/controllers/dbreconcile"
	"github.com/dsx-ai-factory/nke/controller/internal/controllers/dbreconcile/dbwrapper"
	dbfilters "github.com/dsx-ai-factory/nke/controller/internal/controllers/dbreconcile/filters"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

const (
	agentReadinessResyncInterval = 2 * time.Minute
)

// AgentReadinessController reconciles node-agent reachability for CWE nodes.
type AgentReadinessController struct {
	cfg             config.Config
	db              *dbwrapper.DB
	nodeAgentClient agent.Client
	logger          *slog.Logger
}

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

func (c *AgentReadinessController) SetupWithManager(mgr manager.Manager) error {
	return dbreconcile.SetupController(
		mgr,
		"node-agent-health",
		c,
		dbwrapper.NewNodeSource(c.db, dbfilters.Node{
			Deleting: ptr.To(false),
		}),
	)
}

func (c *AgentReadinessController) Reconcile(ctx context.Context, key dbreconcile.Key) (reconcile.Result, error) {
	node, err := c.db.GetNode(ctx, key.ID)
	if errors.Is(err, db.ErrNotFound) {
		return reconcile.Result{}, nil
	}
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("getting node %s: %w", key.ID, err)
	}
	if node.Conditions.Deleting.Status == db.ConditionStatusTrue {
		return reconcile.Result{}, nil
	}

	probeCtx, cancel := context.WithTimeout(ctx, c.cfg.NodeHealthCheckTimeout)
	defer cancel()

	agentVersion, probeErr := c.nodeAgentClient.GetAgentVersion(probeCtx, node)
	probe := nodeAgentProbe{agentVersion: agentVersion, err: probeErr}
	if !applyNodeAgentReadiness(&node, probe) {
		return reconcile.Result{RequeueAfter: agentReadinessResyncInterval}, nil
	}

	if err := c.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		lockedNode, err := tx.GetNodeForUpdate(ctx, node.ID)
		if err != nil {
			return err
		}
		if lockedNode.Conditions.Deleting.Status == db.ConditionStatusTrue {
			return nil
		}

		if !applyNodeAgentReadiness(&lockedNode, probe) {
			return nil
		}

		_, err = tx.UpdateNode(ctx, &lockedNode)
		return err
	}); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("updating node agent health for node %s: %w", node.ID, err)
	}

	return reconcile.Result{RequeueAfter: agentReadinessResyncInterval}, nil
}
