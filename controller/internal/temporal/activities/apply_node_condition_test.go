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
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func TestApplyNodeConditionPatchesTenantNodeStatusDirectly(t *testing.T) {
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
		WithStatusSubresource(&corev1.Node{}).
		WithObjects(&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: node.Name},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
				{Type: constants.NodeConditionTypeNeedsRecycle, Status: corev1.ConditionFalse, Reason: "OldReason"},
			}},
		}).
		Build()
	provider := &fakeTenantClientProvider{tenantClient: tenantClient}

	a := &Activities{db: database, tenantClientProvider: provider}
	condition := corev1.NodeCondition{
		Type:    constants.NodeConditionTypeNeedsRecycle,
		Status:  corev1.ConditionTrue,
		Reason:  "ImageDrift",
		Message: "the node image differs from the desired image",
	}
	result, err := executeApplyNodeConditionActivity(t, a, ApplyNodeConditionInput{
		NodeID:        nodeID,
		NodeCondition: condition,
	})
	require.NoError(t, err)
	require.Equal(t, "node/worker-0 condition nke.nvidia.com/NeedsRecycle applied", result.Output)

	var updated corev1.Node
	require.NoError(t, tenantClient.Get(context.Background(), client.ObjectKey{Name: node.Name}, &updated))
	require.Equal(t, []corev1.NodeCondition{
		{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
		condition,
	}, updated.Status.Conditions)
	require.Equal(t, 1, provider.calls)
	require.Equal(t, cluster, provider.cluster)
	database.AssertExpectations(t)
}

func TestApplyNodeConditionRequiresTypeBeforeTenantAccess(t *testing.T) {
	nodeID := uuid.New()
	node := db.Node{ID: nodeID, ClusterID: uuid.New(), Name: "worker-0"}
	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, nodeID).Return(node, nil)
	provider := &fakeTenantClientProvider{}

	a := &Activities{db: database, tenantClientProvider: provider}
	_, err := executeApplyNodeConditionActivity(t, a, ApplyNodeConditionInput{NodeID: nodeID})
	require.ErrorContains(t, err, "node condition type is required")
	require.Equal(t, 0, provider.calls)
	database.AssertExpectations(t)
}

func TestApplyNodeConditionClassifiesClusterLookupErrors(t *testing.T) {
	transientErr := errors.New("database connection reset")
	tests := []struct {
		name             string
		clusterErr       error
		wantNonRetryable bool
	}{
		{name: "missing cluster", clusterErr: db.ErrNotFound, wantNonRetryable: true},
		{name: "transient database failure", clusterErr: transientErr, wantNonRetryable: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeID := uuid.New()
			clusterID := uuid.New()
			node := db.Node{ID: nodeID, ClusterID: clusterID, Name: "worker-0"}
			database := &testutil.MockDB{}
			database.On("GetNode", mock.Anything, nodeID).Return(node, nil)
			database.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{}, tt.clusterErr)
			provider := &fakeTenantClientProvider{}

			a := &Activities{db: database, tenantClientProvider: provider}
			_, err := executeApplyNodeConditionActivity(t, a, ApplyNodeConditionInput{
				NodeID:        nodeID,
				NodeCondition: corev1.NodeCondition{Type: constants.NodeConditionTypeNeedsRecycle},
			})
			require.Error(t, err)
			var applicationErr *temporal.ApplicationError
			require.ErrorAs(t, err, &applicationErr)
			require.Equal(t, tt.wantNonRetryable, applicationErr.NonRetryable())
			require.Equal(t, 0, provider.calls)
			database.AssertExpectations(t)
		})
	}
}

func executeApplyNodeConditionActivity(
	t *testing.T,
	a *Activities,
	input ApplyNodeConditionInput,
) (ApplyNodeConditionResult, error) {
	t.Helper()

	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestActivityEnvironment()
	env.RegisterActivity(a.ApplyNodeCondition)

	value, err := env.ExecuteActivity(a.ApplyNodeCondition, input)
	if err != nil {
		return ApplyNodeConditionResult{}, err
	}

	var result ApplyNodeConditionResult
	require.NoError(t, value.Get(&result))
	return result, nil
}
