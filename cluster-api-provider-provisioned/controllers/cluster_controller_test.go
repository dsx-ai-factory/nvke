// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/dsx-ai-factory/nvke/cluster-api-provider-provisioned/api/v1alpha1"
)

// Cluster API waits for this flag before it acts on any machine of the cluster,
// so reporting it is the whole job of this reconciler.
func TestClusterReconcilerReportsProvisioned(t *testing.T) {
	reconciler, k8sClient := newClusterReconcilerForTest(t,
		newInfraCluster(
			infrav1.ProvisionedHost{
				Hostname:  "host-01",
				IPAddress: "10.0.0.11",
			},
			infrav1.ProvisionedHost{
				Hostname:  "host-02",
				IPAddress: "10.0.0.12",
			},
		),
	)

	_, err := reconciler.Reconcile(context.Background(), clusterRequest())
	require.NoError(t, err)

	infraCluster := getInfraCluster(t, k8sClient)
	require.NotNil(t, infraCluster.Status.Initialization.Provisioned)
	require.True(t, *infraCluster.Status.Initialization.Provisioned)
	require.Equal(t, int32(2), infraCluster.Status.AvailableHosts)
	require.Empty(t, infraCluster.Status.AllocatedHosts)
	requireReadyCondition(t, infraCluster.Status.Conditions, metav1.ConditionTrue, infrav1.InventoryValidReason)
}

func TestClusterReconcilerReportsAllocations(t *testing.T) {
	reconciler, k8sClient := newClusterReconcilerForTest(t,
		newInfraCluster(
			infrav1.ProvisionedHost{
				Hostname:  "host-01",
				IPAddress: "10.0.0.11",
			},
			infrav1.ProvisionedHost{
				Hostname:  "host-02",
				IPAddress: "10.0.0.12",
			},
		),
		newInfraMachine(testMachineName, "provisioned://test/10.0.0.12"),
	)

	_, err := reconciler.Reconcile(context.Background(), clusterRequest())
	require.NoError(t, err)

	infraCluster := getInfraCluster(t, k8sClient)
	require.Equal(t, int32(1), infraCluster.Status.AvailableHosts)
	require.Equal(t, []infrav1.HostAllocation{
		{HostName: "host-02", MachineName: testMachineName},
	}, infraCluster.Status.AllocatedHosts)
}

func TestClusterReconcilerRejectsARepeatedHostName(t *testing.T) {
	reconciler, k8sClient := newClusterReconcilerForTest(t,
		newInfraCluster(
			infrav1.ProvisionedHost{
				Hostname:  "host-01",
				IPAddress: "10.0.0.11",
			},
			infrav1.ProvisionedHost{
				Hostname:  "host-01",
				IPAddress: "10.0.0.12",
			},
		),
	)

	_, err := reconciler.Reconcile(context.Background(), clusterRequest())
	require.NoError(t, err)

	infraCluster := getInfraCluster(t, k8sClient)
	require.Nil(t, infraCluster.Status.Initialization.Provisioned)
	requireReadyCondition(t, infraCluster.Status.Conditions, metav1.ConditionFalse, infrav1.InventoryInvalidReason)
}

// A cluster whose machines are already running must keep the provisioned flag,
// or Cluster API stops acting on them.
func TestClusterReconcilerKeepsProvisionedWhenTheInventoryTurnsInvalid(t *testing.T) {
	provisioned := true
	infraCluster := newInfraCluster(
		infrav1.ProvisionedHost{
			Hostname:  "host-01",
			IPAddress: "10.0.0.11",
		},
		infrav1.ProvisionedHost{
			Hostname:  "host-01",
			IPAddress: "10.0.0.12",
		},
	)
	infraCluster.Status.Initialization.Provisioned = &provisioned

	reconciler, k8sClient := newClusterReconcilerForTest(t, infraCluster)

	_, err := reconciler.Reconcile(context.Background(), clusterRequest())
	require.NoError(t, err)

	updated := getInfraCluster(t, k8sClient)
	require.NotNil(t, updated.Status.Initialization.Provisioned)
	require.True(t, *updated.Status.Initialization.Provisioned)
	requireReadyCondition(t, updated.Status.Conditions, metav1.ConditionFalse, infrav1.InventoryInvalidReason)
}

func TestClusterReconcilerSkipsAMissingCluster(t *testing.T) {
	reconciler, _ := newClusterReconcilerForTest(t)

	result, err := reconciler.Reconcile(context.Background(), clusterRequest())
	require.NoError(t, err)
	require.Zero(t, result.RequeueAfter)
}

func TestMachineToCluster(t *testing.T) {
	machine := newInfraMachine(testMachineName, "")

	require.Equal(t, []ctrl.Request{{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: testClusterName},
	}}, machineToCluster(context.Background(), machine))

	unlabelled := newInfraMachine("other", "")
	unlabelled.Labels = nil
	require.Nil(t, machineToCluster(context.Background(), unlabelled))
}

func newClusterReconcilerForTest(t *testing.T, objects ...client.Object) (*ClusterReconciler, client.Client) {
	t.Helper()

	k8sClient := fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithObjects(objects...).
		WithStatusSubresource(&infrav1.ProvisionedMachine{}, &infrav1.ProvisionedCluster{}).
		Build()

	return NewClusterReconciler(k8sClient, slog.New(slog.DiscardHandler)), k8sClient
}

func clusterRequest() ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: testClusterName}}
}

func getInfraCluster(t *testing.T, k8sClient client.Client) *infrav1.ProvisionedCluster {
	t.Helper()

	infraCluster := &infrav1.ProvisionedCluster{}
	require.NoError(t, k8sClient.Get(context.Background(),
		client.ObjectKey{Namespace: testNamespace, Name: testClusterName}, infraCluster))
	return infraCluster
}
