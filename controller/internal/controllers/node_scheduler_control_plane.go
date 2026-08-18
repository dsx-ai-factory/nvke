// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// This file holds the control-plane-controller scheduling path: the helpers and
// capacity accounting used to schedule control-plane controller managed
// control-plane nodes. The nodepool scheduling path lives in node_scheduler.go.

package controllers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/google/uuid"
	"k8s.io/utils/ptr"

	"github.com/NVIDIA/nke/controller/internal/capi"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/nodes"
)

// scheduleControlPlaneNodes schedules the control plane controller managed control-plane nodes for
// every cluster at the site
func (a *NodeScheduler) scheduleControlPlaneNodes(ctx context.Context, site db.Site) error {
	clusters, err := a.db.ListClusters(ctx, db.ClusterFilters{SiteID: &site.ID})
	if err != nil {
		return fmt.Errorf("failed to list clusters: %w", err)
	}

	failedClusters := 0

	for _, cluster := range clusters {
		if err := a.scheduleControlPlaneNodesForCluster(ctx, site, cluster); err != nil {
			// The failure is already logged with the enriched logger inside the
			// helper; here we only tally it for the end-of-pass summary.
			failedClusters++
		}
	}

	if failedClusters > 0 {
		a.logger.Warn("Control plane scheduling finished with cluster failures",
			"failedClusters", failedClusters, "totalClusters", len(clusters))
	}

	return nil
}

// scheduleControlPlaneNodesForCluster schedules the control plane controller managed control-plane
// nodes of a single cluster
func (a *NodeScheduler) scheduleControlPlaneNodesForCluster(ctx context.Context, _ db.Site, cluster db.Cluster) error {
	controlPlanePending, err := a.listManagedControlPlanePending(ctx, cluster)
	if err != nil {
		a.logger.Error("Failed to list pending control plane nodes", "error", err, "clusterID", cluster.ID)
		return err
	}
	if len(controlPlanePending) == 0 {
		return nil
	}

	logger := a.logger.With("clusterID", cluster.ID, "controlPlanePending", len(controlPlanePending))

	// CAPI (Nico templateless or template-backed): Nico owns instance-type
	// selection and availability, so route the pending nodes straight to
	// provisioning without any schedule-time Forge instance-type resolution.
	if capi.ShouldUseCAPIForNodeProvisioning(a.cfg.EnableForgeCapi, cluster) {
		return a.scheduleCapiInfraNodes(ctx, logger, controlPlanePending)
	}

	return a.scheduleForgeControlPlaneNodes(ctx, logger, cluster, controlPlanePending)
}

