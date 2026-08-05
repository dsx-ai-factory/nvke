// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package activities

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func TestDeleteKubeNodeDeletesTenantNodeDirectly(t *testing.T) {
	nodeID := uuid.New()
	clusterID := uuid.New()
	node := db.Node{ID: nodeID, ClusterID: clusterID, Name: "worker-0"}
	cluster := db.Cluster{ID: clusterID, Name: "cluster-a"}

	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, nodeID).Return(node, nil)
	database.On("GetCluster", mock.Anything, clusterID).Return(cluster, nil)

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	tenantClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node.Name}}).
		Build()
	provider := &fakeTenantClientProvider{tenantClient: tenantClient}
	a := &Activities{db: database, tenantClientProvider: provider}

	require.NoError(t, executeDeleteKubeNodeActivity(t, a, DeleteKubeNodeInput{NodeID: nodeID}))

	var deleted corev1.Node
	err := tenantClient.Get(context.Background(), client.ObjectKey{Name: node.Name}, &deleted)
	require.True(t, apierrors.IsNotFound(err))
	require.Equal(t, 1, provider.calls)
	require.Equal(t, cluster, provider.cluster)
	database.AssertExpectations(t)
}

func TestDeleteKubeNodeSucceedsWhenTenantNodeIsAlreadyAbsent(t *testing.T) {
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
	provider := &fakeTenantClientProvider{tenantClient: tenantClient}
	a := &Activities{db: database, tenantClientProvider: provider}

	require.NoError(t, executeDeleteKubeNodeActivity(t, a, DeleteKubeNodeInput{NodeID: nodeID}))
	require.Equal(t, 1, provider.calls)
	require.Equal(t, cluster, provider.cluster)
	database.AssertExpectations(t)
}

func executeDeleteKubeNodeActivity(t *testing.T, a *Activities, input DeleteKubeNodeInput) error {
	t.Helper()

	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestActivityEnvironment()
	env.RegisterActivity(a.DeleteKubeNode)

	_, err := env.ExecuteActivity(a.DeleteKubeNode, input)
	return err
}
