// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
)

const (
	provisioningCompleted = "Provisioning completed"
	removeEtcdMember      = "remove-etcd-member"
	unknownValue          = "unknown"

	// psaLabelSystemNamespacesChangeID gates LabelSystemNamespacesForPSA in
	// every workflow that runs it.
	psaLabelSystemNamespacesChangeID = "psa-label-system-namespaces"
)

type ProvisionClusterInput struct {
	ClusterID uuid.UUID
}

func ProvisionCluster(ctx workflow.Context, input ProvisionClusterInput) error {
	logger := workflow.GetLogger(ctx)

	if err := provisionCluster(ctx, input); err != nil {
		// Best effort to update the cluster conditions to indicate the provisioning workflow failed
		if err := markClusterProvisioningFailed(ctx, input.ClusterID, err); err != nil {
			logger.Error("failed to update cluster conditions", "error", err)
		}

		return fmt.Errorf("failed to provision cluster: %w", err)
	}

	return nil
}

func provisionCluster(ctx workflow.Context, input ProvisionClusterInput) error {
	if input.ClusterID == uuid.Nil {
		return errors.New("clusterID is required")
	}

	if err := provisionCAPIClusterResources(ctx, input.ClusterID); err != nil {
		return fmt.Errorf("failed to create CAPI resources: %w", err)
	}

	// Wait for one of the control plane nodes to be ready to join, choose it as the initial node
	initialNodeID, err := waitForControlPlaneNodeReadyToJoin(ctx, input.ClusterID)
	if err != nil {
		return fmt.Errorf("failed to wait for a control plane node to be ready to join: %w", err)
	}

	if err := configureInitialNodeKubeVip(ctx, input.ClusterID, initialNodeID); err != nil {
		return fmt.Errorf("failed to configure initial node KubeVip: %w", err)
	}

	// Create the cluster with a single node -- remaining CP nodes join via JoinNode (started by the control-plane controller)
	if err := kubeadmInitWithCNI(ctx, input.ClusterID, initialNodeID); err != nil {
		return err
	}

	if err := markInitialNodeAsJoined(ctx, initialNodeID); err != nil {
		return fmt.Errorf("failed to mark initial control plane node as joined: %w", err)
	}

	// This could be expanded into an add-on system later, but for now this is the only thing we install after cluster creation
	if err := installKubeLocalDNS(ctx, input.ClusterID); err != nil {
		return fmt.Errorf("failed to install kube-local-dns: %w", err)
	}

	// Clusters provisioned before this step existed must replay without it.
	if workflow.GetVersion(ctx, psaLabelSystemNamespacesChangeID, workflow.DefaultVersion, 1) >= 1 {
		if err := labelSystemNamespacesForPSA(ctx, input.ClusterID, "Labeling system namespaces for Pod Security Admission"); err != nil {
			return err
		}
	}

	// All necessary daemonsets we need running from the get-go - kube-proxy; node-local-dns; cni - [calico, ovn-kube] by default have generic
	// tolerations that can tolerate most taints. Hence, currently we don't explicitly patch tolerations
	// Future: Patch tolerations here to other components as needed for nvke.nvidia.com/provisioning=true:NoSchedule taint (added during kubeadmjoin)

	if err := markClusterAsProvisioned(ctx, input.ClusterID); err != nil {
		return fmt.Errorf("failed to mark cluster as provisioned: %w", err)
	}

	return nil
}

func provisionCAPIClusterResources(ctx workflow.Context, clusterID uuid.UUID) error {
	var a *activities.Activities

	activityOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 2 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 10 * time.Second,
			MaximumInterval: 1 * time.Minute,
			MaximumAttempts: 0,
		},
	}

	return workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, activityOptions),
		a.ApplyCAPIClusterResources,
		activities.ApplyCAPIClusterResourcesInput{ClusterID: clusterID},
	).Get(ctx, nil)
}

