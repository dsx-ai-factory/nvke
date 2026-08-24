// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package controlplanenodescheduler schedules pending control-plane nodes.
package controlplanenodescheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

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
	"github.com/NVIDIA/nke/controller/internal/nodes"
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
	forgeProvider nodescheduling.ForgeResourceReader,
) *ControlPlaneNodeScheduler {
	return &ControlPlaneNodeScheduler{
		cfg:     cfg,
		db:      dbInstance,
		logger:  logger,
		service: nodescheduling.NewService(cfg, logger, dbInstance, cweTemporalClient, forgeProvider),
	}
}

// SetupWithManager wires the scheduler into controller-runtime.
func (s *ControlPlaneNodeScheduler) SetupWithManager(mgr manager.Manager) error {
	return dbreconcile.SetupController(
		mgr,
		"control-plane-node-scheduler",
		s,
		s.db.ClusterSource(dbfilters.Cluster{}),
		s.db.NodeSource(dbfilters.Node{
			Pending:      ptr.To(true),
			Deleting:     ptr.To(false),
			ControlPlane: ptr.To(true),
		}, clusterKeysForManagedControlPlaneNode),
	)
}

func clusterKeysForManagedControlPlaneNode(node db.Node) []dbreconcile.Key {
	if !node.IsManagedByControlPlaneController() || node.ClusterID == uuid.Nil {
		return nil
	}
	return []dbreconcile.Key{dbreconcile.ClusterKey(node.ClusterID)}
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
	if capi.ShouldUseCAPIForNodeProvisioning(s.cfg.EnableForgeCapi, cluster) {
		return s.service.ScheduleCAPIInfraNodes(ctx, logger, controlPlanePending)
	}

	return s.scheduleForgeControlPlaneNodes(ctx, logger, cluster, controlPlanePending)
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
		if node.IsManagedByControlPlaneController() {
			managed = append(managed, node)
		}
	}
	return managed, nil
}

func (s *ControlPlaneNodeScheduler) scheduleForgeControlPlaneNodes(
	ctx context.Context,
	logger *slog.Logger,
	cluster db.Cluster,
	pending []db.Node,
) (reconcile.Result, error) {
	type nodePlacement struct {
		node      db.Node
		placement nodescheduling.ForgePlacementSpec
	}
	targets := make([]nodePlacement, 0, len(pending))
	var errs []error
	for _, node := range pending {
		nodeSet, _, err := nodes.GetNodeContext(ctx, s.db, node)
		if err != nil {
			logger.Warn("Skipping control plane node with unresolved context",
				"nodeID", node.ID, "clusterID", cluster.ID, "error", err)
			errs = append(errs, fmt.Errorf("failed to resolve context for control-plane node %s: %w", node.ID, err))
			continue
		}
		nicoNodeSetProvider, err := nodeSet.NicoProvider()
		if err != nil {
			logger.Error("Skipping control plane node with unresolved node set provider",
				"nodeID", node.ID, "clusterID", cluster.ID, "error", err)
			errs = append(errs, fmt.Errorf("failed to resolve node set provider for control-plane node %s: %w", node.ID, err))
			continue
		}
		targets = append(targets, nodePlacement{node: node, placement: nodescheduling.ForgePlacementSpec{
			SubnetIDs:    append([]uuid.UUID(nil), nicoNodeSetProvider.SubnetIDs...),
			VpcPrefixIDs: append([]uuid.UUID(nil), nicoNodeSetProvider.VpcPrefixIDs...),
		}})
	}

	acct := newControlPlaneCapacityAccount(s, logger)
	for _, t := range targets {
		if t.node.InstanceTypeID != uuid.Nil {
			acct.reserve(ctx, cluster, t.node.InstanceTypeID, t.placement)
		}
	}

	result := reconcile.Result{}
	for _, t := range targets {
		node := t.node
		if node.InstanceTypeID == uuid.Nil {
			selected, unavailable, err := s.assignControlPlaneInstanceType(ctx, cluster, node, t.placement, acct, logger)
			if err != nil {
				logger.Warn("Skipping control plane node: failed to select/persist instance type",
					"nodeID", node.ID, "clusterID", cluster.ID, "error", err)
				errs = append(errs, fmt.Errorf("failed to select/persist instance type for control-plane node %s: %w", node.ID, err))
				continue
			}
			if unavailable {
				result.RequeueAfter = s.service.RetryAfter()
				continue
			}
			if selected == uuid.Nil {
				continue
			}
			node.InstanceTypeID = selected
		}

		nodeResult, err := s.service.ScheduleForgeNodes(ctx, logger, cluster, []db.Node{node}, node.InstanceTypeID, t.placement)
		if err != nil {
			logger.Error("Failed to schedule Forge control plane node",
				"error", err, "nodeID", node.ID, "instanceTypeID", node.InstanceTypeID)
			errs = append(errs, err)
			continue
		}
		if nodeResult.RequeueAfter > 0 {
			result.RequeueAfter = nodeResult.RequeueAfter
		}
	}

	return result, errors.Join(errs...)
}

