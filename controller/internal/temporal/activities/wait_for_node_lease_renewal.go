// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	nodeLeaseRenewalPollInterval = 10 * time.Second
	nodeLeaseRenewalTimeout      = 80 * time.Second
)

type WaitForNodeLeaseRenewalInput struct {
	NodeID uuid.UUID
}

type WaitForNodeLeaseRenewalResult struct {
	Output string
}

func (a *Activities) WaitForNodeLeaseRenewal(
	ctx context.Context,
	input WaitForNodeLeaseRenewalInput,
) (WaitForNodeLeaseRenewalResult, error) {
	return a.waitForNodeLeaseRenewal(
		ctx,
		input,
		nodeLeaseRenewalPollInterval,
		nodeLeaseRenewalTimeout,
	)
}

func (a *Activities) waitForNodeLeaseRenewal(
	ctx context.Context,
	input WaitForNodeLeaseRenewalInput,
	pollInterval time.Duration,
	timeout time.Duration,
) (WaitForNodeLeaseRenewalResult, error) {
	logger := activity.GetLogger(ctx)

	logger.Info("Waiting for node lease renewal", "nodeID", input.NodeID)

	node, err := a.db.GetNode(ctx, input.NodeID)
	if err != nil {
		return WaitForNodeLeaseRenewalResult{}, fmt.Errorf("failed to get node: %w", err)
	}

	cluster, err := a.db.GetCluster(ctx, node.ClusterID)
	if err != nil {
		return WaitForNodeLeaseRenewalResult{}, fmt.Errorf("failed to get cluster: %w", err)
	}

	tenantClient, err := a.tenantClientForCluster(ctx, cluster)
	if err != nil {
		return WaitForNodeLeaseRenewalResult{}, err
	}

	pollCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var (
		initialRenewTime    time.Time
		hasInitialRenewTime bool
	)
	for {
		activity.RecordHeartbeat(ctx)

		var lease coordinationv1.Lease
		err := tenantClient.Get(pollCtx, client.ObjectKey{
			Namespace: corev1.NamespaceNodeLease,
			Name:      node.Name,
		}, &lease)
		if err != nil {
			logger.Debug("Failed to get node lease, waiting to retry", "nodeID", input.NodeID, "error", err)
		} else if lease.Spec.RenewTime != nil {
			renewTime := lease.Spec.RenewTime.Time
			if !hasInitialRenewTime {
				initialRenewTime = renewTime
				hasInitialRenewTime = true
			} else if !renewTime.Equal(initialRenewTime) {
				logger.Info("Node lease renewed", "nodeID", input.NodeID, "renewTime", renewTime)
				return WaitForNodeLeaseRenewalResult{
					Output: fmt.Sprintf("Successfully verified lease renewal for node %q", node.Name),
				}, nil
			}
		}

		select {
		case <-pollCtx.Done():
			if err := ctx.Err(); err != nil {
				return WaitForNodeLeaseRenewalResult{},
					fmt.Errorf("context canceled while waiting for node lease renewal: %w", err)
			}
			if !hasInitialRenewTime {
				return WaitForNodeLeaseRenewalResult{},
					fmt.Errorf("timeout waiting for lease object for node %q", node.Name)
			}
			return WaitForNodeLeaseRenewalResult{},
				fmt.Errorf("timeout waiting for lease to be renewed for node %q; node might be unavailable", node.Name)
		case <-ticker.C:
		}
	}
}
