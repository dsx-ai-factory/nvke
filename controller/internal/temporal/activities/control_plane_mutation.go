// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"

	"go.temporal.io/sdk/activity"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// reserveControlPlaneDeletion fences CAPI machine deletion, even for a node
// that never joined. Keep Deleting set until the node is removed, including
// uncertain remote outcomes; infrastructure deletion is never pre-join cleanup.
func (a *Activities) reserveControlPlaneDeletion(ctx context.Context, node db.Node) error {
	if !node.ControlPlane {
		return nil
	}
	return a.UpdateNodeConditions(ctx, NodeConditionsUpdate{
		NodeID: node.ID,
		Conditions: NodeConditions{Deleting: &ConditionUpdate{
			Status: db.ConditionStatusTrue, Message: "Control-plane deletion is in progress",
		}},
	})
}

// reserveControlPlaneCleanup covers etcd removal and kubeadm reset, which are
// also used before a first/retried join. Classify lifecycle only after taking
// the cluster and node locks: teardown intent wins over an unfinished join.
// Retain the reservation on uncertain RPC outcomes; normal join completion
// clears Provisioning, while real deletion remains reserved until node removal.
func (a *Activities) reserveControlPlaneCleanup(ctx context.Context, node db.Node) error {
	if !node.ControlPlane {
		return nil
	}
	return a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		if err := controlPlaneMutationLock(ctx, tx, node); err != nil {
			return err
		}
		locked, err := tx.GetNodeForUpdate(ctx, node.ID)
		if err != nil {
			return fmt.Errorf("locking control-plane cleanup node: %w", err)
		}
		if locked.Conditions.Joined.Status != db.ConditionStatusTrue &&
			locked.Conditions.DeleteRequested.Status != db.ConditionStatusTrue &&
			locked.Conditions.Deleting.Status != db.ConditionStatusTrue {
			db.SetCondition(&locked.Conditions.Provisioning, db.NewCondition(true, "Control-plane pre-join cleanup is in progress"))
		} else {
			db.SetCondition(&locked.Conditions.Deleting, db.NewCondition(true, "Control-plane deletion is in progress"))
		}
		_, err = tx.UpdateNode(ctx, &locked)
		return err
	})
}

func controlPlaneMutationLock(ctx context.Context, tx db.Tx, node db.Node) error {
	if !node.ControlPlane {
		return nil
	}
	cluster, err := tx.GetClusterForUpdate(ctx, node.ClusterID)
	if err != nil {
		return fmt.Errorf("locking cluster for control-plane mutation: %w", err)
	}
	if db.ControlPlaneMutationBlocked(cluster) {
		return db.ErrControlPlaneMutationBlocked
	}
	return nil
}

func (a *Activities) reserveControlPlaneUpgrade(ctx context.Context, node db.Node, version string, start bool) error {
	message := controlPlaneUpgradeAdmissionMessage(ctx, version)
	return a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		if err := controlPlaneMutationLock(ctx, tx, node); err != nil {
			return err
		}
		locked, err := tx.GetNodeForUpdate(ctx, node.ID)
		if err != nil {
			return fmt.Errorf("locking upgrade node: %w", err)
		}
		condition := &locked.Conditions.Provisioning
		if condition.Status == db.ConditionStatusTrue && condition.Message != message {
			return fmt.Errorf("control-plane node already has provisioning work in progress")
		}
		*condition = db.NewCondition(start, message)
		_, err = tx.UpdateNode(ctx, &locked)
		return err
	})
}

func controlPlaneUpgradeAdmissionMessage(ctx context.Context, version string) string {
	info := activity.GetInfo(ctx)
	return fmt.Sprintf("Kubeadm control-plane upgrade %s (%s/%s)", version, info.WorkflowExecution.ID, info.ActivityID)
}
