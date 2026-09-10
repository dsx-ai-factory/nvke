// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/metrics"
	"github.com/NVIDIA/nke/controller/internal/temporal/activities"
)

type HookType string

const (
	BeforeJoinNode   HookType = "before-join"
	AfterJoinNode    HookType = "after-join"
	BeforeRemoveNode HookType = "before-remove"
	AfterRemoveNode  HookType = "after-remove"
)

type ProvisionNodeInput struct {
	NodeID                         uuid.UUID
	RebootNodeWaitForUptimeTimeout time.Duration
	// SkipJoin leaves the host at ReadyToJoin without running kubeadm join.
	// Control-plane nodes set this true so the control-plane controller can
	// start JoinNode one node at a time. Workers leave it false (auto-join).
	// The zero value must be false so in-flight ProvisionNode workflows keep
	// joining after a worker upgrade.
	SkipJoin bool
}

func ProvisionNode(ctx workflow.Context, input ProvisionNodeInput) error {
	logger := workflow.GetLogger(ctx)

	if input.NodeID == uuid.Nil {
		return errors.New("nodeID is required")
	}

	workflowStart := workflow.Now(ctx)

	labels, labelsErr := getNodeMetricLabels(ctx, input.NodeID)
	if labelsErr != nil {
		logger.Warn("failed to get node metric labels, metrics will use unknown", "error", labelsErr)
		labels = nodeMetricLabels{ClusterName: unknownValue, NodeType: unknownValue}
	}

	nodeCtx, err := getNodeProvisioningContext(ctx, input.NodeID)
	if err != nil {
		return fmt.Errorf("failed to get node provisioning context: %w", err)
	}

	recordPhase := func(phase string, start, end time.Time) {
		if !workflow.IsReplaying(ctx) {
			metrics.RecordProvisionPhase(labels.ClusterName, labels.NodeType, phase, end.Sub(start))
		}
	}

	infraProvisionStart := workflow.Now(ctx)
	if err := provisionCAPINode(ctx, nodeCtx.NodeID); err != nil {
		if err := updateNodeProvisioningMessage(ctx, input.NodeID, fmt.Sprintf("Workflow failed: %s", err)); err != nil {
			logger.Error("failed to update node conditions", "error", err)
		}
		recordPhase("capi_create", infraProvisionStart, workflow.Now(ctx))

		if !workflow.IsReplaying(ctx) {
			metrics.RecordProvisionFailed(labels.ClusterName, labels.NodeType, workflow.Now(ctx).Sub(workflowStart))
		}
		return fmt.Errorf("failed to provision CAPI node: %w", err)
	}
	recordPhase("capi_create", infraProvisionStart, workflow.Now(ctx))

	if err := provisionNode(ctx, input, labels); err != nil {
		// Best effort to update the node conditions to indicate the provisioning workflow failed
		if err := updateNodeProvisioningMessage(ctx, input.NodeID, fmt.Sprintf("Workflow failed: %s", err)); err != nil {
			logger.Error("failed to update node conditions", "error", err)
		}

		if !workflow.IsReplaying(ctx) {
			metrics.RecordProvisionFailed(labels.ClusterName, labels.NodeType, workflow.Now(ctx).Sub(workflowStart))
		}
		return fmt.Errorf("failed to provision node: %w", err)
	}

	if input.SkipJoin {
		logger.Info("Node host provisioned; join deferred to control-plane controller",
			"nodeID", input.NodeID,
			"clusterName", labels.ClusterName,
			"nodeType", labels.NodeType,
		)
		return nil
	}

	// Workers auto-join inline and retry the join indefinitely (boundJoinAttempt
	// false); only the control-plane JoinNode workflow bounds the attempt.
	if err := joinProvisionedNode(ctx, input.NodeID, nodeCtx, labels, false); err != nil {
		// Best effort to update the node conditions to indicate the join failed
		if err := updateNodeProvisioningMessage(ctx, input.NodeID, fmt.Sprintf("Workflow failed: %s", err)); err != nil {
			logger.Error("failed to update node conditions", "error", err)
		}

		if !workflow.IsReplaying(ctx) {
			metrics.RecordProvisionFailed(labels.ClusterName, labels.NodeType, workflow.Now(ctx).Sub(workflowStart))
		}
		return fmt.Errorf("failed to join node: %w", err)
	}

	totalDuration := workflow.Now(ctx).Sub(workflowStart)
	if !workflow.IsReplaying(ctx) {
		metrics.RecordProvisionCompleted(labels.ClusterName, labels.NodeType, totalDuration)
	}

	logger.Info("Node provisioning complete",
		"nodeID", input.NodeID,
		"duration", totalDuration.Seconds(),
		"clusterName", labels.ClusterName,
		"nodeType", labels.NodeType,
	)

	return nil
}

