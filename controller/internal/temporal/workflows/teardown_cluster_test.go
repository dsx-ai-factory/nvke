// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package workflows

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/temporal/activities"
)

// callRecorder records the order teardown steps run in.
type callRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *callRecorder) record(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, name)
}

func (r *callRecorder) indexOf(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Index(r.calls, name)
}

// TestTeardownClusterDeletesTenantAccessSecretsAfterClusterRow pins the ordering
// that makes tenant-access cleanup safe.
//
// The tenant-access controller reconciles from a ListClusters snapshot taken at
// the top of its pass, so a pass already in flight can publish a cluster it saw
// as live. Deleting the Secrets while the row still exists therefore races: the
// controller re-creates them, teardown finishes, and they are orphaned with no
// row left to drive another cleanup. Deleting the row first closes that window —
// once it is gone no later pass can observe the cluster at all.
func TestTeardownClusterDeletesTenantAccessSecretsAfterClusterRow(t *testing.T) {
	clusterID := uuid.New()
	const clusterName = "tenant-a"

	rec := &callRecorder{}

	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()
	env.SetWorkerOptions(worker.Options{DeadlockDetectionTimeout: 5 * time.Second})

	a := activities.NewActivities(config.Config{}, nil, nil, nil, nil, "", nil)

	env.OnActivity(a.GetClusterProvisioningContext, mock.Anything, mock.Anything).
		Return(activities.ClusterProvisioningContext{
			ClusterID:   clusterID,
			InfraMode:   activities.InfraModeForge,
			ClusterName: clusterName,
		}, nil)
	env.OnActivity(a.UpdateClusterConditions, mock.Anything, mock.Anything).Return(nil).Maybe()
	env.OnActivity(a.TriggerClusterTeardownNodePools, mock.Anything, mock.Anything).
		Return(activities.TriggerClusterTeardownNodePoolsResult{}, nil)
	env.OnActivity(a.WaitForClusterNodePoolsDeleted, mock.Anything, mock.Anything).
		Return(activities.WaitForClusterNodePoolsDeletedResult{}, nil)
	env.OnActivity(a.DeleteEtcdEncryptionKeySecret, mock.Anything, mock.Anything).Return(nil)

	env.OnActivity(a.DeleteCluster, mock.Anything, mock.Anything).
		Return(func(context.Context, uuid.UUID) error {
			rec.record("DeleteCluster")
			return nil
		})
	env.OnActivity(a.DeleteTenantAccessSecrets, mock.Anything, mock.Anything).
		Return(func(_ context.Context, input activities.DeleteTenantAccessSecretsInput) error {
			rec.record("DeleteTenantAccessSecrets")
			// The row is gone by now, so the name must have been carried in.
			assert.Equal(t, clusterName, input.ClusterName)
			assert.Equal(t, clusterID, input.ClusterID)
			return nil
		})

	env.OnRequestCancelExternalWorkflow(mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()

	env.ExecuteWorkflow(TeardownCluster, TeardownClusterInput{ClusterID: clusterID})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	rowIdx := rec.indexOf("DeleteCluster")
	secretsIdx := rec.indexOf("DeleteTenantAccessSecrets")
	require.NotEqual(t, -1, rowIdx, "DeleteCluster should run")
	require.NotEqual(t, -1, secretsIdx, "DeleteTenantAccessSecrets should run")
	assert.Less(t, rowIdx, secretsIdx,
		"tenant-access Secrets must be deleted after the cluster row, or the controller can re-publish them")
}