func waitForControlPlaneNodeReadyToJoin(ctx workflow.Context, clusterID uuid.UUID) (uuid.UUID, error) {
	var a *activities.Activities

	waitOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 1 * time.Hour, // Includes waiting for Forge instances to be SSH-able, so can be 40+ minutes
		HeartbeatTimeout:    2 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 30 * time.Second,
			MaximumInterval: 5 * time.Minute,
			MaximumAttempts: 0, // Retry indefinitely, giving operator time to manually resolve any issues
		},
	}

	var result activities.WaitForControlPlaneNodeReadyToJoinResult
	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, waitOptions),
		a.WaitForControlPlaneNodeReadyToJoin,
		activities.WaitForControlPlaneNodeReadyToJoinInput{
			ClusterID:                           clusterID,
			ClusterProvisioningConditionMessage: "Waiting for a control plane node to be ready to join",
		},
	).Get(ctx, &result); err != nil {
		return uuid.Nil, fmt.Errorf("failed to wait for a control plane node to be ready to join: %w", err)
	}

	return result.NodeID, nil
}

func configureInitialNodeKubeVip(ctx workflow.Context, clusterID, initialNodeID uuid.UUID) error {
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
			NodeID:                              initialNodeID,
			ClusterID:                           clusterID,
			ConfigureSuperAdmin:                 false, // Not needed on the initial node because it already has the super-admin kubeconfig
			ClusterProvisioningConditionMessage: "Configuring kube-vip on initial node",
		},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to configure KubeVip: %w", err)
	}

	return nil
}

// kubeadmInitWithCNI initializes the control plane and brings up cluster
// networking. CNI is installed either by the agent during kubeadm init, or
// afterwards by the InstallCNI activity; CNIInstallMode chooses which.
func kubeadmInitWithCNI(ctx workflow.Context, clusterID, initialNodeID uuid.UUID) error {
	var a *activities.Activities

	installCNIAfterInit := false
	// Clusters provisioned before the post-init path existed have neither the
	// mode lookup nor InstallCNI in their history, and must replay without them.
	if workflow.GetVersion(ctx, "post-init-cni", workflow.DefaultVersion, 1) >= 1 {
		// Asked before kubeadm runs: a cluster that cannot use the post-init path
		// has to get its CNI during init instead, and by then it is too late.
		if err := workflow.ExecuteActivity(withDBOptions(ctx), a.CNIInstallMode, clusterID).
			Get(ctx, &installCNIAfterInit); err != nil {
			return fmt.Errorf("failed to resolve the CNI install mode: %w", err)
		}
	}

	if err := kubeadmInit(ctx, clusterID, initialNodeID, installCNIAfterInit); err != nil {
		return fmt.Errorf("failed to initialize Kubernetes control plane: %w", err)
	}

	if !installCNIAfterInit {
		return nil
	}
	return installCNI(ctx, clusterID, initialNodeID)
}

func kubeadmInit(ctx workflow.Context, clusterID, initialNodeID uuid.UUID, installCNIAfterInit bool) error {
	var a *activities.Activities

	activityOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    1 * time.Minute,
			MaximumInterval:    5 * time.Minute,
			BackoffCoefficient: 2,
			MaximumAttempts:    0, // Retry indefinitely
		},
	}
	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, activityOptions),
		a.KubeadmInit,
		activities.KubeadmInitInput{
			ClusterID:                           clusterID,
			InitialNodeID:                       initialNodeID,
			InstallCNIAfterInit:                 installCNIAfterInit,
			ClusterProvisioningConditionMessage: "Initializing Kubernetes control plane",
		},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to initialize Kubernetes control plane: %w", err)
	}

	return nil
}

