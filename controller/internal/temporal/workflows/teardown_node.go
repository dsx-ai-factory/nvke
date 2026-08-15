// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package workflows

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/metrics"
	"github.com/NVIDIA/nke/controller/internal/nodes"
	"github.com/NVIDIA/nke/controller/internal/providers/forge"
	"github.com/NVIDIA/nke/controller/internal/temporal/activities"
)

type TeardownNodeInput = nodes.TeardownNodeInput

type MachineHealthIssue = forge.MachineHealthIssue

const (
	defaultTeardownRunbookTimeout = 35 * time.Minute
	drainNodeActivityTimeout      = 3 * time.Minute

	runbookEtcdMemberRemove  = "etcd-member-remove"
	etcdDryRunExitCodeSafe   = 0
	etcdDryRunExitCodeQuorum = 2
	etcdDryRunExitCodeVoter  = 3

	// forceUnreachableHookMaxAttempts bounds teardown-hook retries when the node
	// is being force-deleted as unreachable, so a hook that can never succeed
	// against a dead node fails fast instead of retrying forever.
	forceUnreachableHookMaxAttempts = 3

	// forceUnreachableRunbookEnvKey is exported to teardown hook runbooks when
	// the operator has declared the node unreachable.
	forceUnreachableRunbookEnvKey = "FORCE_UNREACHABLE"
)

// NewMachineHealthIssueFromJSON creates a MachineHealthIssue from JSON data,
// normalizing it to ensure required fields have sensible defaults.
// Use this when reading MachineHealthIssue from external/untrusted sources (e.g., annotations).
// Empty or whitespace-only input yields nil, nil.
func NewMachineHealthIssueFromJSON(data []byte) (*MachineHealthIssue, error) {
	return forge.NewMachineHealthIssueFromJSON(data)
}

func TeardownNode(ctx workflow.Context, input TeardownNodeInput) error {
	logger := workflow.GetLogger(ctx)

	workflowStart := workflow.Now(ctx)

	labels, labelsErr := getNodeMetricLabels(ctx, input.NodeID)
	if labelsErr != nil {
		logger.Warn("failed to get node metric labels, metrics will use unknown", "error", labelsErr)
		labels = nodeMetricLabels{ClusterName: unknownValue, NodeType: unknownValue}
	}

	// Check if node is a CP node
	isCPNode, err := isControlPlaneNode(ctx, input.NodeID)
	if err != nil {
		return fmt.Errorf("failed to check if node is a control-plane node: %w", err)
	}

	// if CP node, remove terminate label and finish workflow with success
	if isCPNode && !input.AllowCPNode {
		logger.Warn("Skipping teardown: Node is a control-plane node. label based teardown is not allowed for control-plane nodes", "nodeID", input.NodeID)
		deleteTerminateLabel := fmt.Sprintf("%s-", constants.LabelKeyNodeLabelTerminate)
		terminateStatus := fmt.Sprintf("%s=%s", constants.LabelKeyNodeLabelTerminateStatus, constants.LabelValueTerminateStatusNotAllowed)
		tryApplyNodeTerminatingLabels(ctx, input, deleteTerminateLabel, terminateStatus)
		return nil
	}

	terminateStatus := fmt.Sprintf("%s=%s", constants.LabelKeyNodeLabelTerminateStatus, constants.LabelValueTerminateStatusInProgress)
	tryApplyNodeTerminatingLabels(ctx, input, terminateStatus)

	if err := teardownNode(ctx, input, labels); err != nil {
		// Best effort to update the node conditions to indicate the teardown workflow failed
		if err := updateNodeDeletingMessage(ctx, input.NodeID, fmt.Sprintf("Workflow failed: %s", err)); err != nil {
			logger.Error("failed to update node conditions", "error", err)
		}

		if !workflow.IsReplaying(ctx) {
			metrics.RecordTeardownFailed(labels.ClusterName, labels.NodeType, workflow.Now(ctx).Sub(workflowStart))
		}
		return fmt.Errorf("failed to teardown node: %w", err)
	}

	totalDuration := workflow.Now(ctx).Sub(workflowStart)
	if !workflow.IsReplaying(ctx) {
		metrics.RecordTeardownCompleted(labels.ClusterName, labels.NodeType, totalDuration)
	}

	logger.Info("Node teardown complete",
		"nodeID", input.NodeID,
		"duration", totalDuration.Seconds(),
		"clusterName", labels.ClusterName,
		"nodeType", labels.NodeType,
	)

	return nil
}

