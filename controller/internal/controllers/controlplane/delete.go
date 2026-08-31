// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"fmt"
	"sort"

	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/nodes"
)

// reconcileRequestedDeletes turns accepted control-plane node deletes into
// teardowns, one per cluster at a time.
//
// DELETE /nodes/{id} does not tear a control-plane node down inline under this
// flag: it records the request and returns 202, and this loop owns when the
// teardown actually starts. Serializing here is what keeps two etcd member
// removals from overlapping, which no teardown-time dry-run can prevent once
// both workflows are already running.
//
// It reports whether a teardown was started, so the caller can let the resulting
// node write re-enqueue the cluster instead of acting on a stale snapshot.
func (r *reconciler) reconcileRequestedDeletes(
	ctx context.Context,
	cluster db.Cluster,
	cp *controlPlane,
) (bool, error) {
	if !r.cfg.UseControlPlaneControllerForControlPlane() {
		return false, nil
	}

	requested := cp.nodesWithDeleteRequested()
	if len(requested) == 0 {
		return false, nil
	}

	logger := r.logger.With(
		"clusterID", cluster.ID,
		"clusterName", cluster.Name,
		"requestedDeletes", len(requested),
	)

	if deleting, ok := cp.nodeTearingDown(); ok {
		logger.Info("Deferring control-plane node delete: another teardown is already in flight",
			"inFlightNodeID", deleting.ID,
			"inFlightNodeName", deleting.Name,
		)
		return false, nil
	}

	next := selectNextControlPlaneNodeToDelete(requested)
	nodeLogger := logger.With("nodeID", next.ID, "nodeName", next.Name)

	if err := nodes.StartRequestedTeardown(ctx, r.db, next.ID); err != nil {
		return false, fmt.Errorf("starting teardown for requested control-plane node delete %s (%s): %w", next.ID, next.Name, err)
	}

	nodeLogger.Info("Started teardown for a requested control-plane node delete")
	return true, nil
}

// selectNextControlPlaneNodeToDelete picks the request that has been waiting
// longest, so a queue of accepted deletes drains in the order it was accepted
// and no request starves behind a newer one. Ties break on node ID to keep the
// choice stable across reconciles.
func selectNextControlPlaneNodeToDelete(requested []db.Node) db.Node {
	sorted := make([]db.Node, len(requested))
	copy(sorted, requested)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i].Conditions.DeleteRequested, sorted[j].Conditions.DeleteRequested
		if !a.LastTransitionTime.Equal(b.LastTransitionTime) {
			return a.LastTransitionTime.Before(b.LastTransitionTime)
		}
		return sorted[i].ID.String() < sorted[j].ID.String()
	})
	return sorted[0]
}
