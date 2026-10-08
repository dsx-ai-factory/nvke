// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"

	"go.temporal.io/sdk/activity"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// reserveControlPlaneDeletion fences direct/retried destructive activities as
// well as workflow admission. Keep Deleting set until the node is removed, even
// when a remote call's outcome is uncertain; credential operations must not race that work.
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