func teardownNode(ctx workflow.Context, input TeardownNodeInput, labels nodeMetricLabels) error {
	var a *activities.Activities

	logger := workflow.GetLogger(ctx)
	logger.Info("Tearing down node", "nodeID", input.NodeID, "reason", input.Reason, "machineHealthIssue", input.MachineHealthIssue)

	if input.NodeID == uuid.Nil {
		return errors.New("nodeID is required")
	}

	nodeCtx, err := getNodeProvisioningContext(ctx, input.NodeID)
	if err != nil {
		return fmt.Errorf("failed to get node provisioning context: %w", err)
	}

	recordPhase := func(phase string, start, end time.Time) {
		if !workflow.IsReplaying(ctx) {
			metrics.RecordTeardownPhase(labels.ClusterName, labels.NodeType, phase, end.Sub(start))
		}
	}

	if err := cancelNodeProvisionWorkflow(ctx, input.NodeID); err != nil {
		return fmt.Errorf("failed to cancel node provision workflow: %w", err)
	}

	if err := cancelNodeRebootWorkflow(ctx, input.NodeID); err != nil {
		return fmt.Errorf("failed to cancel node reboot workflow: %w", err)
	}

	clusterRemoveStart := workflow.Now(ctx)
	if err := removeNodeFromKubernetes(ctx, input); err != nil {
		recordPhase("cluster_remove", clusterRemoveStart, workflow.Now(ctx))
		return fmt.Errorf("failed to run cluster cleanup phase: %w", err)
	}
	recordPhase("cluster_remove", clusterRemoveStart, workflow.Now(ctx))

	infraDeleteStart := workflow.Now(ctx)
	infraPhase, err := teardownNodeInfrastructure(ctx, input, nodeCtx)
	if err != nil {
		recordPhase(infraPhase, infraDeleteStart, workflow.Now(ctx))
		return err
	}
	recordPhase(infraPhase, infraDeleteStart, workflow.Now(ctx))

	if err := workflow.ExecuteActivity(withDBOptions(ctx), a.DeleteNode, input.NodeID).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to delete node: %w", err)
	}

	return nil
}

func removeNodeFromKubernetes(ctx workflow.Context, input TeardownNodeInput) error {
	if input.SkipRemoveFromCluster {
		return resetNodeForClusterTeardown(ctx, input)
	}

	return removeNodeFromLiveCluster(ctx, input)
}

// resetNodeForClusterTeardown handles the node-local reset for whole-cluster teardown.
// There is no surviving Kubernetes or etcd cluster to update in this path, so it
// skips drain, kube Node deletion, and explicit etcd member removal. It still
// performs kubeadm reset to clear local kubeadm/kubelet state and reduce zombie-node
// risk if the machine, disk, or repair workflow survives long enough for agent.
func resetNodeForClusterTeardown(ctx workflow.Context, input TeardownNodeInput) error {
	isControlPlane, err := isControlPlaneNode(ctx, input.NodeID)
	if err != nil {
		return fmt.Errorf("failed to check if node is a control-plane node: %w", err)
	}

	attemptKubeadmReset(ctx, input.NodeID, isControlPlane)

	return nil
}

