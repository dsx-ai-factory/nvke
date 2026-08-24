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

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

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
	if err := provisionNodeInfrastructure(ctx, nodeCtx); err != nil {
		switch nodeCtx.InfraMode {
		case activities.InfraModeCAPI:
			if err := updateNodeProvisioningMessage(ctx, input.NodeID, fmt.Sprintf("Workflow failed: %s", err)); err != nil {
				logger.Error("failed to update node conditions", "error", err)
			}
			recordPhase("capi_create", infraProvisionStart, workflow.Now(ctx))
		case activities.InfraModeForge:
			// Best effort to update the node conditions to indicate the pending workflow failed
			if err := updateNodePendingMessage(ctx, input.NodeID, fmt.Sprintf("Workflow failed: %s", err)); err != nil {
				logger.Error("failed to update node conditions", "error", err)
			}
			recordPhase("forge_create", infraProvisionStart, workflow.Now(ctx))
		}

		if !workflow.IsReplaying(ctx) {
			metrics.RecordProvisionFailed(labels.ClusterName, labels.NodeType, workflow.Now(ctx).Sub(workflowStart))
		}
		return fmt.Errorf("failed to provision node infrastructure: %w", err)
	}

	switch nodeCtx.InfraMode {
	case activities.InfraModeCAPI:
		recordPhase("capi_create", infraProvisionStart, workflow.Now(ctx))
	case activities.InfraModeForge:
		recordPhase("forge_create", infraProvisionStart, workflow.Now(ctx))
	}

	if err := provisionNode(ctx, input, nodeCtx, labels); err != nil {
		// Best effort to update the node conditions to indicate the provisioning workflow failed
		if err := updateNodeProvisioningMessage(ctx, input.NodeID, fmt.Sprintf("Workflow failed: %s", err)); err != nil {
			logger.Error("failed to update node conditions", "error", err)
		}

		if !workflow.IsReplaying(ctx) {
			metrics.RecordProvisionFailed(labels.ClusterName, labels.NodeType, workflow.Now(ctx).Sub(workflowStart))
		}
		return fmt.Errorf("failed to provision node: %w", err)
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

func provisionNodeInfrastructure(ctx workflow.Context, nodeCtx activities.NodeProvisioningContext) error {
	switch nodeCtx.InfraMode {
	case activities.InfraModeCAPI:
		if err := provisionCAPINode(ctx, nodeCtx.NodeID); err != nil {
			return fmt.Errorf("failed to provision CAPI node: %w", err)
		}
	case activities.InfraModeForge:
		// We only want to move from Pending to Provisioning if the forge instance creation succeeds.
		if err := provisionForgeInstance(ctx, nodeCtx.NodeID); err != nil {
			return fmt.Errorf("failed to provision Forge instance: %w", err)
		}
	default:
		return fmt.Errorf("unsupported node infra mode %q", nodeCtx.InfraMode)
	}

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
				Status:  db.ConditionStatusFalse,
				Message: "",
			},
			Provisioning: &activities.ConditionUpdate{
				Status:  db.ConditionStatusTrue,
				Message: "Creating CAPI infra machine",
			},
		},
	}).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to mark node as provisioning: %w", err)
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

func provisionNode(ctx workflow.Context, input ProvisionNodeInput, nodeCtx activities.NodeProvisioningContext, labels nodeMetricLabels) error {
	recordPhase := func(phase string, start, end time.Time) {
		if !workflow.IsReplaying(ctx) {
			metrics.RecordProvisionPhase(labels.ClusterName, labels.NodeType, phase, end.Sub(start))
		}
	}

	if nodeCtx.InfraMode == activities.InfraModeForge {
		forgeReadyStart := workflow.Now(ctx)
		if err := waitForForgeInstance(ctx, input.NodeID); err != nil {
			recordPhase("forge_ready", forgeReadyStart, workflow.Now(ctx))
			return fmt.Errorf("failed to wait for Forge instance to be ready: %w", err)
		}
		recordPhase("forge_ready", forgeReadyStart, workflow.Now(ctx))
	}

	if nodeCtx.InfraMode == activities.InfraModeCAPI {
		capiReadyStart := workflow.Now(ctx)
		if err := waitForCAPIMachine(ctx, input.NodeID); err != nil {
			recordPhase("capi_ready", capiReadyStart, workflow.Now(ctx))
			return fmt.Errorf("failed to wait for CAPI machine to be ready: %w", err)
		}
		recordPhase("capi_ready", capiReadyStart, workflow.Now(ctx))
	}

	hostPrepStart := workflow.Now(ctx)
	endHostPrep := func() { recordPhase("host_preparation", hostPrepStart, workflow.Now(ctx)) }

	if err := waitForNodePreflight(ctx, input.NodeID); err != nil {
		endHostPrep()
		return fmt.Errorf("failed to wait for node preflight to succeed: %w", err)
	}

	hasConfigProfile := nodeCtx.ConfigProfile != ""
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

	if nodeCtx.ManagedByNodePool {
		if err := waitForNodePoolProvisioning(ctx, input.NodeID); err != nil {
			return fmt.Errorf("failed to wait for node pool provisioning: %w", err)
		}
	}

	joinStart := workflow.Now(ctx)
	isInitialNode, err := joinNode(ctx, input.NodeID)
	if err != nil {
		recordPhase("join", joinStart, workflow.Now(ctx))
		return fmt.Errorf("failed to join node: %w", err)
	}
	recordPhase("join", joinStart, workflow.Now(ctx))

	postJoinStart := workflow.Now(ctx)
	endPostJoin := func() { recordPhase("post_join", postJoinStart, workflow.Now(ctx)) }

	if err := labelNode(ctx, input.NodeID, hasConfigProfile); err != nil {
		endPostJoin()
		return fmt.Errorf("failed to label node: %w", err)
	}

	if err := runHooks(ctx, input.NodeID, AfterJoinNode, configProfile); err != nil {
		endPostJoin()
		return fmt.Errorf("failed to run afterJoinNode hooks: %w", err)
	}

	// If the node is the initial control plane node, we don't need to configure KubeVip, because the cluster creation process does it.
	if !isInitialNode {
		// This is really only needed for control plane nodes -- will be a no-op if it's a worker node
		if err := configureKubeVip(ctx, input.NodeID); err != nil {
			endPostJoin()
			return fmt.Errorf("failed to configure KubeVip: %w", err)
		}
	}

	// Remove taint (nke.nvidia.com/provisioning=true:NoSchedule-)
	// This taint is added via kubeadm join config.
	// Hyphen at the end of the taint effect removes the taint
	taintToRemove := constants.NodeTaintProvisioning()
	if err := taintNode(ctx, input.NodeID, taintToRemove.Key, taintToRemove.Value, taintToRemove.Effect+"-"); err != nil {
		endPostJoin()
		return fmt.Errorf("failed to untaint node: %w", err)
	}

	if err := markNodeAsProvisioned(ctx, input.NodeID); err != nil {
		endPostJoin()
		return fmt.Errorf("failed to mark node as provisioned: %w", err)
	}
	endPostJoin()

	return nil
}

