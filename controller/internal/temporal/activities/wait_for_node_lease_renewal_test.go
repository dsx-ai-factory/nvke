// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func TestWaitForNodeLeaseRenewalUsesSharedTenantClientUntilRenewed(t *testing.T) {
	nodeID := uuid.New()
	clusterID := uuid.New()
	node := db.Node{ID: nodeID, ClusterID: clusterID, Name: "worker-0"}
	cluster := db.Cluster{ID: clusterID, Name: "cluster-a"}

	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, nodeID).Return(node, nil)
	database.On("GetCluster", mock.Anything, clusterID).Return(cluster, nil)

	tenantClient := &renewingLeaseClient{
		renewTimes: []time.Time{
			time.Date(2026, time.August, 3, 12, 0, 0, 0, time.UTC),
			time.Date(2026, time.August, 3, 12, 0, 10, 0, time.UTC),
		},
	}
	provider := &fakeTenantClientProvider{tenantClient: tenantClient}
	a := &Activities{
		db:                   database,
		tenantClientProvider: provider,
	}

	result, err := executeWaitForNodeLeaseRenewalActivity(
		t,
		a,
		WaitForNodeLeaseRenewalInput{NodeID: nodeID},
		time.Millisecond,
		100*time.Millisecond,
	)
	require.NoError(t, err)
	require.Equal(t, `Successfully verified lease renewal for node "worker-0"`, result.Output)
	require.Equal(t, 1, provider.calls)
	require.Equal(t, cluster, provider.cluster)
	require.Equal(t, 2, tenantClient.calls)
	require.Equal(t, ctrlclient.ObjectKey{Namespace: "kube-node-lease", Name: node.Name}, tenantClient.key)
	database.AssertExpectations(t)
}

func TestWaitForNodeLeaseRenewalTimesOutWhenLeaseDoesNotRenew(t *testing.T) {
	nodeID := uuid.New()
	clusterID := uuid.New()
	node := db.Node{ID: nodeID, ClusterID: clusterID, Name: "worker-0"}
	cluster := db.Cluster{ID: clusterID, Name: "cluster-a"}

	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, nodeID).Return(node, nil)
	database.On("GetCluster", mock.Anything, clusterID).Return(cluster, nil)

	tenantClient := &renewingLeaseClient{
		renewTimes: []time.Time{
			time.Date(2026, time.August, 3, 12, 0, 0, 0, time.UTC),
		},
	}
	a := &Activities{
		db: database,
		tenantClientProvider: &fakeTenantClientProvider{
			tenantClient: tenantClient,
		},
	}

	_, err := executeWaitForNodeLeaseRenewalActivity(
		t,
		a,
		WaitForNodeLeaseRenewalInput{NodeID: nodeID},
		time.Millisecond,
		10*time.Millisecond,
	)
	require.Error(t, err)
	require.ErrorContains(t, err, `timeout waiting for lease to be renewed for node "worker-0"`)
	database.AssertExpectations(t)
}

func executeWaitForNodeLeaseRenewalActivity(
	t *testing.T,
	a *Activities,
	input WaitForNodeLeaseRenewalInput,
	pollInterval time.Duration,
	timeout time.Duration,
) (WaitForNodeLeaseRenewalResult, error) {
	t.Helper()

	testActivity := func(ctx context.Context, input WaitForNodeLeaseRenewalInput) (WaitForNodeLeaseRenewalResult, error) {
		return a.waitForNodeLeaseRenewal(ctx, input, pollInterval, timeout)
	}
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestActivityEnvironment()
	env.RegisterActivity(testActivity)

	value, err := env.ExecuteActivity(testActivity, input)
	if err != nil {
		return WaitForNodeLeaseRenewalResult{}, err
	}

	var result WaitForNodeLeaseRenewalResult
	require.NoError(t, value.Get(&result))
	return result, nil
}

type renewingLeaseClient struct {
	ctrlclient.Client

	renewTimes []time.Time
	calls      int
	key        ctrlclient.ObjectKey
}

func (c *renewingLeaseClient) Get(
	_ context.Context,
	key ctrlclient.ObjectKey,
	obj ctrlclient.Object,
	_ ...ctrlclient.GetOption,
) error {
	c.key = key
	index := c.calls
	if index >= len(c.renewTimes) {
		index = len(c.renewTimes) - 1
	}
	c.calls++

	renewTime := metav1.NewMicroTime(c.renewTimes[index])
	lease := obj.(*coordinationv1.Lease)
	*lease = coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: key.Namespace,
			Name:      key.Name,
		},
		Spec: coordinationv1.LeaseSpec{RenewTime: &renewTime},
	}
	return nil
}