func installCNI(ctx workflow.Context, clusterID, initialNodeID uuid.UUID) error {
	var a *activities.Activities

	activityOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    1 * time.Minute,
			MaximumInterval:    5 * time.Minute,
			BackoffCoefficient: 2,
			MaximumAttempts:    0, // Retry indefinitely
		},
	}
	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, activityOptions),
		a.InstallCNI,
		activities.InstallCNIInput{
			ClusterID:                           clusterID,
			InitialNodeID:                       initialNodeID,
			ClusterProvisioningConditionMessage: "Installing cluster networking",
		},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to install cluster networking: %w", err)
	}

	return nil
}

func markInitialNodeAsJoined(ctx workflow.Context, nodeID uuid.UUID) error {
	var a *activities.Activities

	return workflow.ExecuteActivity(withDBOptions(ctx), a.UpdateNodeConditions, activities.NodeConditionsUpdate{
		NodeID: nodeID,
		Conditions: activities.NodeConditions{
			Joined: &activities.ConditionUpdate{
				Status:  db.ConditionStatusTrue,
				Message: "Joined as part of initial cluster creation",
			},
		},
	}).Get(ctx, nil)
}

func installKubeLocalDNS(ctx workflow.Context, clusterID uuid.UUID) error {
	var a *activities.Activities

	activityOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 1 * time.Minute,
			MaximumInterval: 5 * time.Minute,
			MaximumAttempts: 0, // Retry indefinitely
		},
	}

	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, activityOptions),
		a.InstallKubeLocalDNS,
		activities.InstallKubeLocalDNSInput{
			ClusterID:                           clusterID,
			ClusterProvisioningConditionMessage: "Installing kube-local DNS",
		},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to install kube-local-dns: %w", err)
	}

	return nil
}

// labelSystemNamespacesForPSA runs a DB read and a few namespace Get/Patch
// calls, so it gets a short timeout. Retries are bounded: by this point the CNI
// install has created every namespace it labels, so a failure that outlasts
// about seven minutes of retries is a broken cluster, not a slow one.
func labelSystemNamespacesForPSA(ctx workflow.Context, clusterID uuid.UUID, conditionMessage string) error {
	var a *activities.Activities

	activityOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 1 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    10 * time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    1 * time.Minute,
			MaximumAttempts:    10,
		},
	}

	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, activityOptions),
		a.LabelSystemNamespacesForPSA,
		activities.LabelSystemNamespacesForPSAInput{
			ClusterID:                           clusterID,
			ClusterProvisioningConditionMessage: conditionMessage,
		},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to label system namespaces for Pod Security Admission: %w", err)
	}

	return nil
}

// markClusterProvisioningFailed records a dead provisioning workflow. There is
// no failed condition, so Provisioning flips to False with the cause as the
// message; pollers would otherwise wait out their own timeout.
func markClusterProvisioningFailed(ctx workflow.Context, clusterID uuid.UUID, cause error) error {
	var a *activities.Activities

	return workflow.ExecuteActivity(withDBOptions(ctx), a.UpdateClusterConditions, activities.ClusterConditionsUpdate{
		ClusterID: clusterID,
		Conditions: activities.ClusterConditions{
			Provisioning: &activities.ConditionUpdate{
				Status:  db.ConditionStatusFalse,
				Message: fmt.Sprintf("Workflow failed: %s", cause),
			},
		},
	}).Get(ctx, nil)
}

func markClusterAsProvisioned(ctx workflow.Context, clusterID uuid.UUID) error {
	var a *activities.Activities

	return workflow.ExecuteActivity(withDBOptions(ctx), a.UpdateClusterConditions, activities.ClusterConditionsUpdate{
		ClusterID: clusterID,
		Conditions: activities.ClusterConditions{
			Provisioning: &activities.ConditionUpdate{
				Status:  db.ConditionStatusFalse,
				Message: provisioningCompleted,
			},
			Provisioned: &activities.ConditionUpdate{
				Status:  db.ConditionStatusTrue,
				Message: "Provisioned cluster",
			},
		},
	}).Get(ctx, nil)
}