// removeNodeFromLiveCluster removes a single node from a still-running Kubernetes
// cluster: run configured remove-node hooks, drain the node, remove control-plane
// etcd membership when needed, delete the kube Node object, and reset kubeadm.
func removeNodeFromLiveCluster(ctx workflow.Context, input TeardownNodeInput) error {
	if err := runTeardownHooks(ctx, input, BeforeRemoveNode); err != nil {
		return fmt.Errorf("failed to run beforeRemoveNode hooks: %w", err)
	}

	if err := removeNodeFromCluster(ctx, input); err != nil {
		return fmt.Errorf("failed to remove node from cluster: %w", err)
	}

	if err := runTeardownHooks(ctx, input, AfterRemoveNode); err != nil {
		return fmt.Errorf("failed to run afterRemoveNode hooks: %w", err)
	}

	return nil
}

// removeNodeFromCluster is the CWE-native implementation of SWE RemoveSiteNode's
// live-cluster removal sequence.
func removeNodeFromCluster(ctx workflow.Context, input TeardownNodeInput) error {
	logger := workflow.GetLogger(ctx)

	var a *activities.Activities

	var node db.Node
	if err := workflow.ExecuteActivity(withDBOptions(ctx), a.GetNode, input.NodeID).Get(ctx, &node); err != nil {
		return fmt.Errorf("failed to get node: %w", err)
	}

	var cluster db.Cluster
	if err := workflow.ExecuteActivity(withDBOptions(ctx), a.GetClusterForNode, input.NodeID).Get(ctx, &cluster); err != nil {
		return fmt.Errorf("failed to get cluster: %w", err)
	}

	var nodePool db.NodePool
	if err := workflow.ExecuteActivity(withDBOptions(ctx), a.GetNodePoolForNode, input.NodeID).Get(ctx, &nodePool); err != nil {
		return fmt.Errorf("failed to get node pool: %w", err)
	}
	isControlPlane := nodePool.ControlPlane

	// Live-cluster removal requires a surviving joined control-plane node: direct
	// tenant API drain/delete calls need a reachable API server, and etcd member
	// removal still executes as a runbook with ExcludeNodeID=self. When no such
	// peer exists — e.g. tearing down the last joined CP, or a never-provisioned
	// cluster — skip the live-cluster steps and fall through to local kubeadm reset.
	hasSurvivingJoinedCP, err := hasJoinedControlPlaneNodeExcluding(ctx, cluster.ID, input.NodeID)
	if err != nil {
		return err
	}
	if !hasSurvivingJoinedCP {
		logger.Info("Skipping Kubernetes node removal: no surviving joined control plane nodes available",
			"nodeID", input.NodeID, "clusterID", cluster.ID)
		attemptKubeadmReset(ctx, input.NodeID, isControlPlane)
		return nil
	}

	if err := drainNode(ctx, input, node, isControlPlane); err != nil {
		return fmt.Errorf("failed to drain node: %w", err)
	}

	if isControlPlane {
		if err := removeEtcdMemberForControlPlaneNode(ctx, input, cluster.ID, node); err != nil {
			return fmt.Errorf("failed to remove etcd member for control plane node: %w", err)
		}
	}

	if err := deleteKubeNode(ctx, input, node); err != nil {
		return fmt.Errorf("failed to delete kube node: %w", err)
	}

	attemptKubeadmReset(ctx, input.NodeID, isControlPlane)

	return nil
}

// hasJoinedControlPlaneNodeExcluding reports whether a joined control-plane node
// other than excludeNodeID survives the current teardown. The target cannot keep
// the tenant API available or serve as the etcd-removal runbook executor while it
// is being removed.
//
// A node already marked Deleting does not count as a survivor and can't be relied on.
func hasJoinedControlPlaneNodeExcluding(ctx workflow.Context, clusterID, excludeNodeID uuid.UUID) (bool, error) {
	var a *activities.Activities

	var cpNodes []db.Node
	if err := workflow.ExecuteActivity(withDBOptions(ctx), a.ListControlPlaneNodes, clusterID).Get(ctx, &cpNodes); err != nil {
		return false, fmt.Errorf("failed to list control plane nodes: %w", err)
	}

	for _, cpNode := range cpNodes {
		if cpNode.ID == excludeNodeID {
			continue
		}
		if cpNode.Conditions.Deleting.Status == db.ConditionStatusTrue {
			continue
		}
		if cpNode.Conditions.Joined.Status == db.ConditionStatusTrue {
			return true, nil
		}
	}

	return false, nil
}

