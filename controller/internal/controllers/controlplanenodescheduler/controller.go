// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package controlplanenodescheduler schedules pending control-plane nodes.
package controlplanenodescheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/controllers/dbreconcile"
	"github.com/dsx-ai-factory/nke/controller/internal/controllers/dbreconcile/dbwrapper"
	dbfilters "github.com/dsx-ai-factory/nke/controller/internal/controllers/dbreconcile/filters"
	"github.com/dsx-ai-factory/nke/controller/internal/controllers/nodescheduling"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

// ControlPlaneNodeScheduler schedules pending no-nodepool control-plane nodes.
type ControlPlaneNodeScheduler struct {
	cfg     config.Config
	db      *dbwrapper.DB
	logger  *slog.Logger
	service *nodescheduling.Service
}

// NewControlPlaneNodeScheduler creates a ControlPlaneNodeScheduler.
func NewControlPlaneNodeScheduler(
	cfg config.Config,
	logger *slog.Logger,
	dbInstance *dbwrapper.DB,
	cweTemporalClient nodescheduling.WorkflowStarter,
) *ControlPlaneNodeScheduler {
	return &ControlPlaneNodeScheduler{
		cfg:     cfg,
		db:      dbInstance,
		logger:  logger,
		service: nodescheduling.NewService(cfg, logger, dbInstance, cweTemporalClient),
	}
}

// SetupWithManager wires the scheduler into controller-runtime.
func (s *ControlPlaneNodeScheduler) SetupWithManager(mgr manager.Manager) error {
	return dbreconcile.SetupController(
		mgr,
		"control-plane-node-scheduler",
		s,
		dbwrapper.NewClusterSource(s.db, dbfilters.Cluster{}),
		dbwrapper.NewNodeSource(s.db, dbfilters.Node{
			Pending:      ptr.To(true),
			Deleting:     ptr.To(false),
			ControlPlane: ptr.To(true),
		}, clusterKeysForManagedControlPlaneNode),
	)
}

func clusterKeysForManagedControlPlaneNode(_ context.Context, node db.Node) ([]dbreconcile.Key, error) {
	if !node.IsManagedByControlPlaneController() || node.ClusterID == uuid.Nil {
		return nil, nil
	}
	return []dbreconcile.Key{dbreconcile.ClusterKey(node.ClusterID)}, nil
}

// Reconcile schedules pending managed control-plane nodes for one cluster.
func (s *ControlPlaneNodeScheduler) Reconcile(ctx context.Context, key dbreconcile.Key) (reconcile.Result, error) {
	cluster, err := s.db.GetCluster(ctx, key.ID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			s.logger.Debug("Cluster not found, skipping control-plane node scheduler reconciliation", "clusterID", key.ID)
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("failed to get cluster %s: %w", key.ID, err)
	}

	if cluster.Conditions.Deleting.Status == db.ConditionStatusTrue {
		s.logger.Debug("Cluster is deleting, skipping control-plane node scheduling", "clusterID", cluster.ID)
		return reconcile.Result{}, nil
	}

	return s.reconcileCluster(ctx, cluster)
}

func (s *ControlPlaneNodeScheduler) reconcileCluster(ctx context.Context, cluster db.Cluster) (reconcile.Result, error) {
	controlPlanePending, err := s.listManagedControlPlanePending(ctx, cluster)
	if err != nil {
		s.logger.Error("Failed to list pending control plane nodes", "error", err, "clusterID", cluster.ID)
		return reconcile.Result{}, err
	}
	if len(controlPlanePending) == 0 {
		return reconcile.Result{}, nil
	}

	logger := s.logger.With("clusterID", cluster.ID, "controlPlanePending", len(controlPlanePending))
	return s.service.ScheduleCAPIInfraNodes(ctx, logger, controlPlanePending)
}

func (s *ControlPlaneNodeScheduler) listManagedControlPlanePending(ctx context.Context, cluster db.Cluster) ([]db.Node, error) {
	pendingNodes, err := s.db.ListNodes(ctx, db.NodeFilters{
		ClusterID:    &cluster.ID,
		ControlPlane: ptr.To(true),
		Pending:      ptr.To(true),
		Deleting:     ptr.To(false),
	})
	if err != nil {
		return nil, err
	}

	managed := make([]db.Node, 0, len(pendingNodes))
	for _, node := range pendingNodes {
		if !node.IsManagedByControlPlaneController() {
			continue
		}
		managed = append(managed, node)
	}
	return managed, nil
}