func provisionCAPINode(ctx workflow.Context, nodeID uuid.UUID) error {
	var a *activities.Activities
	activityOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 2 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 10 * time.Second,
			MaximumInterval: 1 * time.Minute,
			MaximumAttempts: 0,
		},
	}

	if err := workflow.ExecuteActivity(withDBOptions(ctx), a.UpdateNodeConditions, activities.NodeConditionsUpdate{
		NodeID: nodeID,
		Conditions: activities.NodeConditions{
			Pending: &activities.ConditionUpdate{
				Status:  db.ConditionStatusTrue,
				Message: "Waiting for infrastructure allocation",
			},
			Provisioning: &activities.ConditionUpdate{
				Status:  db.ConditionStatusFalse,
				Message: "",
			},
		},
	}).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to mark node as waiting for infrastructure allocation: %w", err)
	}

	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, activityOptions),
		a.CreateCAPIInfraMachineForNode,
		activities.CreateCAPIInfraMachineForNodeInput{NodeID: nodeID},
	).Get(ctx, nil); err != nil {
		return err
	}

	return nil
}

func getNodeProvisioningContext(ctx workflow.Context, nodeID uuid.UUID) (activities.NodeProvisioningContext, error) {
	var a *activities.Activities

	nodeCtx := activities.NodeProvisioningContext{}
	if err := workflow.ExecuteActivity(withDBOptions(ctx), a.GetNodeProvisioningContext, nodeID).Get(ctx, &nodeCtx); err != nil {
		return activities.NodeProvisioningContext{}, fmt.Errorf(
			"failed to get node provisioning context from database: %s: %w",
			nodeID,
			err,
		)
	}

	return nodeCtx, nil
}

func provisionNode(ctx workflow.Context, input ProvisionNodeInput, labels nodeMetricLabels) error {
	recordPhase := func(phase string, start, end time.Time) {
		if !workflow.IsReplaying(ctx) {
			metrics.RecordProvisionPhase(labels.ClusterName, labels.NodeType, phase, end.Sub(start))
		}
	}

	capiReadyStart := workflow.Now(ctx)
	if err := waitForCAPIMachine(ctx, input.NodeID); err != nil {
		recordPhase("capi_ready", capiReadyStart, workflow.Now(ctx))
		return fmt.Errorf("failed to wait for CAPI machine to be ready: %w", err)
	}
	recordPhase("capi_ready", capiReadyStart, workflow.Now(ctx))

	hostPrepStart := workflow.Now(ctx)
	endHostPrep := func() { recordPhase("host_preparation", hostPrepStart, workflow.Now(ctx)) }

	if err := waitForNodePreflight(ctx, input.NodeID); err != nil {
		endHostPrep()
		return fmt.Errorf("failed to wait for node preflight to succeed: %w", err)
	}

	configProfile, err := getConfigProfile(ctx, input.NodeID)
	if err != nil {
		endHostPrep()
		return fmt.Errorf("failed to get config profile: %w", err)
	}

	if err := runHooks(ctx, input.NodeID, BeforeJoinNode, configProfile); err != nil {
		endHostPrep()
		return fmt.Errorf("failed to run beforeJoinNode hooks: %w", err)
	}

	if err := configureCNIPrerequisites(ctx, input.NodeID); err != nil {
		endHostPrep()
		return fmt.Errorf("failed to configure CNI prerequisites: %w", err)
	}
	endHostPrep()

	if configProfile.PreJoinNodeReboot {
		rebootStart := workflow.Now(ctx)
		if err := rebootNodeWaitForUptimeChildWorkflow(ctx, input.NodeID, input.RebootNodeWaitForUptimeTimeout); err != nil {
			recordPhase("reboot", rebootStart, workflow.Now(ctx))
			return fmt.Errorf("failed to reboot node: %w", err)
		}
		recordPhase("reboot", rebootStart, workflow.Now(ctx))
	}

	if err := markNodeAsReadyToJoin(ctx, input.NodeID); err != nil {
		return fmt.Errorf("failed to mark node as ready to join: %w", err)
	}

	return nil
}