func drainNode(ctx workflow.Context, input TeardownNodeInput, node db.Node, isControlPlane bool) error {
	logger := workflow.GetLogger(ctx)
	var a *activities.Activities

	forceDrainOnFailure := !isControlPlane || input.ForceUnreachable

	logger.Info("Draining node", "nodeID", input.NodeID, "nodeName", node.Name, "forceDrainOnFailure", forceDrainOnFailure)
	activityOptions := workflow.ActivityOptions{
		StartToCloseTimeout: drainNodeActivityTimeout,
		HeartbeatTimeout:    1 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 1 * time.Minute,
			MaximumInterval: 5 * time.Minute,
			MaximumAttempts: 3,
		},
	}
	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, activityOptions),
		a.DrainKubeNode,
		activities.DrainKubeNodeInput{
			NodeID:              input.NodeID,
			ForceDrainOnFailure: forceDrainOnFailure,
		},
	).Get(ctx, nil); err != nil {
		if input.ForceUnreachable {
			logger.Warn("Kubernetes node drain failed, continuing due to ForceUnreachable", "error", err)
			return nil
		}
		return fmt.Errorf("failed to drain Kubernetes node: %w", err)
	}

	logger.Info("Node drained successfully", "nodeID", input.NodeID, "nodeName", node.Name)
	return nil
}

func deleteKubeNode(ctx workflow.Context, input TeardownNodeInput, node db.Node) error {
	logger := workflow.GetLogger(ctx)
	var a *activities.Activities

	logger.Info("Deleting kube node", "nodeID", input.NodeID, "nodeName", node.Name)
	activityOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 1 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			MaximumInterval:    1 * time.Minute,
			BackoffCoefficient: 2,
			MaximumAttempts:    3,
		},
	}
	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, activityOptions),
		a.DeleteKubeNode,
		activities.DeleteKubeNodeInput{NodeID: input.NodeID},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to delete Kubernetes node: %w", err)
	}

	logger.Info("Kube node deleted successfully", "nodeID", input.NodeID, "nodeName", node.Name)
	return nil
}

