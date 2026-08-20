// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/nke/controller/internal/constants"
)

type WaitForNodeRebootInput struct {
	NodeID uuid.UUID

	// PreviousBootID is the bootID of the node before it rebooted.
	// This must be provided so that the wait can key on the bootID changing,
	// which indicates the node has booted and kubelet has reported node system info.
	PreviousBootID string

	// Whether to also check the nke.nvidia.com/reboot-complete-timestamp annotation to consider the node rebooted if it is present and later than the reboot request timestamp.
	CheckRebootCompleteAnnotation bool

	// If CheckRebootCompleteAnnotation is true, the node is considered rebooted if the annotation is present and the timestamp is later than this time.
	AnnotationAfterTime time.Time

	// Optional polling interval. Defaults to 1 minute.
	PollingInterval time.Duration
}

type WaitForNodeRebootResult struct {
	// The new bootID of the node.
	BootID string
}

func (a *Activities) WaitForNodeReboot(ctx context.Context, input WaitForNodeRebootInput) (WaitForNodeRebootResult, error) {
	conditionUpdateWrapper := ActivityStatusConditionUpdateWrapper[WaitForNodeRebootResult]{
		Message:         "Waiting for node to reboot",
		UpdateCondition: a.nodeRebootingConditionMessageUpdater(input.NodeID),
		ActivityBody: func() (WaitForNodeRebootResult, error) {
			return a.waitForNodeReboot(ctx, input)
		},
	}

	return conditionUpdateWrapper.Run(ctx)
}

func (a *Activities) waitForNodeReboot(ctx context.Context, input WaitForNodeRebootInput) (WaitForNodeRebootResult, error) {
	pollInterval := input.PollingInterval
	if pollInterval == 0 {
		pollInterval = 1 * time.Minute
	}

	logger := activity.GetLogger(ctx)

	// This is a long running blocking activity.
	// Start a heartbeat goroutine to prevent the activity from timing out.
	heartbeatCancel := startActivityHeartbeat(ctx)
	defer heartbeatCancel()

	logger.Info("Waiting for node to reboot", "nodeID", input.NodeID, "previousBootID", input.PreviousBootID)

	machineNamespace := ""
	machineName := ""
	if input.CheckRebootCompleteAnnotation {
		node, err := a.db.GetNode(ctx, input.NodeID)
		if err != nil {
			return WaitForNodeRebootResult{}, fmt.Errorf("failed to get node: %w", err)
		}

		machineNamespace = constants.NamespaceNKESystem
		machineName = node.Name
	}

	for {
		result, err := a.getKubeNode(ctx, GetKubeNodeInput{NodeID: input.NodeID})
		if err != nil {
			return WaitForNodeRebootResult{}, fmt.Errorf("failed to get kube node: %w", err)
		}

		kubeNodeBootID := result.Node.Status.NodeInfo.BootID
		if kubeNodeBootID != input.PreviousBootID {
			logger.Info("Node successfully rebooted", "nodeID", input.NodeID, "bootID", kubeNodeBootID)
			return WaitForNodeRebootResult{
				BootID: kubeNodeBootID,
			}, nil
		}

		if input.CheckRebootCompleteAnnotation {
			rebootComplete, rebootCompleteTimestamp, err := a.getMachineRebootCompleteTimestamp(ctx, machineNamespace, machineName, input.AnnotationAfterTime)
			if err != nil {
				// If this errors for any reason, which includes capi core not being installed, or a malformed annotation, just consider it not rebooted yet.
				logger.Debug("failed to get machine reboot complete timestamp, considering it not rebooted yet", "error", err)
			} else if rebootComplete {
				logger.Info("Node successfully rebooted by annotation", "nodeID", input.NodeID, "bootID", kubeNodeBootID, "rebootCompleteTimestamp", rebootCompleteTimestamp)
				return WaitForNodeRebootResult{
					BootID: kubeNodeBootID,
				}, nil
			}
		}

		// Node has same bootID as previous bootID, so it has not rebooted yet.
		logger.Info("Node has not rebooted yet", "nodeID", input.NodeID, "bootID", kubeNodeBootID, "previousBootID", input.PreviousBootID)

		// Sleep before polling again
		select {
		case <-ctx.Done():
			return WaitForNodeRebootResult{}, fmt.Errorf("context canceled while waiting for node reboot: %w", ctx.Err())
		case <-time.After(pollInterval):
			// Continue polling
		}
	}
}

func (a *Activities) getMachineRebootCompleteTimestamp(ctx context.Context, machineNamespace, machineName string, annotationAfterTime time.Time) (bool, time.Time, error) {
	machine := &clusterv1.Machine{}
	err := a.k8sClient.Get(ctx, client.ObjectKey{
		Namespace: machineNamespace,
		Name:      machineName,
	}, machine)
	if err != nil {
		return false, time.Time{}, fmt.Errorf("failed to get machine: %w", err)
	}

	rebootCompleteTimestamp, ok := machine.Annotations[constants.MachineAnnotationKeyRebootCompleteTimestamp]
	if !ok {
		return false, time.Time{}, nil
	}

	rebootCompleteTime, err := time.Parse(time.RFC3339, rebootCompleteTimestamp)
	if err != nil {
		return false, time.Time{}, fmt.Errorf("failed to parse reboot complete timestamp: %s: %w", rebootCompleteTimestamp, err)
	}

	return rebootCompleteTime.After(annotationAfterTime), rebootCompleteTime, nil
}
