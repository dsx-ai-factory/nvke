// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/NVIDIA/nke/controller/internal/db"
)

type WaitForControlPlaneNodeReadyToJoinInput struct {
	ClusterID uuid.UUID

	ClusterProvisioningConditionMessage string
}

type WaitForControlPlaneNodeReadyToJoinResult struct {
	NodeID uuid.UUID
}

// WaitForControlPlaneNodeReadyToJoin polls the database until at least one control plane node is ready to join.
func (a *Activities) WaitForControlPlaneNodeReadyToJoin(
	ctx context.Context,
	input WaitForControlPlaneNodeReadyToJoinInput,
) (WaitForControlPlaneNodeReadyToJoinResult, error) {
	conditionUpdateWrapper := ActivityStatusConditionUpdateWrapper[WaitForControlPlaneNodeReadyToJoinResult]{
		Message:         input.ClusterProvisioningConditionMessage,
		UpdateCondition: a.clusterProvisioningConditionMessageUpdater(input.ClusterID),
		ActivityBody: func() (WaitForControlPlaneNodeReadyToJoinResult, error) {
			return a.waitForControlPlaneNodeReadyToJoin(ctx, input)
		},
	}

	return conditionUpdateWrapper.Run(ctx)
}

func (a *Activities) waitForControlPlaneNodeReadyToJoin(
	ctx context.Context,
	input WaitForControlPlaneNodeReadyToJoinInput,
) (WaitForControlPlaneNodeReadyToJoinResult, error) {
	pollInterval := 10 * time.Second
	logger := activity.GetLogger(ctx)

	logger.Info("Waiting for a control plane node to be ready to join", "clusterID", input.ClusterID)

	// Fail fast when the cluster's site is not registered. The scheduler only
	// works through registered sites, so with none there nothing is scheduled,
	// no node ever becomes ready, and this polls for the full hour before
	// Temporal retries it -- all while reporting "waiting for a control plane
	// node", which names a symptom rather than the cause.
	if err := a.assertSiteRegistered(ctx, input.ClusterID); err != nil {
		return WaitForControlPlaneNodeReadyToJoinResult{}, err
	}

	for {
		// Record heartbeat to prevent activity timeout
		activity.RecordHeartbeat(ctx)

		// Get all control plane nodes for this cluster
		controlPlane := true
		nodes, err := a.db.ListNodes(ctx, db.NodeFilters{
			ClusterID:    &input.ClusterID,
			ControlPlane: &controlPlane,
		})
		if err != nil {
			return WaitForControlPlaneNodeReadyToJoinResult{}, fmt.Errorf("failed to list control plane nodes: %w", err)
		}

		for _, node := range nodes {
			if node.Conditions.ReadyToJoin.Status == db.ConditionStatusTrue {
				return WaitForControlPlaneNodeReadyToJoinResult{NodeID: node.ID}, nil
			}
		}

		// Sleep before polling again
		select {
		case <-ctx.Done():
			return WaitForControlPlaneNodeReadyToJoinResult{},
				fmt.Errorf("context canceled while waiting for a control plane node to be ready to join: %w", ctx.Err())
		case <-time.After(pollInterval):
			// Continue polling
		}
	}
}

// assertSiteRegistered returns a non-retryable error when the cluster's site
// does not exist, so a missing site surfaces as an error naming the site rather
// than as provisioning that appears merely slow.
func (a *Activities) assertSiteRegistered(ctx context.Context, clusterID uuid.UUID) error {
	cluster, err := a.db.GetCluster(ctx, clusterID)
	if err != nil {
		// Leave cluster lookup problems to the normal retry path.
		return nil //nolint:nilerr // not this check's job to report
	}
	if _, err := a.db.GetSite(ctx, cluster.SiteID, false); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return temporal.NewNonRetryableApplicationError(
				fmt.Sprintf("site %s is not registered, so no node can be scheduled for cluster %s",
					cluster.SiteID, clusterID),
				"site.NotRegistered", err,
			)
		}
	}
	return nil
}