func removeEtcdMemberForControlPlaneNode(ctx workflow.Context, input TeardownNodeInput, clusterID uuid.UUID, node db.Node) error {
	logger := workflow.GetLogger(ctx)

	// First execute the etcd-member-remove dry run to determine if the etcd member can be removed safely.
	logger.Info("Executing etcd member removal dry-run", "nodeID", input.NodeID, "nodeName", node.Name)

	expectedExitCodes := []int{etcdDryRunExitCodeSafe, etcdDryRunExitCodeQuorum, etcdDryRunExitCodeVoter}
	dryRunResult, err := executeTeardownClusterRunbook(ctx, defaultTeardownRunbookTimeout, activities.ExecuteClusterRunbookInput{
		ClusterID:         clusterID,
		RunbookName:       runbookEtcdMemberRemove,
		RunbookArgs:       []string{"--dry-run", node.Name},
		ExpectedExitCodes: expectedExitCodes,
		ExcludeNodeID:     &input.NodeID,
	})
	if err != nil {
		return fmt.Errorf("failed to execute etcd member removal dry-run: %w", err)
	}

	switch dryRunResult.ExitCode {
	case etcdDryRunExitCodeSafe:
		// Safe to remove etcd member; continue.
	case etcdDryRunExitCodeQuorum:
		if !input.AllowQuorumLoss {
			return fmt.Errorf("etcd member removal would break quorum: %s", dryRunResult.Output)
		}
		logger.Warn("Etcd member removal would break quorum, but continuing due to allow_quorum_loss", "output", dryRunResult.Output, "errorOutput", dryRunResult.ErrorMessage)
	case etcdDryRunExitCodeVoter:
		if !input.AllowSingleVoter {
			return fmt.Errorf("etcd member removal would leave single-voter cluster: %s", dryRunResult.Output)
		}
		logger.Warn("Etcd member removal would leave single-voter cluster, but continuing due to allow_single_voter", "output", dryRunResult.Output, "errorOutput", dryRunResult.ErrorMessage)
	default:
		return fmt.Errorf("etcd member removal dry-run returned unexpected exit code %d: %s; %s", dryRunResult.ExitCode, dryRunResult.Output, dryRunResult.ErrorMessage)
	}
	logger.Info("Etcd member removal dry-run completed successfully", "nodeID", input.NodeID, "nodeName", node.Name, "exitCode", dryRunResult.ExitCode, "output", dryRunResult.Output, "errorOutput", dryRunResult.ErrorMessage)

	// Then start and wait for the control-plane graceful shutdown through the agent.
	// If the agent version supports graceful shutdown (>= v0.1.22).
	logger.Info("Start and wait for control-plane graceful shutdown", "nodeID", input.NodeID, "nodeName", node.Name)
	if err := waitForControlPlaneGracefulShutdown(ctx, input); err != nil {
		if input.ForceUnreachable {
			logger.Warn("control-plane graceful shutdown failed, continuing due to force_unreachable", "error", err)
		} else {
			return fmt.Errorf("failed to wait for control-plane graceful shutdown: %w", err)
		}
	}

	// Finally, remove the etcd member from the etcd cluster.
	etcdRemoveArgs := []string{node.Name}
	if input.AllowQuorumLoss {
		etcdRemoveArgs = append(etcdRemoveArgs, "--allow-quorum-loss")
	}
	if input.AllowSingleVoter {
		etcdRemoveArgs = append(etcdRemoveArgs, "--allow-single-voter")
	}

	logger.Info("Removing etcd member", "nodeID", input.NodeID, "nodeName", node.Name, "etcdRemoveArgs", etcdRemoveArgs)
	_, err = executeTeardownClusterRunbook(ctx, defaultTeardownRunbookTimeout, activities.ExecuteClusterRunbookInput{
		ClusterID:     clusterID,
		RunbookName:   runbookEtcdMemberRemove,
		RunbookArgs:   etcdRemoveArgs,
		ExcludeNodeID: &input.NodeID,
	})
	if err != nil {
		// Unlike a graceful shutdown failure, an Etcd Member Removal failure should not be tolerated, an error here fails the teardown node workflow.
		return fmt.Errorf("failed to execute etcd member removal runbook: %w", err)
	}

	logger.Info("Etcd member removed successfully", "nodeID", input.NodeID, "nodeName", node.Name)
	return nil
}

// Make a best effort to kubeadm reset the node.
// This is to reduce the risk of leaving the node in a state that prevents it from being re-provisioned (zombie node).
// However this is not a critical operation, dead nodes are not reachable, and we should not fail the teardown node workflow if it fails.
func attemptKubeadmReset(ctx workflow.Context, nodeID uuid.UUID, isControlPlane bool) {
	logger := workflow.GetLogger(ctx)

	var a *activities.Activities

	var skipPhases []string
	if isControlPlane {
		// CWE handles etcd member removal explicitly for live CP teardown, and skips it
		// entirely for whole-cluster teardown, so kubeadm reset must not run its own
		// remove-etcd-member phase.
		skipPhases = []string{removeEtcdMember}
	}

	logger.Info("Attempting best effort kubeadm reset", "nodeID", nodeID, "skipPhases", skipPhases)
	err := workflow.ExecuteActivity(
		withKubeadmResetOptions(ctx),
		a.KubeadmReset,
		activities.KubeadmResetInput{
			NodeID:                       nodeID,
			SkipPhases:                   skipPhases,
			NodeDeletingConditionMessage: "Attempting kubeadm reset",
		},
	).Get(ctx, nil)
	if err != nil {
		logger.Warn("Best effort kubeadm reset failed, continuing teardown node workflow", "nodeID", nodeID, "skipPhases", skipPhases, "error", err)
	}
}

