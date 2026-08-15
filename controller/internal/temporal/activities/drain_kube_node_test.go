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
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	temporalactivity "go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func TestDrainKubeNodeCordonsEvictsAndLabelsUsingTenantClient(t *testing.T) {
	controller := true
	fixture := newDrainKubeNodeFixture(t, true,
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "workload",
				OwnerReferences: []metav1.OwnerReference{{
					Kind:       "ReplicaSet",
					Controller: &controller,
				}},
			},
			Spec: corev1.PodSpec{NodeName: "worker-0"},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: kubeSystem,
				Name:      "daemon",
				OwnerReferences: []metav1.OwnerReference{{
					Kind:       daemonSet,
					Controller: &controller,
				}},
			},
			Spec: corev1.PodSpec{NodeName: "worker-0"},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:   kubeSystem,
				Name:        "mirror",
				Annotations: map[string]string{mirrorPodAnnotation: "hash"},
			},
			Spec: corev1.PodSpec{NodeName: "worker-0"},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "other-node"},
			Spec:       corev1.PodSpec{NodeName: "worker-1"},
		},
	)

	require.NoError(t, executeDrainKubeNodeActivity(t, fixture.activities, fixture.input))

	var node corev1.Node
	require.NoError(t, fixture.tenantClient.Get(context.Background(), client.ObjectKey{Name: fixture.node.Name}, &node))
	require.True(t, node.Spec.Unschedulable)
	require.Equal(t, trueValue, node.Labels[drainedNodeLabel])

	assertPodNotFound(t, fixture.tenantClient, "default", "workload")
	assertPodExists(t, fixture.tenantClient, kubeSystem, "daemon")
	assertPodExists(t, fixture.tenantClient, kubeSystem, "mirror")
	assertPodExists(t, fixture.tenantClient, "default", "other-node")
	require.Equal(t, 1, fixture.provider.calls)
	require.Equal(t, fixture.cluster, fixture.provider.cluster)
	fixture.database.AssertExpectations(t)
}

func TestDrainKubeNodeForceDeletesAllRemainingPodsAfterGracefulFailure(t *testing.T) {
	controller := true
	fixture := newDrainKubeNodeFixture(t, true,
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "unmanaged"},
			Spec:       corev1.PodSpec{NodeName: "worker-0"},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: kubeSystem,
				Name:      "daemon",
				OwnerReferences: []metav1.OwnerReference{{
					Kind:       daemonSet,
					Controller: &controller,
				}},
			},
			Spec: corev1.PodSpec{NodeName: "worker-0"},
		},
	)
	fixture.input.ForceDrainOnFailure = true

	require.NoError(t, executeDrainKubeNodeActivity(t, fixture.activities, fixture.input))

	assertPodNotFound(t, fixture.tenantClient, "default", "unmanaged")
	assertPodNotFound(t, fixture.tenantClient, kubeSystem, "daemon")
	var node corev1.Node
	require.NoError(t, fixture.tenantClient.Get(context.Background(), client.ObjectKey{Name: fixture.node.Name}, &node))
	require.True(t, node.Spec.Unschedulable)
	require.Equal(t, trueValue, node.Labels[drainedNodeLabel])
}

func TestDrainKubeNodeDoesNotForceDeleteControlPlanePods(t *testing.T) {
	fixture := newDrainKubeNodeFixture(t, true, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "unmanaged"},
		Spec:       corev1.PodSpec{NodeName: "worker-0"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	})

	err := executeDrainKubeNodeActivity(t, fixture.activities, fixture.input)
	require.Error(t, err)
	require.ErrorContains(t, err, "pod is not managed by a controller")

	assertPodExists(t, fixture.tenantClient, "default", "unmanaged")
	var node corev1.Node
	require.NoError(t, fixture.tenantClient.Get(context.Background(), client.ObjectKey{Name: fixture.node.Name}, &node))
	require.True(t, node.Spec.Unschedulable)
	require.NotContains(t, node.Labels, drainedNodeLabel)
}

