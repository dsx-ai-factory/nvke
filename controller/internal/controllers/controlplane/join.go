// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"fmt"
	"sort"
	"time"

	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"

	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/otelx"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/workflows"
)

const (
	// controlPlaneJoinMaxAttempts is how many times a single control-plane node's
	// JoinNode workflow retries before it closes failed. When a node exhausts its
	// budget it is rotated to the back so other ready nodes get a turn; it is
	// retried again (with a fresh budget) once no un-attempted node remains.
	controlPlaneJoinMaxAttempts = 5
	// controlPlaneJoinRetryInitialInterval and controlPlaneJoinRetryMaxInterval
	// bound the backoff between those retries.
	controlPlaneJoinRetryInitialInterval = 30 * time.Second
	controlPlaneJoinRetryMaxInterval     = 5 * time.Minute
)

// joinStatus is the state of a control-plane node's JoinNode workflow, as far as
// serial-join reconciliation cares.
type joinStatus int

const (
	// joinStatusNotStarted means no JoinNode workflow exists for the node yet.
	joinStatusNotStarted joinStatus = iota
	// joinStatusJoining means the JoinNode workflow is open and the node is not
	// yet Joined: the kubeadm join (which changes etcd membership) is still in
	// flight. This is the only state that holds the serial join slot.
	joinStatusJoining
	// joinStatusPostJoin means the JoinNode workflow is still open but the node is
	// already Joined: only node-local post-join steps remain, which are safe to
	// overlap with another node's join, so this does not hold the serial slot.
	joinStatusPostJoin
	// joinStatusSettling means JoinNode completed successfully but the node is
	// not yet marked Provisioned; the provisioned condition is still catching up.
	joinStatusSettling
	// joinStatusExhausted means the JoinNode workflow closed unsuccessfully after
	// using up its retry budget. The node is retried later, behind fresh nodes.
	joinStatusExhausted
)

// joinCandidate pairs a control-plane node ready to join with the state of its
// JoinNode workflow.
type joinCandidate struct {
	node   db.Node
	status joinStatus
	// closeTime is when the JoinNode workflow last closed; only meaningful for
	// joinStatusExhausted, where it orders retries oldest-failure-first so
	// exhausted nodes rotate fairly.
	closeTime time.Time
}

// reconcileControlPlaneJoins starts JoinNode for at most one ReadyToJoin control-plane
// node per cluster (serial join). Host provisioning may run in parallel; only the
// join is serialized. A node that exhausts its retry budget is rotated to the back
// so it does not block others, and is retried once every other ready node has had
// a turn.
//
// It reports whether a join holds the serial slot: one was started here, or one is
// already in flight. The caller uses that to leave a queued delete alone until the
// control plane has finished growing, so a cluster that is both adding and removing
// a member does the addition first.
func (r *reconciler) reconcileControlPlaneJoins(ctx context.Context, cluster db.Cluster, cp *controlPlane) (bool, error) {
	// The control-plane controller only owns joins when it owns the control
	// plane; otherwise control-plane nodes auto-join inline in ProvisionNode.
	if !r.cfg.UseControlPlaneControllerForControlPlane() {
		return false, nil
	}
	if r.controllerTemporalClient == nil {
		return false, nil
	}
	if cluster.Conditions.Provisioned.Status != db.ConditionStatusTrue {
		r.logger.Debug("Skipping control-plane joins: cluster is not provisioned",
			"clusterID", cluster.ID,
			"clusterName", cluster.Name,
		)
		return false, nil
	}

	// A join and an etcd member removal both mutate etcd membership, and
	// overlapping them can move the cluster through a state where the removal's
	// quorum arithmetic no longer holds. Only a teardown actually in flight blocks
	// a join: a delete that is merely queued has not touched etcd yet, and the
	// reconcile order leaves it queued until the join finishes.
	if leaving, inFlight := cp.nodeTearingDown(); inFlight {
		r.logger.Info("Skipping control-plane joins: a control-plane node delete is in flight",
			"clusterID", cluster.ID,
			"clusterName", cluster.Name,
			"deletingNodeID", leaving.ID,
			"deletingNodeName", leaving.Name,
		)
		return false, nil
	}

	nodes := controlPlaneJoinCandidates(cp.nodes)
	if len(nodes) == 0 {
		return false, nil
	}

	// Resolve every candidate's join workflow state. Host provisioning runs in
	// parallel, so nodes can reach ReadyToJoin out of creation order; we must
	// look at all candidates, not only the oldest, or a join already running for
	// one node would not stop us from starting another.
	candidates := make([]joinCandidate, len(nodes))
	for i, node := range nodes {
		status, closeTime, err := r.joinWorkflowStatus(ctx, node)
		if err != nil {
			return false, err
		}
		candidates[i] = joinCandidate{node: node, status: status, closeTime: closeTime}
	}

	node, ok := selectControlPlaneJoin(candidates)
	if !ok {
		if blocker, found := firstJoiningNode(candidates); found {
			r.logger.Info("Control-plane join in flight; deferring other joins",
				"clusterID", cluster.ID,
				"nodeID", blocker.node.ID,
				"nodeName", blocker.node.Name,
			)
			return true, nil
		}
		return false, nil
	}

	workflowID := constants.JoinNodeWorkflowPrefix + node.ID.String()
	options := client.StartWorkflowOptions{
		ID:                       workflowID,
		TaskQueue:                r.cfg.TemporalTaskQueue,
		WorkflowIDConflictPolicy: enums.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
		WorkflowIDReusePolicy:    enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
		// The workflow-level retry budget: JoinNode bounds each join to a single
		// attempt, so this policy owns "retry X times". Re-executing an exhausted
		// node starts a fresh workflow with a fresh budget (reset each cycle).
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    controlPlaneJoinRetryInitialInterval,
			BackoffCoefficient: 2,
			MaximumInterval:    controlPlaneJoinRetryMaxInterval,
			MaximumAttempts:    controlPlaneJoinMaxAttempts,
		},
	}
	ctx, span := otelx.StartWorkflowSpan(ctx, otelx.WorkflowStart, otelx.WorkflowAttributes{
		WorkflowType: workflows.JoinNodeWorkflowType,
		WorkflowID:   workflowID,
		ClusterID:    cluster.ID,
		ClusterName:  cluster.Name,
		NodePoolID:   node.NodePoolID,
		NodeID:       node.ID,
		NodeName:     node.Name,
	})
	run, err := r.controllerTemporalClient.ExecuteWorkflow(ctx, options, workflows.JoinNode, workflows.JoinNodeInput{
		NodeID: node.ID,
	})
	otelx.EndWorkflowSpan(span, err)
	if err != nil {
		return false, fmt.Errorf("starting JoinNode workflow for node %s: %w", node.Name, err)
	}

	r.logger.InfoContext(ctx, "Started control-plane JoinNode workflow",
		"cluster_id", cluster.ID,
		"node_pool_id", node.NodePoolID,
		"node_id", node.ID,
		"node_name", node.Name,
		"workflow_id", workflowID,
		"run_id", run.GetRunID(),
	)
	return true, nil
}