func (s *ControlPlaneNodeScheduler) assignControlPlaneInstanceType(
	ctx context.Context,
	cluster db.Cluster,
	node db.Node,
	placement nodescheduling.ForgePlacementSpec,
	acct *controlPlaneCapacityAccount,
	logger *slog.Logger,
) (uuid.UUID, bool, error) {
	preference, err := s.resolveControlPlaneInstanceTypePreference(ctx, cluster, node)
	if err != nil {
		return uuid.Nil, false, err
	}

	selected, unavailable, err := s.selectControlPlaneInstanceType(ctx, cluster, preference, placement, acct)
	if err != nil || unavailable || selected == uuid.Nil {
		return selected, unavailable, err
	}

	persisted, err := s.persistControlPlaneInstanceType(ctx, node.ID, selected)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("failed to persist selected instance type: %w", err)
	}
	logger.Info("Selected Forge instance type for control plane node",
		"nodeID", node.ID, "clusterID", cluster.ID, "instanceTypeID", persisted)
	return persisted, false, nil
}

func (s *ControlPlaneNodeScheduler) selectControlPlaneInstanceType(
	ctx context.Context,
	cluster db.Cluster,
	preference []uuid.UUID,
	placement nodescheduling.ForgePlacementSpec,
	acct *controlPlaneCapacityAccount,
) (uuid.UUID, bool, error) {
	if len(preference) == 0 {
		return uuid.Nil, false, fmt.Errorf("no instance type preference configured for control-plane scheduling")
	}

	for _, instanceTypeID := range preference {
		forgeRemaining, networkRemaining, err := acct.remaining(ctx, cluster, instanceTypeID, placement)
		if err != nil {
			if errors.Is(err, db.ErrMissingProviderPayload) || !errors.Is(err, nodescheduling.ErrForgeResourcesTemporarilyUnavailable) {
				return uuid.Nil, false, err
			}
			acct.logger.Warn("Skipping candidate instance type: failed to resolve Forge capacity",
				"clusterID", cluster.ID, "instanceTypeID", instanceTypeID, "error", err)
			continue
		}
		if forgeRemaining > 0 && networkRemaining > 0 {
			acct.reserve(ctx, cluster, instanceTypeID, placement)
			return instanceTypeID, false, nil
		}
	}

	return uuid.Nil, true, nil
}

func (s *ControlPlaneNodeScheduler) resolveControlPlaneInstanceTypePreference(
	ctx context.Context,
	cluster db.Cluster,
	node db.Node,
) ([]uuid.UUID, error) {
	cfgID := node.ControlPlaneConfigurationID
	if cfgID == uuid.Nil || cfgID == cluster.ControlPlaneConfig.ConfigurationID {
		return nicoControlPlaneInstanceTypePreference(cluster.ControlPlaneConfig.ControlPlaneConfigSpec)
	}
	row, err := s.db.GetControlPlaneConfiguration(ctx, cfgID)
	if err != nil {
		return nil, fmt.Errorf("failed to get control plane configuration snapshot %s: %w", cfgID, err)
	}
	return nicoControlPlaneInstanceTypePreference(row.Config)
}