// joinProvisionedNode runs kubeadm join and the post-join steps for a host that
// has already completed host provisioning and is marked ReadyToJoin. It is
// called inline by ProvisionNode for auto-join nodes (workers) and by the
// JoinNode workflow for control-plane nodes the control-plane controller joins
// one at a time.
func joinProvisionedNode(ctx workflow.Context, nodeID uuid.UUID, nodeCtx activities.NodeProvisioningContext, labels nodeMetricLabels, boundJoinAttempt bool) error {
	recordPhase := func(phase string, start, end time.Time) {
		if !workflow.IsReplaying(ctx) {
			metrics.RecordProvisionPhase(labels.ClusterName, labels.NodeType, phase, end.Sub(start))
		}
	}

	hasConfigProfile := nodeCtx.ConfigProfile != ""
	configProfile, err := getConfigProfile(ctx, nodeID)
	if err != nil {
		return fmt.Errorf("failed to get config profile: %w", err)
	}

	if nodeCtx.ManagedByNodePool {
		if err := waitForNodePoolProvisioning(ctx, nodeID); err != nil {
			return fmt.Errorf("failed to wait for node pool provisioning: %w", err)
		}
	}

	// Skip kubeadm join if it already succeeded in a prior attempt. The join
	// changes etcd membership and is serialized across control-plane nodes by the
	// control-plane controller, which releases the serial slot once the node is
	// Joined so the next node can join while this one finishes post-join. A retry
	// (e.g. a failed post-join step) must therefore never re-run the join, or it
	// could overlap another node's join. A joined node is never the initial node.
	alreadyJoined, err := isNodeJoined(ctx, nodeID)
	if err != nil {
		return fmt.Errorf("failed to check node joined status: %w", err)
	}

	joinStart := workflow.Now(ctx)
	isInitialNode := false
	if alreadyJoined {
		workflow.GetLogger(ctx).Info("Node already joined; skipping kubeadm join and resuming post-join", "nodeID", nodeID)
	} else {
		// A bounded join is the serialized control-plane path. kubeadm join is not
		// idempotent, so clean up any leftovers from a prior failed attempt before
		// re-joining (no-op on a freshly provisioned node).
		if boundJoinAttempt {
			cleanBeforeControlPlaneRejoin(ctx, nodeID)
		}
		isInitialNode, err = joinNode(ctx, nodeID, boundJoinAttempt)
		if err != nil {
			recordPhase("join", joinStart, workflow.Now(ctx))
			return fmt.Errorf("failed to join node: %w", err)
		}
	}
	recordPhase("join", joinStart, workflow.Now(ctx))

	postJoinStart := workflow.Now(ctx)
	endPostJoin := func() { recordPhase("post_join", postJoinStart, workflow.Now(ctx)) }

	if err := labelNode(ctx, nodeID, hasConfigProfile); err != nil {
		endPostJoin()
		return fmt.Errorf("failed to label node: %w", err)
	}

	if err := runHooks(ctx, nodeID, AfterJoinNode, configProfile); err != nil {
		endPostJoin()
		return fmt.Errorf("failed to run afterJoinNode hooks: %w", err)
	}

	// If the node is the initial control plane node, we don't need to configure KubeVip, because the cluster creation process does it.
	if !isInitialNode {
		// This is really only needed for control plane nodes -- will be a no-op if it's a worker node
		if err := configureKubeVip(ctx, nodeID); err != nil {
			endPostJoin()
			return fmt.Errorf("failed to configure KubeVip: %w", err)
		}
	}

	// Remove taint (nke.nvidia.com/provisioning=true:NoSchedule-)
	// This taint is added via kubeadm join config.
	// Hyphen at the end of the taint effect removes the taint
	taintToRemove := constants.NodeTaintProvisioning()
	if err := taintNode(ctx, nodeID, taintToRemove.Key, taintToRemove.Value, taintToRemove.Effect+"-"); err != nil {
		endPostJoin()
		return fmt.Errorf("failed to untaint node: %w", err)
	}

	if err := markNodeAsProvisioned(ctx, nodeID); err != nil {
		endPostJoin()
		return fmt.Errorf("failed to mark node as provisioned: %w", err)
	}
	endPostJoin()

	return nil
}