func teardownNodeInfrastructure(ctx workflow.Context, input TeardownNodeInput, nodeCtx activities.NodeProvisioningContext) (string, error) {
	switch nodeCtx.InfraMode {
	case activities.InfraModeForge:
		if err := teardownForgeInstance(ctx, input.NodeID, input.MachineHealthIssue); err != nil {
			return "forge_delete", fmt.Errorf("failed to teardown forge instance: %w", err)
		}
		return "forge_delete", nil
	case activities.InfraModeCAPI:
		if err := teardownCAPIMachine(ctx, input.NodeID, input.MachineHealthIssue); err != nil {
			return "capi_delete", fmt.Errorf("failed to teardown capi machine: %w", err)
		}
		return "capi_delete", nil
	default:
		return "unknown_delete", fmt.Errorf("unsupported node infra mode %q", nodeCtx.InfraMode)
	}
}

func teardownCAPIMachine(ctx workflow.Context, nodeID uuid.UUID, machineHealthIssue *MachineHealthIssue) error {
	var a *activities.Activities

	var capiMachineHealthIssue *string
	if machineHealthIssue != nil {
		data, err := json.Marshal(machineHealthIssue)
		if err != nil {
			// Fall back to summary-only so teardown is never blocked by a marshal failure.
			capiMachineHealthIssue = &machineHealthIssue.Summary
		} else {
			s := string(data)
			capiMachineHealthIssue = &s
		}
	}

	if err := workflow.ExecuteActivity(
		withK8sDeleteWaitOptions(ctx),
		a.DeleteCAPIMachineForNode,
		activities.DeleteCAPIMachineForNodeInput{
			NodeID:             nodeID,
			MachineHealthIssue: capiMachineHealthIssue,
		},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to delete capi machine: %w", err)
	}

	return nil
}

func cancelNodeProvisionWorkflow(ctx workflow.Context, nodeID uuid.UUID) error {
	if err := updateNodeDeletingMessage(ctx, nodeID, "Canceling provision workflow"); err != nil {
		return fmt.Errorf("failed to update node conditions: %w", err)
	}

	// Cancel the node provision workflow if it exists
	provisionWorkflowID := fmt.Sprintf("%s%s", constants.ProvisionNodeWorkflowPrefix, nodeID)
	if err := workflow.RequestCancelExternalWorkflow(ctx, provisionWorkflowID, "").Get(ctx, nil); err != nil {
		var notFound *temporal.UnknownExternalWorkflowExecutionError
		if !errors.As(err, &notFound) {
			return fmt.Errorf("failed to cancel provision workflow: %w", err)
		}
		// Workflow doesn't exist, which we'll consider to be a successful cancellation
	}

	return nil
}

func cancelNodeRebootWorkflow(ctx workflow.Context, nodeID uuid.UUID) error {
	if err := updateNodeDeletingMessage(ctx, nodeID, "Canceling reboot workflow"); err != nil {
		return fmt.Errorf("failed to update node conditions: %w", err)
	}

	rebootWorkflowID := fmt.Sprintf("%s%s", constants.RebootNodeWorkflowPrefix, nodeID)
	if err := workflow.RequestCancelExternalWorkflow(ctx, rebootWorkflowID, "").Get(ctx, nil); err != nil {
		var notFound *temporal.UnknownExternalWorkflowExecutionError
		if !errors.As(err, &notFound) {
			return fmt.Errorf("failed to cancel reboot workflow: %w", err)
		}
		// Workflow doesn't exist, which we'll consider to be a successful cancellation
	}

	return nil
}

func executeTeardownClusterRunbook(ctx workflow.Context, timeout time.Duration, input activities.ExecuteClusterRunbookInput) (activities.ExecuteClusterRunbookResult, error) {
	var a *activities.Activities

	var result activities.ExecuteClusterRunbookResult
	if err := workflow.ExecuteActivity(
		withTeardownRunbookOptions(ctx, timeout),
		a.ExecuteClusterRunbook,
		input,
	).Get(ctx, &result); err != nil {
		return activities.ExecuteClusterRunbookResult{}, err
	}
	return result, nil
}

