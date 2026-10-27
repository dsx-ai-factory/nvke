// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// stableControlPlane runs under the cluster row lock shared by every admission
// writer. Existing conditions cover accepted outbox work as well as agent-side
// joins that outlive their activity. Do not seize ownership from it. The caller
// supplies the non-retryable precondition error of the operation being admitted.
func stableControlPlane(ctx context.Context, tx db.Tx, clusterID uuid.UUID, precondition func(string) error) ([]db.Node, error) {
	cp := true
	pools, err := tx.ListNodePools(ctx, db.NodePoolFilters{ClusterID: &clusterID, ControlPlane: &cp})
	if err != nil {
		return nil, errors.New("cannot inspect control-plane pools before credential operation")
	}
	for _, pool := range pools {
		if pool.Conditions.Deleting.Status == db.ConditionStatusTrue {
			return nil, precondition("control-plane pool deletion is unfinished")
		}
	}
	nodes, err := tx.ListNodes(ctx, db.NodeFilters{ClusterID: &clusterID, ControlPlane: &cp})
	if err != nil {
		return nil, errors.New("cannot inspect control-plane nodes before credential operation")
	}
	if len(nodes) == 0 {
		return nil, precondition("no control-plane nodes are available")
	}
	for _, node := range nodes {
		if node.IP == "" || node.Conditions.Joined.Status != db.ConditionStatusTrue ||
			node.Conditions.Provisioned.Status != db.ConditionStatusTrue ||
			node.Conditions.Provisioning.Status == db.ConditionStatusTrue ||
			node.Conditions.DeleteRequested.Status == db.ConditionStatusTrue ||
			node.Conditions.Deleting.Status == db.ConditionStatusTrue ||
			node.Conditions.Rebooting.Status == db.ConditionStatusTrue {
			return nil, precondition("control-plane membership work is unfinished")
		}
	}
	return nodes, nil
}