func waitForCAPIMachine(ctx workflow.Context, nodeID uuid.UUID) error {
	var a *activities.Activities

	activityOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    1 * time.Second,
			MaximumInterval:    10 * time.Second,
			BackoffCoefficient: 2,
			MaximumAttempts:    0, // Retry indefinitely
		},
	}

	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, activityOptions),
		a.WaitForCAPIMachineReady,
		activities.WaitForCAPIMachineReadyInput{
			NodeID:                           nodeID,
			NodeProvisioningConditionMessage: "Waiting for CAPI machine to be ready",
		},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to wait for CAPI machine to be ready: %w", err)
	}

	return nil
}

func waitForNodePreflight(ctx workflow.Context, nodeID uuid.UUID) error {
	var a *activities.Activities

	waitOptions := workflow.ActivityOptions{
		// The preflight runs agent readiness checks and optional file/cloud-init checks.
		// 7 minutes is sufficient for a single attempt.
		StartToCloseTimeout: 7 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 30 * time.Second,
			MaximumInterval: 2 * time.Minute,
			MaximumAttempts: 0, // Retry indefinitely
		},
	}

	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, waitOptions),
		a.NodePreflight,
		activities.NodePreflightInput{
			NodeID:                           nodeID,
			NodeProvisioningConditionMessage: "Waiting for node preflight to succeed",
		},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to wait for node preflight to succeed: %w", err)
	}

	return nil
}

func rebootNodeWaitForUptimeChildWorkflow(ctx workflow.Context, nodeID uuid.UUID, timeout time.Duration) error {
	workflowOptions := workflow.ChildWorkflowOptions{
		WorkflowID: fmt.Sprintf("%s%s", constants.RebootNodeUptimeWorkflowPrefix, nodeID),
		// child workflow options cannot specify WorkflowIDConflictPolicy.
		// Hence, to prevent duplicates we use ALLOW_DUPLICATE_FAILED_ONLY to avoid multiple reboot workflows on the same node
		WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 1 * time.Minute,
			MaximumInterval: 5 * time.Minute,
			MaximumAttempts: 0, // retry indefinitely
		},
	}

	rebootNodeWaitForUptimeInput := RebootNodeWaitForUptimeInput{
		NodeID:  nodeID,
		Timeout: timeout,
	}

	err := workflow.ExecuteChildWorkflow(
		workflow.WithChildOptions(ctx, workflowOptions),
		RebootNodeWaitForUptime,
		&rebootNodeWaitForUptimeInput,
	).Get(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to execute reboot node child workflow: %w", err)
	}
	return nil
}

