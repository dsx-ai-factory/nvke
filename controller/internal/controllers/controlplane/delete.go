// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"fmt"
	"sort"

	enums "go.temporal.io/api/enums/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/sdk/client"

	"github.com/dsx-ai-factory/nke/controller/internal/constants"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/nodes"
	"github.com/dsx-ai-factory/nke/controller/internal/temporal/workflows"
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

	logger := r.logger.With(
		"clusterID", cluster.ID,
		"clusterName", cluster.Name,
		"requestedDeletes", len(requested),
	)

	// The in-flight check runs before the "nothing requested" return on purpose.
	// A teardown whose workflow closed without completing is not in flight, and
	// nothing else retries it: the outbox marks its entry processed as soon as
	// the workflow starts, so the node keeps Deleting for good. That one row then
	// defers every later control-plane delete, join, remediation and rollout, and
	// still counts toward the control plane's size, so no replacement is built
	// either. A wedged cluster has no requested deletes left, so returning early
	// would never look at it.
	if deleting, ok := cp.nodeTearingDown(); ok {
		restarted, err := r.restartFailedTeardown(ctx, deleting)
		if err != nil {
			return false, err
		}
		if restarted {
			return true, nil
		}
		if len(requested) == 0 {
			return false, nil
		}
		logger.Info("Deferring control-plane node delete: another teardown is already in flight",
			"inFlightNodeID", deleting.ID,
			"inFlightNodeName", deleting.Name,
		)
		return false, nil
	}

	if len(requested) == 0 {
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

// restartFailedTeardown starts a fresh TeardownNode run for a node whose last
// run closed without completing, and reports whether it started one.
//
// Same shape as joinWorkflowStatus above and reconcileK8sUpgrade: Temporal keeps
// returning the closed execution, so without an explicit restart the node stays
// Deleting forever. TeardownNode is idempotent step by step -- the cancels treat
// a missing workflow as success, the drain and the kube Node delete ignore a
// node that is already gone, the CAPI deletes ignore NotFound, and the agent's
// etcd member removal succeeds when the member is absent -- so a second run
// picks up wherever the first stopped.
func (r *reconciler) restartFailedTeardown(ctx context.Context, node db.Node) (bool, error) {
	if r.controllerTemporalClient == nil {
		return false, nil
	}

	workflowID := constants.TeardownNodeWorkflowPrefix + node.ID.String()
	desc, err := r.controllerTemporalClient.DescribeWorkflowExecution(ctx, workflowID, "")
	if err != nil {
		if isWorkflowNotFoundError(err) {
			// The outbox has not delivered the start yet. Leave it alone.
			return false, nil
		}
		return false, fmt.Errorf("describing teardown workflow for node %s: %w", node.ID, err)
	}
	if !teardownClosedUnsuccessfully(desc.GetWorkflowExecutionInfo()) {
		return false, nil
	}

	input, err := nodes.RestartTeardownInput(node)
	if err != nil {
		return false, err
	}

	run, err := r.controllerTemporalClient.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                       workflowID,
		TaskQueue:                r.cfg.TemporalTaskQueue,
		WorkflowIDConflictPolicy: enums.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
		WorkflowIDReusePolicy:    enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
	}, workflows.TeardownNode, input)
	if err != nil {
		return false, fmt.Errorf("restarting teardown workflow for node %s (%s): %w", node.ID, node.Name, err)
	}

	r.logger.Warn("Restarted a control-plane node teardown whose workflow closed without completing",
		"nodeID", node.ID,
		"nodeName", node.Name,
		"workflowID", run.GetID(),
		"runID", run.GetRunID(),
	)
	return true, nil
}

// teardownClosedUnsuccessfully reports whether a described TeardownNode run has
// finished and did not complete, so nothing will finish that teardown unless a
// new run starts.
func teardownClosedUnsuccessfully(info *workflowpb.WorkflowExecutionInfo) bool {
	return info.GetCloseTime() != nil && info.GetStatus() != enums.WORKFLOW_EXECUTION_STATUS_COMPLETED
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
