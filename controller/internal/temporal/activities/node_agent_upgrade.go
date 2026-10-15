// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// EnsureNodeAgentVersionInput carries the immutable lifecycle target.
type EnsureNodeAgentVersionInput struct {
	NodeID  uuid.UUID
	Version string
	Method  string
}

// EnsureNodeAgentVersion performs one step and retries until the target runs.
func (a *Activities) EnsureNodeAgentVersion(ctx context.Context, input EnsureNodeAgentVersionInput) error {
	if input.Version == "" {
		return nil
	}
	node, err := a.db.GetNode(ctx, input.NodeID)
	if err != nil {
		return err
	}
	if !node.IsManagedByControlPlaneController() {
		return nil
	}
	if nodeAgentRemovalRequested(node) {
		return temporal.NewCanceledError("node-agent convergence canceled by node removal")
	}
	if err := a.reserveLifecycleAgentUpgrade(ctx, node, input); err != nil {
		return err
	}

	// Save admission before the RPC, but do not hold a database lock while the
	// agent replaces itself. Deletion must remain free to proceed.
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	version, done, callErr := a.nodeAgentClient.ConvergeNodeAgent(callCtx, node, input.Version, input.Method)
	cancel()
	if done && callErr == nil && version != input.Version {
		callErr = fmt.Errorf("node agent reports %q, expected %q", version, input.Version)
	}
	err = a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		cluster, err := tx.GetClusterForUpdate(ctx, node.ClusterID)
		if err != nil {
			return err
		}
		active := cluster.NodeAgentUpgrade
		if active == nil || active.NodeID != node.ID || active.Source != db.NodeAgentUpgradeSourceLifecycle {
			return temporal.NewCanceledError("node-agent lifecycle target is no longer active")
		}
		locked, err := tx.GetNodeForUpdate(ctx, node.ID)
		if err != nil {
			return err
		}
		if version != "" {
			locked.AgentVersion = version
			if _, err := tx.UpdateNode(ctx, &locked); err != nil {
				return fmt.Errorf("persisting agent observation: %w", err)
			}
		}
		active.Phase = "Running"
		if callErr != nil {
			active.ErrorMessage = callErr.Error()
		}
		if done && callErr == nil {
			cluster.NodeAgentUpgrade = nil
		}
		_, err = tx.UpdateCluster(ctx, &cluster)
		return err
	})
	if err != nil {
		return err
	}
	if callErr != nil {
		return callErr
	}
	if !done {
		return fmt.Errorf("node-agent convergence is in progress")
	}
	return nil
}

// reserveLifecycleAgentUpgrade saves the selected node before an upgrade RPC can start its updater.
func (a *Activities) reserveLifecycleAgentUpgrade(ctx context.Context, node db.Node, input EnsureNodeAgentVersionInput) error {
	return a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		cluster, err := tx.GetClusterForUpdate(ctx, node.ClusterID)
		if err != nil {
			return err
		}
		locked, err := tx.GetNodeForUpdate(ctx, node.ID)
		if err != nil {
			return err
		}
		if cluster.Conditions.Deleting.Status == db.ConditionStatusTrue || nodeAgentRemovalRequested(locked) {
			return temporal.NewCanceledError("node or cluster is deleting")
		}
		if active := cluster.NodeAgentUpgrade; active != nil {
			if active.NodeID != node.ID || active.Source != db.NodeAgentUpgradeSourceLifecycle || active.TargetVersion != input.Version || active.Method != input.Method {
				return fmt.Errorf("another control-plane operation is active")
			}
			return nil
		}
		if cluster.CertificateRenewal != nil || cluster.Conditions.UpdateInProgress != nil {
			return fmt.Errorf("control-plane maintenance is active")
		}
		cluster.NodeAgentUpgrade = &db.NodeAgentUpgrade{
			Source:        db.NodeAgentUpgradeSourceLifecycle,
			NodeID:        node.ID,
			TargetVersion: input.Version,
			Method:        input.Method,
			Phase:         "Pending",
		}
		_, err = tx.UpdateCluster(ctx, &cluster)
		return err
	})
}

func nodeAgentRemovalRequested(node db.Node) bool {
	return node.Conditions.DeleteRequested.Status == db.ConditionStatusTrue || node.Conditions.Deleting.Status == db.ConditionStatusTrue || node.Conditions.MarkedForRepair.Status == db.ConditionStatusTrue || node.Conditions.MarkedForRecycling.Status == db.ConditionStatusTrue
}