func getConfigProfile(ctx workflow.Context, nodeID uuid.UUID) (activities.ConfigProfile, error) {
	var a *activities.Activities

	var configProfile activities.ConfigProfile
	if err := workflow.ExecuteActivity(withDBOptions(ctx), a.GetNodeConfigProfile, activities.GetNodeConfigProfileInput{
		NodeID: nodeID,
	}).Get(ctx, &configProfile); err != nil {
		return activities.ConfigProfile{}, fmt.Errorf("failed to get config profile: %w", err)
	}

	return configProfile, nil
}

func runHooks(ctx workflow.Context, nodeID uuid.UUID, hookType HookType, configProfile activities.ConfigProfile) error {
	var a *activities.Activities

	var hooks []*activities.HookSpec
	switch hookType {
	case BeforeJoinNode:
		hooks = configProfile.Hooks.BeforeJoinNode
	case AfterJoinNode:
		hooks = configProfile.Hooks.AfterJoinNode
	case BeforeRemoveNode:
		hooks = configProfile.Hooks.BeforeRemoveNode
	case AfterRemoveNode:
		hooks = configProfile.Hooks.AfterRemoveNode
	default:
		return fmt.Errorf("invalid hook type: %s", hookType)
	}

	execRunbookActivityOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 35 * time.Minute,
		HeartbeatTimeout:    defaultRunbookActivityHeartbeatTimeout(),
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 1 * time.Minute,
			MaximumInterval: 5 * time.Minute,
			MaximumAttempts: 0, // Retry indefinitely
		},
	}

	for _, hook := range hooks {
		if err := workflow.ExecuteActivity(
			workflow.WithActivityOptions(ctx, execRunbookActivityOptions),
			a.ExecuteNodeRunbookHook,
			activities.ExecuteNodeRunbookHookInput{
				NodeID:      nodeID,
				RunbookName: hook.Runbook.Name,
				RunbookEnv:  hook.Runbook.Env,

				NodeProvisioningConditionMessage: fmt.Sprintf("Executing %s runbook %s", string(hookType), hook.Runbook.Name),
			},
		).Get(ctx, nil); err != nil {
			return fmt.Errorf("failed to run %s runbook %s: %w", hookType, hook.Runbook.Name, err)
		}
	}

	return nil
}

func configureCNIPrerequisites(ctx workflow.Context, nodeID uuid.UUID) error {
	var a *activities.Activities

	var cluster db.Cluster
	if err := workflow.ExecuteActivity(withDBOptions(ctx), a.GetClusterForNode, nodeID).Get(ctx, &cluster); err != nil {
		return fmt.Errorf("failed to get cluster using nodeID: %w", err)
	}

	if cluster.CniPlugin == db.CNIOvnKubernetes {
		activityOptions := workflow.ActivityOptions{
			StartToCloseTimeout: 1 * time.Minute,
			RetryPolicy: &temporal.RetryPolicy{
				InitialInterval: 1 * time.Minute,
				MaximumInterval: 5 * time.Minute,
				MaximumAttempts: 0, // Retry indefinitely
			},
		}
		if err := workflow.ExecuteActivity(
			workflow.WithActivityOptions(ctx, activityOptions),
			a.ExecuteHostRunbook,
			activities.ExecuteHostRunbookInput{
				HostID:      nodeID,
				RunbookName: "configure-openvswitch",
			},
		).Get(ctx, nil); err != nil {
			return fmt.Errorf("failed to run configure-openvswitch runbook: %w", err)
		}
	}

	return nil
}

func waitForNodePoolProvisioning(ctx workflow.Context, nodeID uuid.UUID) error {
	var a *activities.Activities

	waitOptions := workflow.ActivityOptions{
		// Takes longest on initial cluster provisioning while waiting for the cluster to be ready.
		// Should be less than a minute if adding a new node pool to an existing cluster
		StartToCloseTimeout: 30 * time.Minute,
		HeartbeatTimeout:    2 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 30 * time.Second,
			MaximumInterval: 5 * time.Minute,
			MaximumAttempts: 0, // Retry indefinitely, giving operator time to manually resolve any issues
		},
	}

	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, waitOptions),
		a.WaitForParentNodePoolProvisioned,
		activities.WaitForParentNodePoolProvisionedInput{
			NodeID:                           nodeID,
			NodeProvisioningConditionMessage: "Waiting for parent node pool to be provisioned",
		},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to wait for node's parent node pool to be provisioned: %w", err)
	}

	return nil
}

