// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/testutil"
)

func TestGetKubeNodeReadsTenantNodeDirectly(t *testing.T) {
	nodeID := uuid.New()
	clusterID := uuid.New()
	node := db.Node{
		ID:        nodeID,
		ClusterID: clusterID,
		Name:      "worker-0",
	}
	cluster := db.Cluster{ID: clusterID, Name: "cluster-a"}

	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, nodeID).Return(node, nil)
	database.On("GetCluster", mock.Anything, clusterID).Return(cluster, nil)

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	tenantClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   node.Name,
				Labels: map[string]string{"role": "worker"},
			},
		}).
		Build()
	provider := &fakeTenantClientProvider{tenantClient: tenantClient}

	a := &Activities{
		db:                   database,
		tenantClientProvider: provider,
	}
	result, err := executeGetKubeNodeActivity(t, a, GetKubeNodeInput{NodeID: nodeID})
	require.NoError(t, err)
	require.Equal(t, nodeID, result.NodeID)
	require.Equal(t, node.Name, result.Node.Name)
	require.Equal(t, "worker", result.Node.Labels["role"])
	require.Equal(t, 1, provider.calls)
	require.Equal(t, cluster, provider.cluster)
}

func TestGetKubeNodeReturnsTenantClientError(t *testing.T) {
	nodeID := uuid.New()
	clusterID := uuid.New()
	node := db.Node{ID: nodeID, ClusterID: clusterID, Name: "worker-0"}
	cluster := db.Cluster{ID: clusterID, Name: "cluster-a"}

	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, nodeID).Return(node, nil)
	database.On("GetCluster", mock.Anything, clusterID).Return(cluster, nil)

	providerErr := errors.New("tenant API unavailable")
	a := &Activities{
		db:                   database,
		tenantClientProvider: &fakeTenantClientProvider{err: providerErr},
	}
	_, err := executeGetKubeNodeActivity(t, a, GetKubeNodeInput{NodeID: nodeID})
	require.Error(t, err)
	require.ErrorContains(t, err, "failed to build tenant client for cluster")
	require.ErrorContains(t, err, providerErr.Error())
}

func TestGetKubeNodeReturnsNotFoundFromTenantAPI(t *testing.T) {
	nodeID := uuid.New()
	clusterID := uuid.New()
	node := db.Node{ID: nodeID, ClusterID: clusterID, Name: "missing-worker"}
	cluster := db.Cluster{ID: clusterID, Name: "cluster-a"}

	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, nodeID).Return(node, nil)
	database.On("GetCluster", mock.Anything, clusterID).Return(cluster, nil)

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	tenantClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	a := &Activities{
		db:                   database,
		tenantClientProvider: &fakeTenantClientProvider{tenantClient: tenantClient},
	}

	_, err := executeGetKubeNodeActivity(t, a, GetKubeNodeInput{NodeID: nodeID})
	require.Error(t, err)
	require.ErrorContains(t, err, "failed to get Kubernetes node missing-worker")
	require.ErrorContains(t, err, `nodes "missing-worker" not found`)
}

func executeGetKubeNodeActivity(
	t *testing.T,
	a *Activities,
	input GetKubeNodeInput,
) (GetKubeNodeResult, error) {
	t.Helper()

	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestActivityEnvironment()
	env.RegisterActivity(a.GetKubeNode)

	value, err := env.ExecuteActivity(a.GetKubeNode, input)
	if err != nil {
		return GetKubeNodeResult{}, err
	}

	var result GetKubeNodeResult
	require.NoError(t, value.Get(&result))
	return result, nil
}

type fakeTenantClientProvider struct {
	tenantClient ctrlclient.Client
	err          error

	calls   int
	cluster db.Cluster
}

func (p *fakeTenantClientProvider) ClientForDBCluster(
	_ context.Context,
	cluster db.Cluster,
) (ctrlclient.Client, error) {
	p.calls++
	p.cluster = cluster
	return p.tenantClient, p.err
}