func TestDrainKubeNodeRetriesPodDisruptionBudgetRejection(t *testing.T) {
	controller := true
	fixture := newDrainKubeNodeFixture(t, true, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "workload",
			OwnerReferences: []metav1.OwnerReference{{
				Kind:       "ReplicaSet",
				Controller: &controller,
			}},
		},
		Spec: corev1.PodSpec{NodeName: "worker-0"},
	})

	evictionAttempts := 0
	fixture.tenantClient = interceptor.NewClient(fixture.tenantClient, interceptor.Funcs{
		SubResourceCreate: func(
			ctx context.Context,
			underlying client.Client,
			subResourceName string,
			obj client.Object,
			subResource client.Object,
			opts ...client.SubResourceCreateOption,
		) error {
			evictionAttempts++
			eviction, ok := subResource.(*policyv1.Eviction)
			require.True(t, ok)
			require.Equal(t, drainGracePeriodSeconds, *eviction.DeleteOptions.GracePeriodSeconds)
			if evictionAttempts == 1 {
				return apierrors.NewTooManyRequests("pod disruption budget denied eviction", 0)
			}
			return underlying.SubResource(subResourceName).Create(ctx, obj, subResource, opts...)
		},
	})
	fixture.provider.tenantClient = fixture.tenantClient

	require.NoError(t, executeDrainKubeNodeActivity(t, fixture.activities, fixture.input))
	require.Equal(t, 2, evictionAttempts)
	assertPodNotFound(t, fixture.tenantClient, "default", "workload")
}

func TestDrainKubeNodeSucceedsWhenTenantNodeIsAlreadyAbsent(t *testing.T) {
	fixture := newDrainKubeNodeFixture(t, false)

	require.NoError(t, executeDrainKubeNodeActivity(t, fixture.activities, fixture.input))
	require.Equal(t, 1, fixture.provider.calls)
	fixture.database.AssertExpectations(t)
}

type drainKubeNodeFixture struct {
	activities   *Activities
	tenantClient client.WithWatch
	provider     *fakeTenantClientProvider
	database     *testutil.MockDB
	node         db.Node
	cluster      db.Cluster
	input        DrainKubeNodeInput
}

func newDrainKubeNodeFixture(t *testing.T, includeKubeNode bool, pods ...*corev1.Pod) drainKubeNodeFixture {
	t.Helper()

	nodeID := uuid.New()
	clusterID := uuid.New()
	node := db.Node{ID: nodeID, ClusterID: clusterID, Name: "worker-0"}
	cluster := db.Cluster{ID: clusterID, Name: "cluster-a"}

	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, nodeID).Return(node, nil)
	database.On("GetCluster", mock.Anything, clusterID).Return(cluster, nil)

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, policyv1.AddToScheme(scheme))

	objects := make([]client.Object, 0, len(pods)+1)
	if includeKubeNode {
		objects = append(objects, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node.Name}})
	}
	for _, pod := range pods {
		objects = append(objects, pod)
	}

	tenantClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithIndex(&corev1.Pod{}, podNodeNameField, func(obj client.Object) []string {
			return []string{obj.(*corev1.Pod).Spec.NodeName}
		}).
		Build()
	provider := &fakeTenantClientProvider{tenantClient: tenantClient}

	return drainKubeNodeFixture{
		activities: &Activities{
			db:                   database,
			tenantClientProvider: provider,
		},
		tenantClient: tenantClient,
		provider:     provider,
		database:     database,
		node:         node,
		cluster:      cluster,
		input:        DrainKubeNodeInput{NodeID: nodeID},
	}
}

func executeDrainKubeNodeActivity(
	t *testing.T,
	a *Activities,
	input DrainKubeNodeInput,
) error {
	t.Helper()

	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestActivityEnvironment()
	testActivity := func(ctx context.Context, input DrainKubeNodeInput) error {
		return a.drainKubeNode(ctx, input, time.Millisecond, time.Second)
	}
	env.RegisterActivityWithOptions(testActivity, temporalactivity.RegisterOptions{Name: "TestDrainKubeNode"})

	_, err := env.ExecuteActivity("TestDrainKubeNode", input)
	return err
}

func assertPodExists(t *testing.T, tenantClient client.Client, namespace, name string) {
	t.Helper()
	var pod corev1.Pod
	require.NoError(t, tenantClient.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, &pod))
}

func assertPodNotFound(t *testing.T, tenantClient client.Client, namespace, name string) {
	t.Helper()
	var pod corev1.Pod
	err := tenantClient.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, &pod)
	require.True(t, apierrors.IsNotFound(err))
}