func provisionForgeInstance(ctx workflow.Context, nodeID uuid.UUID) error {
	var a *activities.Activities

	// Optimistically try to provision the forge instance with limited retries.
	//
	// We only retry 3 times instead of indefinitely to handle race conditions:
	// When multiple node workflows compete for the last available forge machine,
	// only one will succeed. The losing workflows should fail quickly rather than
	// retry endlessly.
	//
	// Failed workflows won't be rescheduled until new forge capacity becomes
	// available, since node schedulers check forge capacity before scheduling.
	// This prevents a buildup of perpetually failing workflows.
	activityOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 1 * time.Minute, // Calling a synchronous API via FWE/temporal
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    1 * time.Minute,
			MaximumInterval:    10 * time.Minute,
			BackoffCoefficient: 2,
			MaximumAttempts:    3,
		},
	}

	var createInstanceResponse *forgesdk.Instance
	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, activityOptions),
		a.CreateForgeInstance,
		activities.CreateForgeInstanceInput{
			NodeID: nodeID,
		},
	).Get(ctx, &createInstanceResponse); err != nil {
		return fmt.Errorf("failed to create Forge instance: %w", err)
	}

	if err := workflow.ExecuteActivity(withDBOptions(ctx), a.UpdateNodeForgeDetails, activities.NodeForgeDetailsUpdate{
		NodeID:          nodeID,
		ForgeInstanceID: createInstanceResponse.GetId(),
		ForgeMachineID:  createInstanceResponse.GetMachineId(),
		// IP is not immediately available
	}).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to update node Forge details: %w", err)
	}

	return nil
}

func waitForForgeInstance(ctx workflow.Context, nodeID uuid.UUID) error {
	var a *activities.Activities

	activityOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Minute, // Usually takes about 5 minutes
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    1 * time.Minute,
			MaximumInterval:    10 * time.Minute,
			BackoffCoefficient: 2,
			MaximumAttempts:    0, // Retry indefinitely
		},
	}

	var waitForForgeInstanceReadyResponse *forgesdk.Instance
	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, activityOptions),
		a.WaitForForgeInstanceReady,
		activities.WaitForForgeInstanceReadyInput{
			NodeID:                           nodeID,
			NodeProvisioningConditionMessage: "Waiting for Forge instance to be ready",
		},
	).Get(ctx, &waitForForgeInstanceReadyResponse); err != nil {
		return fmt.Errorf("failed to wait for Forge instance to be ready: %w", err)
	}

	interfaces := waitForForgeInstanceReadyResponse.GetInterfaces()
	if len(interfaces) == 0 {
		return fmt.Errorf("forge instance %s has no interfaces", waitForForgeInstanceReadyResponse.GetId())
	}

	ipAddresses := interfaces[0].GetIpAddresses()
	if len(ipAddresses) == 0 {
		return fmt.Errorf("forge instance %s has no IP addresses", waitForForgeInstanceReadyResponse.GetId())
	}

	if err := workflow.ExecuteActivity(withDBOptions(ctx), a.UpdateNodeForgeDetails, activities.NodeForgeDetailsUpdate{
		NodeID: nodeID,
		IP:     ipAddresses[0],
	}).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to update node IP address: %w", err)
	}

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

func joinNode(ctx workflow.Context, nodeID uuid.UUID) (bool, error) {
	var a *activities.Activities

	activityOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 1 * time.Minute,
			MaximumInterval: 5 * time.Minute,
			MaximumAttempts: 0, // Retry indefinitely
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
			AddTopologyLabels:                true,
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

func updateNodePendingMessage(ctx workflow.Context, nodeID uuid.UUID, message string) error {
	var a *activities.Activities

	return workflow.ExecuteActivity(withDBOptions(ctx), a.UpdateNodeConditions, activities.NodeConditionsUpdate{
		NodeID: nodeID,
		Conditions: activities.NodeConditions{
			Pending: &activities.ConditionUpdate{
				Status:  db.ConditionStatusTrue,
				Message: message,
			},
		},
	}).Get(ctx, nil)
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