// nodeProvisioningStart returns the node's provisioning-start time: the
// Provisioning condition's transition time, falling back to CreatedAt. It anchors
// JoinNode's end-to-end provision-completed metric.
func nodeProvisioningStart(ctx workflow.Context, nodeID uuid.UUID) (time.Time, error) {
	var a *activities.Activities

	var node db.Node
	if err := workflow.ExecuteActivity(withDBOptions(ctx), a.GetNode, nodeID).Get(ctx, &node); err != nil {
		return time.Time{}, fmt.Errorf("failed to get node: %w", err)
	}

	provisioningStartedAt := node.Conditions.Provisioning.LastTransitionTime
	if provisioningStartedAt.IsZero() {
		provisioningStartedAt = node.CreatedAt
	}
	return provisioningStartedAt, nil
}

// isNodeJoined reports whether the node's Joined condition is already true, i.e.
// kubeadm join completed on a prior attempt.
func isNodeJoined(ctx workflow.Context, nodeID uuid.UUID) (bool, error) {
	var a *activities.Activities

	var node db.Node
	if err := workflow.ExecuteActivity(withDBOptions(ctx), a.GetNode, nodeID).Get(ctx, &node); err != nil {
		return false, fmt.Errorf("failed to get node: %w", err)
	}
	return node.Conditions.Joined.Status == db.ConditionStatusTrue, nil
}

// joinNode runs the kubeadm join activity. When boundJoinAttempt is true the
// activity is limited to a single attempt so a failed join surfaces to the
// caller and the workflow can fail; the control-plane JoinNode workflow uses
// this so its workflow-level RetryPolicy owns the retry budget (retry X times,
// then the control-plane controller rotates to the next node). Workers pass
// false and keep retrying the join indefinitely inline.
func joinNode(ctx workflow.Context, nodeID uuid.UUID, boundJoinAttempt bool) (bool, error) {
	var a *activities.Activities

	joinMaximumAttempts := int32(0) // Retry indefinitely.
	if boundJoinAttempt {
		joinMaximumAttempts = 1
	}

	activityOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 1 * time.Minute,
			MaximumInterval: 5 * time.Minute,
			MaximumAttempts: joinMaximumAttempts,
		},
	}

	var result activities.KubeadmJoinResult
	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, activityOptions),
		a.KubeadmJoin,
		activities.KubeadmJoinInput{
			NodeID:                           nodeID,
			NodeProvisioningConditionMessage: "Joining node",
		},
	).Get(ctx, &result); err != nil {
		return false, fmt.Errorf("failed to join node: %w", err)
	}

	if result.IsInitialNode {
		return true, nil
	}

	if err := workflow.ExecuteActivity(withDBOptions(ctx), a.UpdateNodeConditions, activities.NodeConditionsUpdate{
		NodeID: nodeID,
		Conditions: activities.NodeConditions{
			Joined: &activities.ConditionUpdate{
				Status:  db.ConditionStatusTrue,
				Message: "Node joined",
			},
		},
	}).Get(ctx, nil); err != nil {
		return false, fmt.Errorf("failed to update node conditions: %w", err)
	}

	return false, nil
}

