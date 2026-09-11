// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func TestLabelNodePatchesTenantNodeDirectly(t *testing.T) {
	nodeID := uuid.New()
	clusterID := uuid.New()
	node := db.Node{
		ID:           nodeID,
		ClusterID:    clusterID,
		Name:         "worker-0",
		ControlPlane: true,
	}
	cluster := db.Cluster{ID: clusterID, Name: "cluster-a"}
	cluster.SetNicoProvider(db.NicoClusterProvider{OrgID: "test-org", SiteID: uuid.New()})

	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, nodeID).Return(node, nil)
	database.On("GetCluster", mock.Anything, clusterID).Return(cluster, nil).Twice()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	tenantClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: node.Name,
				Labels: map[string]string{
					"keep":    "unchanged",
					"replace": "old",
					"delete":  "value",
				},
			},
		}).
		Build()
	provider := &fakeTenantClientProvider{tenantClient: tenantClient}

	a := &Activities{
		db:                   database,
		tenantClientProvider: provider,
	}
	result, err := executeLabelNodeActivity(t, a, LabelNodeInput{
		NodeID: nodeID,
		Labels: []string{
			"new=value",
			"replace=new",
			"delete-",
		},
	})
	require.NoError(t, err)
	require.Equal(t, "node/worker-0 labeled", result.Output)

	var updated corev1.Node
	require.NoError(t, tenantClient.Get(context.Background(), client.ObjectKey{Name: node.Name}, &updated))
	require.Equal(t, map[string]string{
		"keep":    "unchanged",
		"new":     "value",
		"replace": "new",
	}, updated.Labels)
	require.Equal(t, 1, provider.calls)
	require.Equal(t, cluster, provider.cluster)
	database.AssertExpectations(t)
}

func TestLabelNodeRejectsInvalidLabelBeforeTenantAccess(t *testing.T) {
	nodeID := uuid.New()
	clusterID := uuid.New()
	node := db.Node{
		ID:           nodeID,
		ClusterID:    clusterID,
		Name:         "worker-0",
		ControlPlane: true,
	}
	cluster := db.Cluster{ID: clusterID, Name: "cluster-a"}
	cluster.SetNicoProvider(db.NicoClusterProvider{OrgID: "test-org", SiteID: uuid.New()})

	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, nodeID).Return(node, nil)
	database.On("GetCluster", mock.Anything, clusterID).Return(cluster, nil).Once()
	provider := &fakeTenantClientProvider{}

	a := &Activities{
		db:                   database,
		tenantClientProvider: provider,
	}
	_, err := executeLabelNodeActivity(t, a, LabelNodeInput{
		NodeID: nodeID,
		Labels: []string{"missing-operation"},
	})
	require.Error(t, err)
	require.ErrorContains(t, err, `invalid label "missing-operation": expected key=value or key-`)
	require.Equal(t, 0, provider.calls)
	database.AssertExpectations(t)
}

func executeLabelNodeActivity(
	t *testing.T,
	a *Activities,
	input LabelNodeInput,
) (LabelNodeResult, error) {
	t.Helper()

	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestActivityEnvironment()
	env.RegisterActivity(a.LabelNode)

	value, err := env.ExecuteActivity(a.LabelNode, input)
	if err != nil {
		return LabelNodeResult{}, err
	}

	var result LabelNodeResult
	require.NoError(t, value.Get(&result))
	return result, nil
}