func nicoControlPlaneInstanceTypePreference(cfg db.ControlPlaneConfigSpec) ([]uuid.UUID, error) {
	nico := cfg.NicoControlPlaneProvider()
	if nico == nil {
		return nil, fmt.Errorf("nico control-plane provider payload is required")
	}
	return nico.InstanceTypePreference, nil
}

func (s *ControlPlaneNodeScheduler) persistControlPlaneInstanceType(
	ctx context.Context,
	nodeID uuid.UUID,
	instanceTypeID uuid.UUID,
) (uuid.UUID, error) {
	result := instanceTypeID
	err := s.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		node, err := tx.GetNodeForUpdate(ctx, nodeID)
		if err != nil {
			return err
		}
		if node.InstanceTypeID != uuid.Nil {
			result = node.InstanceTypeID
			return nil
		}
		node.InstanceTypeID = instanceTypeID
		if _, err := tx.UpdateNode(ctx, &node); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return uuid.Nil, err
	}
	return result, nil
}

type controlPlaneCapacityAccount struct {
	scheduler        *ControlPlaneNodeScheduler
	logger           *slog.Logger
	forgeRemaining   map[uuid.UUID]int
	networkRemaining map[string]int
}

func newControlPlaneCapacityAccount(
	scheduler *ControlPlaneNodeScheduler,
	logger *slog.Logger,
) *controlPlaneCapacityAccount {
	return &controlPlaneCapacityAccount{
		scheduler:        scheduler,
		logger:           logger,
		forgeRemaining:   make(map[uuid.UUID]int),
		networkRemaining: make(map[string]int),
	}
}

func (acct *controlPlaneCapacityAccount) remaining(
	ctx context.Context,
	cluster db.Cluster,
	instanceTypeID uuid.UUID,
	placement nodescheduling.ForgePlacementSpec,
) (int, int, error) {
	placementKey := forgeNetworkPlacementKey(placement)
	_, forgeSeeded := acct.forgeRemaining[instanceTypeID]
	_, networkSeeded := acct.networkRemaining[placementKey]

	if !forgeSeeded || !networkSeeded {
		forgeInstanceType, networkCapacity, err := acct.scheduler.service.GetForgeResources(ctx, cluster, nodescheduling.ForgePlacementSpec{
			InstanceTypeID: instanceTypeID,
			SubnetIDs:      placement.SubnetIDs,
			VpcPrefixIDs:   placement.VpcPrefixIDs,
		})
		if err != nil {
			return 0, 0, err
		}
		if !forgeSeeded {
			acct.forgeRemaining[instanceTypeID] = max(0, int(*forgeInstanceType.AllocationStats.UnusedUsable))
		}
		if !networkSeeded {
			acct.networkRemaining[placementKey] = networkCapacity
		}
	}

	return acct.forgeRemaining[instanceTypeID], acct.networkRemaining[placementKey], nil
}

func (acct *controlPlaneCapacityAccount) reserve(
	ctx context.Context,
	cluster db.Cluster,
	instanceTypeID uuid.UUID,
	placement nodescheduling.ForgePlacementSpec,
) {
	if _, _, err := acct.remaining(ctx, cluster, instanceTypeID, placement); err != nil {
		acct.logger.Warn("Failed to seed capacity for sticky control plane node",
			"clusterID", cluster.ID, "error", err)
		return
	}
	placementKey := forgeNetworkPlacementKey(placement)
	if acct.forgeRemaining[instanceTypeID] > 0 {
		acct.forgeRemaining[instanceTypeID]--
	}
	if acct.networkRemaining[placementKey] > 0 {
		acct.networkRemaining[placementKey]--
	}
}

func forgeNetworkPlacementKey(placement nodescheduling.ForgePlacementSpec) string {
	return fmt.Sprintf("%v|%v", sortedUUIDStrings(placement.SubnetIDs), sortedUUIDStrings(placement.VpcPrefixIDs))
}

func sortedUUIDStrings(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	slices.Sort(out)
	return out
}
