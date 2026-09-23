// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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

	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/temporal/activities"
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

// TestTeardownClusterDeletesCAPIClusterBeforeClusterRow pins the ordering that
// makes tenant-access cleanup safe without an explicit teardown step: the
// tenant-access controller owns its published Secrets by a reference to the
// CAPI Cluster, so deleting that Cluster (and waiting for it to finish
// deleting) before the DB row is what lets Kubernetes GC remove them, with no
// Temporal-driven Secret deletion required.
func TestTeardownClusterDeletesCAPIClusterBeforeClusterRow(t *testing.T) {
	clusterID := uuid.New()

	rec := &callRecorder{}

	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()
	env.SetWorkerOptions(worker.Options{DeadlockDetectionTimeout: 5 * time.Second})

	a := activities.NewActivities(config.Config{}, nil, nil, nil, "", nil)

	env.OnActivity(a.UpdateClusterConditions, mock.Anything, mock.Anything).Return(nil).Maybe()
	env.OnActivity(a.TriggerClusterTeardownNodePools, mock.Anything, mock.Anything).
		Return(activities.TriggerClusterTeardownNodePoolsResult{}, nil)
	env.OnActivity(a.WaitForClusterNodePoolsDeleted, mock.Anything, mock.Anything).
		Return(activities.WaitForClusterNodePoolsDeletedResult{}, nil)
	env.OnActivity(a.TriggerClusterTeardownControlPlaneNodes, mock.Anything, mock.Anything).
		Return(activities.TriggerClusterTeardownControlPlaneNodesResult{}, nil)
	env.OnActivity(a.WaitForClusterControlPlaneNodesDeleted, mock.Anything, mock.Anything).
		Return(activities.WaitForClusterControlPlaneNodesDeletedResult{}, nil)
	env.OnActivity(a.DeleteEtcdEncryptionKeySecret, mock.Anything, mock.Anything).Return(nil)

	env.OnActivity(a.DeleteCAPIClusterResources, mock.Anything, mock.Anything).
		Return(func(context.Context, activities.DeleteCAPIClusterResourcesInput) error {
			rec.record("DeleteCAPIClusterResources")
			return nil
		})
	env.OnActivity(a.DeleteCluster, mock.Anything, mock.Anything).
		Return(func(context.Context, uuid.UUID) error {
			rec.record("DeleteCluster")
			return nil
		})

	env.OnRequestCancelExternalWorkflow(mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()

	env.ExecuteWorkflow(TeardownCluster, TeardownClusterInput{ClusterID: clusterID})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	capiIdx := rec.indexOf("DeleteCAPIClusterResources")
	rowIdx := rec.indexOf("DeleteCluster")
	require.NotEqual(t, -1, capiIdx, "DeleteCAPIClusterResources should run")
	require.NotEqual(t, -1, rowIdx, "DeleteCluster should run")
	assert.Less(t, capiIdx, rowIdx,
		"the CAPI cluster (and, via GC, its owned tenant-access Secrets) must be deleted before the cluster row")
}
