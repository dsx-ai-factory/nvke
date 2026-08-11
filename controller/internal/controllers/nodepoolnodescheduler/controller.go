// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package nodepoolnodescheduler schedules pending node-pool-managed nodes.
package nodepoolnodescheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/NVIDIA/nke/controller/internal/capi"
	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile"
	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile/dbwrapper"
	dbfilters "github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile/filters"
	"github.com/NVIDIA/nke/controller/internal/controllers/nodescheduling"
	"github.com/NVIDIA/nke/controller/internal/db"
)

// NodePoolNodeScheduler schedules pending nodes owned by a node pool.
type NodePoolNodeScheduler struct {
	// capacityAccount spans reconciles rather than living inside one, so pools
	// sharing an instance type draw from a single budget. Reconciles of this
	// controller are serialized, but capacity is consumed asynchronously by the
	// ProvisionNode workflow, so without it each pool sees capacity the previous
	// pool has already spent.
	capacityAccount *nodescheduling.CapacityAccount

	// controlPlanePoolAccount is deliberately separate from capacityAccount.
	// Control-plane pools are ordered ahead of workers for the same instance
	// type and are already counted by the reservation ledger, so charging them
	// to the worker budget would subtract them twice.
	controlPlanePoolAccount *nodescheduling.CapacityAccount
	cfg                     config.Config
	db                      *dbwrapper.DB
	logger                  *slog.Logger
	service                 *nodescheduling.Service
}

// NewNodePoolNodeScheduler creates a NodePoolNodeScheduler.
func NewNodePoolNodeScheduler(
	cfg config.Config,
	logger *slog.Logger,
	dbInstance *dbwrapper.DB,
	cweTemporalClient nodescheduling.WorkflowStarter,
	forgeProvider nodescheduling.ForgeResourceReader,
) *NodePoolNodeScheduler {
	return &NodePoolNodeScheduler{
		cfg:                     cfg,
		db:                      dbInstance,
		logger:                  logger,
		service:                 nodescheduling.NewService(cfg, logger, dbInstance, cweTemporalClient, forgeProvider),
		capacityAccount:         nodescheduling.NewCapacityAccount(nodescheduling.DefaultCapacityAccountTTL),
		controlPlanePoolAccount: nodescheduling.NewCapacityAccount(nodescheduling.DefaultCapacityAccountTTL),
	}
}

// SetupWithManager wires the scheduler into controller-runtime.
func (s *NodePoolNodeScheduler) SetupWithManager(mgr manager.Manager) error {
	return dbreconcile.SetupController(
		mgr,
		"node-pool-node-scheduler",
		s,
		dbwrapper.NewNodePoolSource(s.db, dbfilters.NodePool{}),
		dbwrapper.NewNodeSource(s.db, dbfilters.Node{
			Pending:  ptr.To(true),
			Deleting: ptr.To(false),
		}, nodePoolKeysForPendingNode),
	)
}

func nodePoolKeysForPendingNode(_ context.Context, node db.Node) ([]dbreconcile.Key, error) {
	if node.NodePoolID == uuid.Nil {
		return nil, nil
	}
	return []dbreconcile.Key{dbreconcile.NodePoolKey(node.NodePoolID)}, nil
}

// Reconcile schedules pending nodes for one node pool.
func (s *NodePoolNodeScheduler) Reconcile(ctx context.Context, key dbreconcile.Key) (reconcile.Result, error) {
	nodePool, err := s.db.GetNodePool(ctx, key.ID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			s.logger.Debug("Node pool not found, skipping node pool node scheduler reconciliation", "nodePoolID", key.ID)
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("failed to get node pool %s: %w", key.ID, err)
	}

	return s.reconcileNodePool(ctx, nodePool)
}

func (s *NodePoolNodeScheduler) reconcileNodePool(ctx context.Context, nodePool db.NodePool) (reconcile.Result, error) {
	logger := s.logger.With("nodePoolID", nodePool.ID)

	if nodePool.AutoProvisionDisabled {
		logger.Debug("Node pool has auto-provision disabled, skipping node pool nodes scheduling")
		return reconcile.Result{}, nil
	}
	if nodePool.Conditions.Deleting.Status == db.ConditionStatusTrue {
		logger.Debug("Node pool is deleting, skipping node pool nodes scheduling")
		return reconcile.Result{}, nil
	}

	cluster, err := s.db.GetCluster(ctx, nodePool.ClusterID)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to get cluster for node pool: %w", err)
	}

	pendingNodes, err := s.db.ListNodes(ctx, db.NodeFilters{
		NodePoolID: &nodePool.ID,
		Pending:    ptr.To(true),
		Deleting:   ptr.To(false),
	})
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to list pending nodes: %w", err)
	}

	if capi.ShouldUseCAPIForNodeProvisioning(s.cfg.EnableForgeCapi, cluster) {
		return s.service.ScheduleCAPIInfraNodes(ctx, logger, pendingNodes)
	}

	nicoNodePoolProvider, err := nodePool.NicoProvider()
	if err != nil {
		return reconcile.Result{}, err
	}

	return s.service.ScheduleForgeNodes(ctx, logger, cluster, pendingNodes, nicoNodePoolProvider.InstanceTypeID, nodescheduling.ForgePlacementSpec{
		SubnetIDs:    nicoNodePoolProvider.SubnetIDs,
		VpcPrefixIDs: nicoNodePoolProvider.VpcPrefixIDs,
	}, s.forgeScheduleOptions(nodePool))
}

// forgeScheduleOptions picks the budget and reservation behavior for one pool.
//
// A control-plane pool schedules control-plane nodes itself, so it must not
// apply the control-plane reservation -- the ledger counts the very nodes it is
// scheduling, and the pool would block itself. It also keeps its own budget so
// its nodes are not charged to the worker account on top of being reserved.
func (s *NodePoolNodeScheduler) forgeScheduleOptions(nodePool db.NodePool) nodescheduling.ForgeScheduleOptions {
	if nodePool.ControlPlane {
		return nodescheduling.ForgeScheduleOptions{
			Account:                s.controlPlanePoolAccount,
			SchedulingControlPlane: true,
		}
	}
	return nodescheduling.ForgeScheduleOptions{Account: s.capacityAccount}
}
