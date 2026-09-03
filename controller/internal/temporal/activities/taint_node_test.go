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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func TestTaintNodePatchesTenantNodeDirectly(t *testing.T) {
	nodeID := uuid.New()
	clusterID := uuid.New()
	node := db.Node{ID: nodeID, ClusterID: clusterID, Name: "worker-0"}
	cluster := db.Cluster{ID: clusterID, Name: "cluster-a"}

	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, nodeID).Return(node, nil).Twice()
	database.On("GetCluster", mock.Anything, clusterID).Return(cluster, nil)

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	tenantClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: node.Name},
			Spec: corev1.NodeSpec{Taints: []corev1.Taint{
				{Key: "keep", Value: "unchanged", Effect: corev1.TaintEffectNoExecute},
				{Key: "nke.nvidia.com/provisioning", Value: "old", Effect: corev1.TaintEffectNoSchedule},
			}},
		}).
		Build()
	provider := &fakeTenantClientProvider{tenantClient: tenantClient}
	a := &Activities{db: database, tenantClientProvider: provider}

	result, err := executeTaintNodeActivity(t, a, TaintNodeInput{
		NodeID:      nodeID,
		TaintKey:    "nke.nvidia.com/provisioning",
		TaintValue:  "true",
		TaintEffect: string(corev1.TaintEffectNoSchedule),
	})
	require.NoError(t, err)
	require.Equal(t, "taint nke.nvidia.com/provisioning=true:NoSchedule applied to node/worker-0", result.Output)

	var updated corev1.Node
	require.NoError(t, tenantClient.Get(context.Background(), client.ObjectKey{Name: node.Name}, &updated))
	require.Equal(t, []corev1.Taint{
		{Key: "keep", Value: "unchanged", Effect: corev1.TaintEffectNoExecute},
		{Key: "nke.nvidia.com/provisioning", Value: "true", Effect: corev1.TaintEffectNoSchedule},
	}, updated.Spec.Taints)
	require.Equal(t, 1, provider.calls)
	require.Equal(t, cluster, provider.cluster)
	database.AssertExpectations(t)
}

func TestTaintNodeRemovesTaintIdempotently(t *testing.T) {
	nodeID := uuid.New()
	clusterID := uuid.New()
	node := db.Node{ID: nodeID, ClusterID: clusterID, Name: "worker-0"}
	cluster := db.Cluster{ID: clusterID, Name: "cluster-a"}

	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, nodeID).Return(node, nil).Times(4)
	database.On("GetCluster", mock.Anything, clusterID).Return(cluster, nil).Twice()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	tenantClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: node.Name},
			Spec: corev1.NodeSpec{Taints: []corev1.Taint{
				{Key: "keep", Value: "unchanged", Effect: corev1.TaintEffectNoExecute},
				{Key: "nke.nvidia.com/provisioning", Value: "different", Effect: corev1.TaintEffectNoSchedule},
			}},
		}).
		Build()
	provider := &fakeTenantClientProvider{tenantClient: tenantClient}
	a := &Activities{db: database, tenantClientProvider: provider}
	input := TaintNodeInput{
		NodeID:      nodeID,
		TaintKey:    "nke.nvidia.com/provisioning",
		TaintEffect: string(corev1.TaintEffectNoSchedule) + "-",
	}

	for range 2 {
		result, err := executeTaintNodeActivity(t, a, input)
		require.NoError(t, err)
		require.Equal(t, "taint nke.nvidia.com/provisioning:NoSchedule removed from node/worker-0", result.Output)
	}

	var updated corev1.Node
	require.NoError(t, tenantClient.Get(context.Background(), client.ObjectKey{Name: node.Name}, &updated))
	require.Equal(t, []corev1.Taint{
		{Key: "keep", Value: "unchanged", Effect: corev1.TaintEffectNoExecute},
	}, updated.Spec.Taints)
	require.Equal(t, 2, provider.calls)
	require.Equal(t, cluster, provider.cluster)
	database.AssertExpectations(t)
}

func TestTaintNodeRetriesConflictAndPreservesConcurrentTaint(t *testing.T) {
	nodeID := uuid.New()
	clusterID := uuid.New()
	node := db.Node{ID: nodeID, ClusterID: clusterID, Name: "worker-0"}
	cluster := db.Cluster{ID: clusterID, Name: "cluster-a"}

	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, nodeID).Return(node, nil).Twice()
	database.On("GetCluster", mock.Anything, clusterID).Return(cluster, nil)

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	baseClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node.Name}}).
		Build()
	patchAttempts := 0
	tenantClient := interceptor.NewClient(baseClient, interceptor.Funcs{
		Patch: func(
			ctx context.Context,
			underlying client.WithWatch,
			obj client.Object,
			patch client.Patch,
			opts ...client.PatchOption,
		) error {
			patchAttempts++
			if patchAttempts == 1 {
				var concurrent corev1.Node
				require.NoError(t, underlying.Get(ctx, client.ObjectKey{Name: node.Name}, &concurrent))
				concurrent.Spec.Taints = append(concurrent.Spec.Taints, corev1.Taint{
					Key: "concurrent", Value: "preserved", Effect: corev1.TaintEffectNoExecute,
				})
				require.NoError(t, underlying.Update(ctx, &concurrent))
				return apierrors.NewConflict(schema.GroupResource{Resource: "nodes"}, node.Name, errors.New("concurrent update"))
			}
			return underlying.Patch(ctx, obj, patch, opts...)
		},
	})
	provider := &fakeTenantClientProvider{tenantClient: tenantClient}
	a := &Activities{db: database, tenantClientProvider: provider}

	_, err := executeTaintNodeActivity(t, a, TaintNodeInput{
		NodeID:      nodeID,
		TaintKey:    "nke.nvidia.com/provisioning",
		TaintValue:  "true",
		TaintEffect: string(corev1.TaintEffectNoSchedule),
	})
	require.NoError(t, err)
	require.Equal(t, 2, patchAttempts)

	var updated corev1.Node
	require.NoError(t, tenantClient.Get(context.Background(), client.ObjectKey{Name: node.Name}, &updated))
	require.ElementsMatch(t, []corev1.Taint{
		{Key: "concurrent", Value: "preserved", Effect: corev1.TaintEffectNoExecute},
		{Key: "nke.nvidia.com/provisioning", Value: "true", Effect: corev1.TaintEffectNoSchedule},
	}, updated.Spec.Taints)
	require.Equal(t, 1, provider.calls)
	database.AssertExpectations(t)
}

func TestTaintNodeRejectsInvalidTaintBeforeTenantAccess(t *testing.T) {
	provider := &fakeTenantClientProvider{}
	a := &Activities{tenantClientProvider: provider}

	_, err := executeTaintNodeActivity(t, a, TaintNodeInput{
		NodeID:      uuid.New(),
		TaintKey:    "invalid key",
		TaintValue:  "true",
		TaintEffect: string(corev1.TaintEffectNoSchedule),
	})
	require.ErrorContains(t, err, `invalid taint key "invalid key"`)
	require.Equal(t, 0, provider.calls)
}

func executeTaintNodeActivity(t *testing.T, a *Activities, input TaintNodeInput) (TaintNodeResult, error) {
	t.Helper()

	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestActivityEnvironment()
	env.RegisterActivity(a.TaintNode)

	value, err := env.ExecuteActivity(a.TaintNode, input)
	if err != nil {
		return TaintNodeResult{}, err
	}

	var result TaintNodeResult
	require.NoError(t, value.Get(&result))
	return result, nil
}