// listManagedControlPlanePending returns the pending control-plane nodes of a
// cluster that this scheduler owns (control plane controller managed control-plane nodes).
func (a *NodeScheduler) listManagedControlPlanePending(ctx context.Context, cluster db.Cluster) ([]db.Node, error) {
	pendingNodes, err := a.db.ListNodes(ctx, db.NodeFilters{
		ClusterID:    &cluster.ID,
		ControlPlane: ptr.To(true),
		Pending:      ptr.To(true),
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

// scheduleForgeControlPlaneNodes provisions each pending Forge CP node in one
// pass. Sticky: a node with a type keeps it; a type-less node gets a
// capacity-aware pick (Forge + IP gating, batch-accounted) persisted before
// provisioning. Nodes with an unresolved context, a failed selection, or no
// available type are skipped this pass; failures are logged and skipped so one
// node can't block the rest.
func (a *NodeScheduler) scheduleForgeControlPlaneNodes(
	ctx context.Context,
	logger *slog.Logger,
	cluster db.Cluster,
	pending []db.Node,
) error {
	if cluster.ProviderType != db.ProviderTypeNico {
		logger.Warn("Cluster is not NICo-backed, skipping forge-based control-plane scheduling",
			"clusterID", cluster.ID, "providerType", cluster.ProviderType)
		return nil
	}

	// Resolve each node's placement once; nodes with an unresolved context are
	// skipped this pass.
	type nodePlacement struct {
		node      db.Node
		placement ForgePlacementSpec
	}
	targets := make([]nodePlacement, 0, len(pending))
	for _, node := range pending {
		nodeSet, _, err := nodes.GetNodeContext(ctx, a.db, node)
		if err != nil {
			logger.Warn("Skipping control plane node with unresolved context",
				"nodeID", node.ID, "clusterID", cluster.ID, "error", err)
			continue
		}
		nicoNodeSetProvider, err := nodeSet.NicoProvider()
		if err != nil {
			logger.Error("Skipping control plane node with unresolved node set provider",
				"nodeID", node.ID, "clusterID", cluster.ID, "error", err)
			continue
		}
		targets = append(targets, nodePlacement{node: node, placement: ForgePlacementSpec{
			SubnetIDs:    append([]uuid.UUID(nil), nicoNodeSetProvider.SubnetIDs...),
			VpcPrefixIDs: append([]uuid.UUID(nil), nicoNodeSetProvider.VpcPrefixIDs...),
		}})
	}

	acct := newControlPlaneCapacityAccount(a, cluster, pending, logger)

	// Sticky pending nodes with already assigned type but not provisioned yet
	// so Forge's live counts still show their compute + IP capacity as free.
	// Reserve both ledgers for each sticky node up front so type-less selections
	// in this pass can't double-spend.
	for _, t := range targets {
		if t.node.InstanceTypeID != uuid.Nil {
			acct.reserve(ctx, cluster, t.node.InstanceTypeID, t.placement)
		}
	}

	var errs []error
	for _, t := range targets {
		node := t.node
		// Sticky nodes keep their type; type-less nodes get a capacity-aware pick
		// persisted before provisioning, else are skipped this pass.
		if node.InstanceTypeID == uuid.Nil {
			selected, err := a.assignControlPlaneInstanceType(ctx, cluster, node, t.placement, acct, logger)
			if err != nil {
				logger.Warn("Skipping control plane node: failed to select/persist instance type",
					"nodeID", node.ID, "clusterID", cluster.ID, "error", err)
				continue
			}
			if selected == uuid.Nil {
				logger.Info("Skipping control plane node this pass: no preferred instance type has capacity",
					"nodeID", node.ID, "clusterID", cluster.ID)
				continue
			}
			node.InstanceTypeID = selected
		}

		if err := a.scheduleForgeNodes(ctx, logger, cluster, []db.Node{node}, node.InstanceTypeID, t.placement); err != nil {
			logger.Error("Failed to schedule Forge control plane node",
				"error", err, "nodeID", node.ID, "instanceTypeID", node.InstanceTypeID)
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// assignControlPlaneInstanceType selects an instance type for a type-less
// pending control-plane node and persists it. It returns uuid.Nil (no error)
// when no preferred type currently has capacity, signaling the caller to skip
// the node this pass.
func (a *NodeScheduler) assignControlPlaneInstanceType(
	ctx context.Context,
	cluster db.Cluster,
	node db.Node,
	placement ForgePlacementSpec,
	acct *controlPlaneCapacityAccount,
	logger *slog.Logger,
) (uuid.UUID, error) {
	preference, err := a.resolveControlPlaneInstanceTypePreference(ctx, cluster, node)
	if err != nil {
		return uuid.Nil, err
	}

	selected, err := a.selectControlPlaneInstanceType(ctx, cluster, preference, placement, acct)
	if err != nil {
		return uuid.Nil, err
	}
	if selected == uuid.Nil {
		return uuid.Nil, nil
	}

	persisted, err := a.persistControlPlaneInstanceType(ctx, node.ID, selected)
	if err != nil {
		return uuid.Nil, fmt.Errorf("failed to persist selected instance type: %w", err)
	}
	logger.Info("Selected Forge instance type for control plane node",
		"nodeID", node.ID, "clusterID", cluster.ID, "instanceTypeID", persisted)
	return persisted, nil
}

// selectControlPlaneInstanceType picks the highest-preference instance type that
// has both Forge instance capacity and network (IP) capacity remaining in the
// current pass, decrementing the in-pass accounting on success. An empty
// preference list is a real error; a fully-queried preference with no remaining
// capacity returns uuid.Nil without error.
func (a *NodeScheduler) selectControlPlaneInstanceType(
	ctx context.Context,
	cluster db.Cluster,
	preference []uuid.UUID,
	placement ForgePlacementSpec,
	acct *controlPlaneCapacityAccount,
) (uuid.UUID, error) {
	if len(preference) == 0 {
		return uuid.Nil, fmt.Errorf("no instance type preference configured for control-plane scheduling")
	}

	// Non-NICo clusters (in practice CAPI-routed) have no Forge capacity to
	// query: treat as unbounded and pick the highest preference.
	if cluster.ProviderType != db.ProviderTypeNico {
		return preference[0], nil
	}

	for _, instanceTypeID := range preference {
		forgeRemaining, networkRemaining, err := acct.remaining(ctx, cluster, instanceTypeID, placement)
		if err != nil {
			acct.logger.Warn("Skipping candidate instance type: failed to resolve Forge capacity",
				"clusterID", cluster.ID, "instanceTypeID", instanceTypeID, "error", err)
			continue
		}
		if forgeRemaining > 0 && networkRemaining > 0 {
			acct.reserve(ctx, cluster, instanceTypeID, placement)
			return instanceTypeID, nil
		}
	}

	return uuid.Nil, nil
}

// resolveControlPlaneInstanceTypePreference returns the pinned instance-type
// preference for a control-plane node. It reads the node's
// ControlPlaneConfiguration snapshot when it differs from the cluster's current
// configuration, falling back to the cluster's current configuration otherwise.
func (a *NodeScheduler) resolveControlPlaneInstanceTypePreference(
	ctx context.Context,
	cluster db.Cluster,
	node db.Node,
) ([]uuid.UUID, error) {
	cfgID := node.ControlPlaneConfigurationID
	if cfgID == uuid.Nil || cfgID == cluster.ControlPlaneConfig.ConfigurationID {
		return nicoControlPlaneInstanceTypePreference(cluster.ControlPlaneConfig.ControlPlaneConfigSpec)
	}
	row, err := a.db.GetControlPlaneConfiguration(ctx, cfgID)
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

// persistControlPlaneInstanceType stamps the selected instance type onto the
// node row inside a transaction, before the ProvisionNode workflow is started,
// so downstream provisioning activities resolve a concrete type. It is
// idempotent and sticky: if the node already carries a non-nil InstanceTypeID
// (e.g. from a previous pass), that value is kept and returned unchanged.
func (a *NodeScheduler) persistControlPlaneInstanceType(
	ctx context.Context,
	nodeID uuid.UUID,
	instanceTypeID uuid.UUID,
) (uuid.UUID, error) {
	result := instanceTypeID
	err := a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		node, err := tx.GetNodeForUpdate(ctx, nodeID)
		if err != nil {
			return err
		}
		if node.InstanceTypeID != uuid.Nil {
			// Already stamped (raced with a prior pass): keep it sticky.
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

// controlPlaneCapacityAccount tracks remaining Forge instance and network (IP)
// capacity across a single non-CAPI Forge scheduling pass, so pending nodes are
// packed across preferred types without exceeding live capacity. forgeRemaining
// is seeded lazily per type from the live unused-usable count; networkRemaining
// is seeded lazily per placement from live IP capacity. Both are decremented as
// nodes are assigned (type-less selections) or reserved (sticky nodes). CAPI/Nico
// nodes never reach here: Nico owns its own instance-type availability.
type controlPlaneCapacityAccount struct {
	scheduler        *NodeScheduler
	logger           *slog.Logger
	forgeRemaining   map[uuid.UUID]int
	networkRemaining map[string]int
}

func newControlPlaneCapacityAccount(
	scheduler *NodeScheduler,
	_ db.Cluster,
	_ []db.Node,
	logger *slog.Logger,
) *controlPlaneCapacityAccount {
	return &controlPlaneCapacityAccount{
		scheduler:        scheduler,
		logger:           logger,
		forgeRemaining:   make(map[uuid.UUID]int),
		networkRemaining: make(map[string]int),
	}
}

// remaining returns the currently-available Forge and network capacity for a
// candidate (instanceType, placement), seeding either counter lazily from live
// Forge data on first access.
func (acct *controlPlaneCapacityAccount) remaining(
	ctx context.Context,
	cluster db.Cluster,
	instanceTypeID uuid.UUID,
	placement ForgePlacementSpec,
) (int, int, error) {
	placementKey := forgeNetworkPlacementKey(placement)
	_, forgeSeeded := acct.forgeRemaining[instanceTypeID]
	_, networkSeeded := acct.networkRemaining[placementKey]

	if !forgeSeeded || !networkSeeded {
		forgeInstanceType, networkCapacity, err := acct.scheduler.getForgeResources(ctx, cluster, ForgePlacementSpec{
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

// reserve debits one instance and one IP from the in-pass ledgers for
// (instanceType, placement), seeding each from live capacity on first use.
// Both the selector and sticky pending nodes call it so one pass can't overcommit.
func (acct *controlPlaneCapacityAccount) reserve(
	ctx context.Context,
	cluster db.Cluster,
	instanceTypeID uuid.UUID,
	placement ForgePlacementSpec,
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

// forgeNetworkPlacementKey keys network capacity by placement only (network/IP
// capacity is independent of the instance type). IDs are sorted so the same
// subnet/prefix set yields one key regardless of input order (otherwise two
// nodes with the same set in different order would get separate counters and
// double-spend the same IP capacity).
func forgeNetworkPlacementKey(placement ForgePlacementSpec) string {
	return fmt.Sprintf("%v|%v", sortedUUIDStrings(placement.SubnetIDs), sortedUUIDStrings(placement.VpcPrefixIDs))
}

// sortedUUIDStrings returns the UUIDs as a sorted string slice, for
// order-independent keying.
func sortedUUIDStrings(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	slices.Sort(out)
	return out
}