// cleanBeforeControlPlaneRejoin makes a serialized control-plane join repeatable.
// kubeadm join is not idempotent: a prior attempt can leave local kubeadm state
// (certs, manifests, kubelet config) that trips preflight on every retry, and --
// if it failed after `etcdctl member add` -- a stale voting member that a
// re-join cannot re-add. This runs before each bounded control-plane join and is
// a no-op on a freshly provisioned node: kubeadm reset on a clean node does
// nothing, and etcd removal reports success when the name is not a member.
//
// It is unconditional rather than gated on a retry count because the
// NKE controllers start a fresh JoinNode workflow (attempt resets to
// 1) once a node exhausts its retry budget, so the workflow attempt number is
// not a reliable "is this a retry" signal -- only node/cluster state is, and
// unconditional cleanup sidesteps having to read it. Both steps are best-effort
// so a transient agent failure never fails an otherwise-healthy first
// join; a genuinely dirty node is simply cleaned again on the next attempt.
//
// FIXME: kubeadm reset belongs in the node-agent, run before every kubeadm join,
// and is needed for workers too: a worker that partially joins then fails loops
// on preflight leftovers, because the agent re-runs join without a reset and the
// worker retry lives in the KubeadmJoin activity loop rather than here. This
// CWE-side, control-plane-only cleanup is a fallback until the agent does
// reset-before-join for all nodes.
func cleanBeforeControlPlaneRejoin(ctx workflow.Context, nodeID uuid.UUID) {
	logger := workflow.GetLogger(ctx)
	var a *activities.Activities

	var node db.Node
	if err := workflow.ExecuteActivity(withDBOptions(ctx), a.GetNode, nodeID).Get(ctx, &node); err != nil {
		logger.Warn("Skipping pre-join cleanup; failed to get node", "nodeID", nodeID, "error", err)
		return
	}
	if !node.ControlPlane {
		return
	}

	// Run only the typed etcd-removal activity (not the teardown flow) on a
	// surviving control-plane node, with join-scoped activity options so this path
	// does not inherit teardown's retry/timeout preset or any future
	// teardown-specific behavior. The activity retains the Unimplemented fallback
	// for old agents and keeps its behavior aligned with the typed RPC: dropping a
	// 2-voter cluster back to the lone survivor before re-adding this node is
	// expected during control-plane scale-up, while quorum loss still fails.
	etcdCleanupOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
		HeartbeatTimeout:    defaultRunbookActivityHeartbeatTimeout(),
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 30 * time.Second,
			MaximumInterval: 1 * time.Minute,
			MaximumAttempts: 3,
		},
	}
	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, etcdCleanupOptions),
		a.RemoveEtcdMember,
		activities.EtcdMemberRemovalInput{
			ClusterID:        node.ClusterID,
			TargetNodeID:     nodeID,
			TargetMemberName: node.Name,
		},
	).Get(ctx, nil); err != nil {
		logger.Warn("Best-effort etcd member cleanup before rejoin failed; continuing",
			"nodeID", nodeID, "nodeName", node.Name, "error", err)
	}

	// Reset local kubeadm state so the join does not trip preflight on leftovers.
	// Call the activity directly rather than the teardown helper: an empty
	// condition message tells the activity to skip the condition update, so a
	// joining node's Deleting condition is never touched (the teardown helper
	// writes it). Skip the etcd phase -- CWE removed the member above.
	resetOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 7 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 30 * time.Second,
			MaximumInterval: 30 * time.Second,
			MaximumAttempts: 3,
		},
	}
	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, resetOptions),
		a.KubeadmReset,
		activities.KubeadmResetInput{
			NodeID:     nodeID,
			SkipPhases: []string{removeEtcdMember},
		},
	).Get(ctx, nil); err != nil {
		logger.Warn("Best-effort kubeadm reset before rejoin failed; continuing",
			"nodeID", nodeID, "nodeName", node.Name, "error", err)
	}
}