func withTeardownRunbookOptions(ctx workflow.Context, timeout time.Duration) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: timeout,
		HeartbeatTimeout:    defaultRunbookActivityHeartbeatTimeout(),
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 1 * time.Minute,
			MaximumInterval: 5 * time.Minute,
			MaximumAttempts: 3,
		},
	})
}

func withKubeadmResetOptions(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout:    7 * time.Minute,
		ScheduleToCloseTimeout: 23 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			// Kubeadm reset usually completes quickly but can take several
			// minutes. Match SWE's bounded retry window:
			// 7m + 30s + 7m + 30s + 7m ~= 22m, with 1m buffer.
			InitialInterval: 30 * time.Second,
			MaximumInterval: 30 * time.Second,
			MaximumAttempts: 3,
		},
	})
}

func teardownForgeInstance(ctx workflow.Context, nodeID uuid.UUID, machineHealthIssue *MachineHealthIssue) error {
	var a *activities.Activities

	activityOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 20 * time.Minute, // usually takes about 10 minutes
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    1 * time.Minute,
			MaximumInterval:    10 * time.Minute,
			BackoffCoefficient: 2,
			MaximumAttempts:    0, // Retry indefinitely, giving operator time to manually resolve any issues
		},
	}

	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, activityOptions),
		a.DeleteForgeInstance,
		activities.DeleteForgeInstanceInput{
			NodeID:                       nodeID,
			MachineHealthIssue:           machineHealthIssue,
			NodeDeletingConditionMessage: "Tearing down Forge instance",
		},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to delete forge instance: %w", err)
	}

	return nil
}

func runTeardownHooks(ctx workflow.Context, input TeardownNodeInput, hookType HookType) error {
	var a *activities.Activities

	logger := workflow.GetLogger(ctx)
	nodeID := input.NodeID

	var configProfile activities.ConfigProfile
	if err := workflow.ExecuteActivity(withDBOptions(ctx), a.GetNodeConfigProfile, activities.GetNodeConfigProfileInput{
		NodeID: nodeID,
	}).Get(ctx, &configProfile); err != nil {
		return fmt.Errorf("failed to get config profile: %w", err)
	}

	var hooks []*activities.HookSpec
	switch hookType { //nolint:exhaustive // Only remove-node hooks are valid for teardown
	case BeforeRemoveNode:
		hooks = configProfile.Hooks.BeforeRemoveNode
	case AfterRemoveNode:
		hooks = configProfile.Hooks.AfterRemoveNode
	default:
		return fmt.Errorf("invalid teardown hook type: %s", hookType)
	}

	if len(hooks) == 0 {
		return nil
	}

	// Get node name for the runbooks
	var node db.Node
	if err := workflow.ExecuteActivity(withDBOptions(ctx), a.GetNode, nodeID).Get(ctx, &node); err != nil {
		return fmt.Errorf("failed to get node: %w", err)
	}

	// Get the cluster for this node
	var cluster db.Cluster
	if err := workflow.ExecuteActivity(withDBOptions(ctx), a.GetClusterForNode, nodeID).Get(ctx, &cluster); err != nil {
		return fmt.Errorf("failed to get cluster for node: %w", err)
	}

	// Teardown hooks run as cluster runbooks, which need a joined control-plane
	// node other than the target to execute them: the target is being removed and,
	// when it is the unreachable node an operator is force-deleting, dispatching
	// the hook to it wedges the teardown. Same reasoning as the live-removal skip
	// in removeNodeFromCluster (NKX-13735); this also covers the never-provisioned
	// cluster, where no API server ever existed to run kubectl against.
	hasSurvivingJoinedCP, err := hasJoinedControlPlaneNodeExcluding(ctx, cluster.ID, nodeID)
	if err != nil {
		return err
	}
	if !hasSurvivingJoinedCP {
		logger.Info("Skipping teardown hooks: no surviving joined control plane node to execute them",
			"hookType", hookType, "nodeID", nodeID, "clusterID", cluster.ID)
		return nil
	}

	// The default policy retries a failing hook forever. That is the right
	// behavior for a healthy node — the hooks protect data (CNPG switchover,
	// Longhorn replica migration) and must not be skipped silently. But when the
	// operator has declared the node unreachable, the hooks' own waits can never
	// be satisfied: evict-cnpg-node blocks on PVCs whose owning pod will never
	// terminate because the kubelet is gone. Bound the retries so the tolerate
	// branch below is actually reachable instead of the workflow spinning
	// forever (NKX-13160).
	maximumAttempts := int32(0)
	if input.ForceUnreachable {
		maximumAttempts = forceUnreachableHookMaxAttempts
	}
	execRunbookActivityOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 35 * time.Minute,
		HeartbeatTimeout:    defaultRunbookActivityHeartbeatTimeout(),
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 1 * time.Minute,
			MaximumInterval: 5 * time.Minute,
			MaximumAttempts: maximumAttempts,
		},
	}

	for _, hook := range hooks {
		if hook == nil || hook.Runbook == nil {
			return fmt.Errorf("invalid hook configuration: hook or hook.Runbook is nil")
		}
		if err := workflow.ExecuteActivity(
			workflow.WithActivityOptions(ctx, execRunbookActivityOptions),
			a.ExecuteClusterRunbook,
			activities.ExecuteClusterRunbookInput{
				ClusterID:     cluster.ID,
				RunbookName:   hook.Runbook.Name,
				RunbookEnv:    teardownHookRunbookEnv(hook.Runbook.Env, input.ForceUnreachable),
				RunbookArgs:   []string{node.Name},
				ExcludeNodeID: &nodeID,
			},
		).Get(ctx, nil); err != nil {
			if input.ForceUnreachable {
				logger.Warn("teardown hook runbook failed, continuing due to force_unreachable",
					"hookType", hookType, "runbook", hook.Runbook.Name, "nodeID", nodeID, "error", err)
				continue
			}
			return fmt.Errorf("failed to run %s runbook %s: %w", hookType, hook.Runbook.Name, err)
		}
	}

	return nil
}

