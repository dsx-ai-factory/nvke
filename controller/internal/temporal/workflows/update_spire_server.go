// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
)

// UpdateSpireServerInput is the input the PATCH /clusters/{clusterID}/spire-server
// handler enqueues.
type UpdateSpireServerInput struct {
	ClusterID uuid.UUID `json:"clusterID"`
	Address   string    `json:"address"`
	Port      int       `json:"port"`
}

// UpdateSpireServer points the SPIRE agent on every SPIFFE-enabled node of a
// cluster at a new SPIRE server, all nodes in parallel, then releases the
// spire_server_update lock, recording the failed nodes in UpdateFailed.
func UpdateSpireServer(ctx workflow.Context, input UpdateSpireServerInput) error {
	logger := workflow.GetLogger(ctx)

	failureMessage := ""
	err := updateSpireServer(ctx, input)
	if err != nil {
		failureMessage = err.Error()
	}

	// Release the lock even if the workflow is being canceled, so the PATCH
	// handler is not wedged at 409.
	cleanupCtx, _ := workflow.NewDisconnectedContext(ctx)
	var a *activities.Activities
	if clearErr := workflow.ExecuteActivity(withDBOptions(cleanupCtx), a.ClearSpireServerUpdateInProgress, activities.ClearSpireServerUpdateInProgressInput{
		ClusterID:      input.ClusterID,
		FailureMessage: failureMessage,
	}).Get(cleanupCtx, nil); clearErr != nil {
		logger.Error("failed to clear SPIRE server update progress", "error", clearErr)
	}
	return err
}

func updateSpireServer(ctx workflow.Context, input UpdateSpireServerInput) error {
	logger := workflow.GetLogger(ctx)
	var a *activities.Activities

	var nodes []db.Node
	if err := workflow.ExecuteActivity(withDBOptions(ctx), a.ListNodes, db.NodeFilters{ClusterID: &input.ClusterID}).Get(ctx, &nodes); err != nil {
		return fmt.Errorf("SPIRE server update failed: listing nodes: %w", err)
	}

	nodeCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		// The node agent waits up to about three minutes for the new SVID.
		StartToCloseTimeout: 5 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 30 * time.Second,
			MaximumAttempts: 3,
		},
	})

	type pending struct {
		node   db.Node
		future workflow.Future
	}
	var started []pending
	var failed []string
	for _, node := range nodes {
		if !node.SpiffeEnabled {
			logger.Info("skipping SPIRE server update on node without SPIFFE", "node", node.Name)
			continue
		}
		if node.IP == "" {
			// It cannot be reached yet, so it still points at the old server.
			logger.Error("SPIRE server update failed on node without an IP", "node", node.Name)
			failed = append(failed, node.Name)
			continue
		}
		started = append(started, pending{node: node, future: workflow.ExecuteActivity(nodeCtx, a.ReconfigureNodeSpireAgent, activities.ReconfigureNodeSpireAgentInput{
			NodeID:  node.ID,
			Address: input.Address,
			Port:    input.Port,
		})})
	}

	for _, p := range started {
		if err := p.future.Get(ctx, nil); err != nil {
			logger.Error("SPIRE server update failed on node", "node", p.node.Name, "error", err)
			failed = append(failed, p.node.Name)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("SPIRE server update failed on nodes: %s", strings.Join(failed, ", "))
	}
	return nil
}