// joinWorkflowStatus reports the state of a node's JoinNode workflow by
// describing it, along with the workflow's close time (used to rotate exhausted
// nodes). A missing workflow is joinStatusNotStarted. An open workflow is
// joinStatusJoining until the node is Joined, then joinStatusPostJoin — so the
// serial join slot is released as soon as kubeadm join finishes, letting the
// node-local post-join steps overlap the next node's join. A completed workflow
// whose node is not yet provisioned is joinStatusSettling; any other closed
// status is joinStatusExhausted.
func (r *reconciler) joinWorkflowStatus(ctx context.Context, node db.Node) (joinStatus, time.Time, error) {
	workflowID := constants.JoinNodeWorkflowPrefix + node.ID.String()
	desc, err := r.controllerTemporalClient.DescribeWorkflowExecution(ctx, workflowID, "")
	if err != nil {
		if isWorkflowNotFoundError(err) {
			return joinStatusNotStarted, time.Time{}, nil
		}
		return joinStatusNotStarted, time.Time{}, fmt.Errorf("describing join workflow for node %s: %w", node.ID, err)
	}

	info := desc.GetWorkflowExecutionInfo()
	if info.GetCloseTime() == nil {
		if node.Conditions.Joined.Status == db.ConditionStatusTrue {
			return joinStatusPostJoin, time.Time{}, nil
		}
		return joinStatusJoining, time.Time{}, nil
	}
	if info.GetStatus() == enums.WORKFLOW_EXECUTION_STATUS_COMPLETED {
		return joinStatusSettling, info.GetCloseTime().AsTime(), nil
	}
	return joinStatusExhausted, info.GetCloseTime().AsTime(), nil
}

// selectControlPlaneJoin decides which candidate's JoinNode to start next.
//
// It keeps the kubeadm join serial: if any candidate is joining, it returns
// ok=false so at most one join changes etcd membership at a time, regardless of
// the order hosts became ReadyToJoin. Post-join and settling nodes do not hold
// the slot — their remaining work is node-local and safe to overlap.
//
// Otherwise it schedules fairly: un-attempted (fresh) nodes go first, oldest
// node first; only when no fresh node remains does it retry an exhausted node,
// oldest-failure-first so exhausted nodes rotate rather than one of them
// starving the rest.
func selectControlPlaneJoin(candidates []joinCandidate) (db.Node, bool) {
	var fresh, exhausted []joinCandidate
	for _, c := range candidates {
		switch c.status {
		case joinStatusJoining:
			// A kubeadm join is in flight; keep joins serial.
			return db.Node{}, false
		case joinStatusPostJoin, joinStatusSettling:
			// Post-join work in flight; safe to overlap, so neither block nor
			// reselect this node.
		case joinStatusNotStarted:
			fresh = append(fresh, c)
		case joinStatusExhausted:
			exhausted = append(exhausted, c)
		}
	}

	if len(fresh) > 0 {
		sort.Slice(fresh, func(i, j int) bool {
			return fresh[i].node.CreatedAt.Before(fresh[j].node.CreatedAt)
		})
		return fresh[0].node, true
	}

	if len(exhausted) > 0 {
		sort.Slice(exhausted, func(i, j int) bool {
			return exhausted[i].closeTime.Before(exhausted[j].closeTime)
		})
		return exhausted[0].node, true
	}

	return db.Node{}, false
}

// firstJoiningNode returns the first candidate whose kubeadm join is in flight,
// for logging which node is holding the serial join slot.
func firstJoiningNode(candidates []joinCandidate) (joinCandidate, bool) {
	for _, c := range candidates {
		if c.status == joinStatusJoining {
			return c, true
		}
	}
	return joinCandidate{}, false
}

// controlPlaneJoinCandidates returns the control-plane nodes that are ready to
// join and not yet provisioned, skipping nodes marked for deletion.
func controlPlaneJoinCandidates(nodes []db.Node) []db.Node {
	candidates := make([]db.Node, 0, len(nodes))
	for _, node := range nodes {
		if isMarkedForDeletion(node) {
			continue
		}
		if node.Conditions.ReadyToJoin.Status != db.ConditionStatusTrue {
			continue
		}
		if isProvisioned(node) {
			continue
		}
		candidates = append(candidates, node)
	}
	return candidates
}