func labelNode(ctx workflow.Context, nodeID uuid.UUID, addConfigProfileLabels bool) error {
	var a *activities.Activities

	activityOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 1 * time.Minute, // Should be quick
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 1 * time.Minute,
			MaximumInterval: 5 * time.Minute,
			MaximumAttempts: 0, // Retry indefinitely
		},
	}

	labels := []string{}

	var cluster db.Cluster
	if err := workflow.ExecuteActivity(withDBOptions(ctx), a.GetClusterForNode, nodeID).Get(ctx, &cluster); err != nil {
		return fmt.Errorf("failed to get cluster using nodeID: %w", err)
	}

	var node db.Node
	if err := workflow.ExecuteActivity(withDBOptions(ctx), a.GetNode, nodeID).Get(ctx, &node); err != nil {
		return fmt.Errorf("failed to get node using nodeID: %w", err)
	}

	// Label the node with its Forge/Carbide machine ID so the machine identity
	// is discoverable directly on the K8s node object. Skipped when the node has
	// no machine ID (e.g. non-Forge / CAPD-only nodes).
	if cluster.ProviderType == db.ProviderTypeNico {
		if _, err := cluster.NicoProvider(); err != nil {
			return err
		}
		nicoNodeProvider, err := node.NicoProvider()
		if err != nil {
			return err
		}
		if nicoNodeProvider.ForgeMachineID != "" {
			labels = append(labels, constants.LabelKeyMachineID+"="+nicoNodeProvider.ForgeMachineID)
		}
	}

	// If the cluster is using ovn cni, add zone-name label 'k8s.ovn.org/zone-name=<node-name>'
	// For current ovn version, this is needed to schedule ovn pods on the node
	// In previous ovn cluster builds, this label was applied manually
	if cluster.CniPlugin == db.CNIOvnKubernetes {
		labels = append(labels, "k8s.ovn.org/zone-name="+node.Name)
	}

	if !addConfigProfileLabels && len(labels) == 0 {
		return nil
	}

	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, activityOptions),
		a.LabelNode,
		activities.LabelNodeInput{
			AddConfigProfileLabels:           addConfigProfileLabels,
			Labels:                           labels,
			NodeID:                           nodeID,
			NodeProvisioningConditionMessage: "Labeling node",
		},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to label node: %w", err)
	}

	return nil
}

func configureKubeVip(ctx workflow.Context, nodeID uuid.UUID) error {
	var a *activities.Activities

	activityOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 1 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 1 * time.Minute,
			MaximumInterval: 5 * time.Minute,
			MaximumAttempts: 0, // Retry indefinitely
		},
	}

	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, activityOptions),
		a.ConfigureKubeVip,
		activities.ConfigureKubeVipInput{
			NodeID:                           nodeID,
			ConfigureSuperAdmin:              true,
			NodeProvisioningConditionMessage: "Configuring kube-vip",
		},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to configure kube-vip: %w", err)
	}

	return nil
}

func updateNodeProvisioningMessage(ctx workflow.Context, nodeID uuid.UUID, message string) error {
	var a *activities.Activities

	return workflow.ExecuteActivity(withDBOptions(ctx), a.UpdateNodeConditions, activities.NodeConditionsUpdate{
		NodeID: nodeID,
		Conditions: activities.NodeConditions{
			Provisioning: &activities.ConditionUpdate{
				Status:  db.ConditionStatusTrue,
				Message: message,
			},
		},
	}).Get(ctx, nil)
}

func markNodeAsReadyToJoin(ctx workflow.Context, nodeID uuid.UUID) error {
	var a *activities.Activities

	return workflow.ExecuteActivity(withDBOptions(ctx), a.UpdateNodeConditions, activities.NodeConditionsUpdate{
		NodeID: nodeID,
		Conditions: activities.NodeConditions{
			ReadyToJoin: &activities.ConditionUpdate{
				Status:  db.ConditionStatusTrue,
				Message: "Ready to join",
			},
		},
	}).Get(ctx, nil)
}

func markNodeAsProvisioned(ctx workflow.Context, nodeID uuid.UUID) error {
	var a *activities.Activities

	return workflow.ExecuteActivity(withDBOptions(ctx), a.UpdateNodeConditions, activities.NodeConditionsUpdate{
		NodeID: nodeID,
		Conditions: activities.NodeConditions{
			Provisioning: &activities.ConditionUpdate{
				Status:  db.ConditionStatusFalse,
				Message: provisioningCompleted,
			},
			Provisioned: &activities.ConditionUpdate{
				Status:  db.ConditionStatusTrue,
				Message: "Provisioned node",
			},
		},
	}).Get(ctx, nil)
}
