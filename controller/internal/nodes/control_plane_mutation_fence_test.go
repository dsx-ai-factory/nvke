// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodes_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/nodes"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

// TestCredentialOwnershipFencesControlPlaneDeletes covers every delete admission
// path under the cluster row lock for both credential operations, including the
// retained ownership a failed renewal leaves behind.
func TestCredentialOwnershipFencesControlPlaneDeletes(t *testing.T) {
	for _, owner := range []db.OperationType{db.OperationTypeEtcdKeyRotation, db.OperationTypeCertRenewal} {
		for _, path := range []string{"live_node", "live_pool", "controller_node", "requested_teardown"} {
			t.Run(string(owner)+"/"+path, func(t *testing.T) {
				target := joinedCPNode(cpNodeIDs(1)[0])
				target.NodePoolID = uuid.New()
				target.Conditions.DeleteRequested = db.NewCondition(path == "requested_teardown", "")
				cluster := db.Cluster{ID: target.ClusterID, Conditions: db.ClusterConditions{UpdateInProgress: &db.UpdateInProgress{Type: owner}}}
				pool := db.NodePool{ID: target.NodePoolID, ClusterID: target.ClusterID, ControlPlane: true}

				tx := &testutil.MockTx{}
				tx.On("GetNode", mock.Anything, target.ID).Return(target, nil).Maybe()
				tx.On("GetNodePool", mock.Anything, pool.ID).Return(pool, nil).Maybe()
				tx.On("GetNodePoolForUpdate", mock.Anything, pool.ID).Return(pool, nil).Maybe()
				tx.On("GetNodeForUpdate", mock.Anything, target.ID).Return(target, nil).Maybe()
				tx.On("GetClusterForUpdate", mock.Anything, target.ClusterID).Return(cluster, nil)

				ids := cpNodeIDs(4)
				expectControlPlaneCensus(tx, []db.Node{target, joinedCPNode(ids[1]), joinedCPNode(ids[2]), joinedCPNode(ids[3])})
				tx.On("UpdateNode", mock.Anything, mock.Anything).Return(target, nil).Maybe()
				tx.On("UpdateNodePool", mock.Anything, mock.Anything).Return(pool, nil).Maybe()
				tx.On("CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(db.WorkflowOutbox{}, nil).Maybe()

				var err error
				switch path {
				case "live_node":
					err = nodes.DeleteWithTx(t.Context(), tx, target.ID)
				case "live_pool":
					err = nodes.DeleteNodePoolWithTx(t.Context(), tx, pool.ID)
				case "controller_node":
					err = nodes.DeleteWithTx(t.Context(), tx, target.ID, nodes.WithSkipLiveDeleteGuard())
				case "requested_teardown":
					err = nodes.StartRequestedTeardown(t.Context(), tx, target.ID)
				}
				require.ErrorIs(t, err, db.ErrControlPlaneMutationBlocked)
				tx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
				tx.AssertNotCalled(t, "UpdateNodePool", mock.Anything, mock.Anything)
				tx.AssertNotCalled(t, "CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
			})
		}
	}
}