// teardownHookRunbookEnv returns the environment for a teardown hook runbook,
// adding FORCE_UNREACHABLE when the operator has declared the node unreachable
// so hooks can take a dead-node code path instead of waiting on the target. The
// hook's configured environment is copied rather than mutated: it is owned by
// the cached config profile and shared across executions.
func teardownHookRunbookEnv(hookEnv map[string]string, forceUnreachable bool) map[string]string {
	if !forceUnreachable {
		return hookEnv
	}

	env := make(map[string]string, len(hookEnv)+1)
	maps.Copy(env, hookEnv)
	env[forceUnreachableRunbookEnvKey] = runbookEnvTrue

	return env
}

func updateNodeDeletingMessage(ctx workflow.Context, nodeID uuid.UUID, message string) error {
	var a *activities.Activities

	return workflow.ExecuteActivity(withDBOptions(ctx), a.UpdateNodeConditions, activities.NodeConditionsUpdate{
		NodeID: nodeID,
		Conditions: activities.NodeConditions{
			Deleting: &activities.ConditionUpdate{
				Status:  db.ConditionStatusTrue,
				Message: message,
			},
		},
	}).Get(ctx, nil)
}

// tryApplyNodeTerminatingLabels is a best effort to apply terminating labels to a node.
// The LabelNode activity will fail if no control plane nodes have joined the cluster yet.
// This may occur during initial setup of a cluster, and we still need the ability to delete nodes.
func tryApplyNodeTerminatingLabels(ctx workflow.Context, input TeardownNodeInput, labels ...string) {
	var a *activities.Activities

	logger := workflow.GetLogger(ctx)

	activityOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 1 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 3, // This is best effort, so limit the retries
		},
	}

	err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, activityOptions),
		a.LabelNode,
		activities.LabelNodeInput{
			NodeID: input.NodeID,
			Labels: labels,
		},
	).Get(ctx, nil)

	if err != nil {
		logger.Warn("failed to apply label to node", "error", err)
	} else {
		logger.Debug("applied label to node", "nodeID", input.NodeID, "labels", labels)
	}
}
