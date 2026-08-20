// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/NVIDIA/nke/controller/preprovisioned/api/v1alpha1"
)

const (
	testNamespace   = "cluster-test"
	testClusterName = "test"
	testMachineName = "test-cp-a1b2"
)

func TestMachineReconcilerBindsFirstFreeHost(t *testing.T) {
	reconciler, k8sClient := newMachineReconcilerForTest(t,
		newInfraCluster(
			infrav1.PreprovisionedHost{
				Name:      "host-01",
				Addresses: []clusterv1.MachineAddress{{Type: clusterv1.MachineInternalIP, Address: "10.0.0.11"}},
			},
		),
		newCAPICluster(),
		newOwnerMachine(false),
		newInfraMachine(testMachineName, ""),
	)

	_, err := reconciler.Reconcile(context.Background(), machineRequest(testMachineName))
	require.NoError(t, err)

	machine := getInfraMachine(t, k8sClient, testMachineName)
	require.Equal(t, "preprovisioned://test/host-01", machine.Spec.ProviderID)
	require.Equal(t, "host-01", machine.Status.HostName)
	require.Equal(t, []clusterv1.MachineAddress{
		{Type: clusterv1.MachineInternalIP, Address: "10.0.0.11"},
	}, machine.Status.Addresses)
	require.NotNil(t, machine.Status.Initialization.Provisioned)
	require.True(t, *machine.Status.Initialization.Provisioned)
	requireReadyCondition(t, machine.Status.Conditions, metav1.ConditionTrue, infrav1.HostAllocatedReason)
}

// Two machines must never share a host. The second machine sees the claim of
// the first because the used set comes from the provider IDs that exist.
func TestMachineReconcilerDoesNotReuseAHeldHost(t *testing.T) {
	reconciler, k8sClient := newMachineReconcilerForTest(t,
		newInfraCluster(
			infrav1.PreprovisionedHost{
				Name:      "host-01",
				Addresses: []clusterv1.MachineAddress{{Type: clusterv1.MachineInternalIP, Address: "10.0.0.11"}},
			},
			infrav1.PreprovisionedHost{
				Name:      "host-02",
				Addresses: []clusterv1.MachineAddress{{Type: clusterv1.MachineInternalIP, Address: "10.0.0.12"}},
			},
		),
		newCAPICluster(),
		newOwnerMachine(false),
		newOwnerMachineNamed("second", false),
		newInfraMachine(testMachineName, ""),
		newInfraMachine("second", ""),
	)

	_, err := reconciler.Reconcile(context.Background(), machineRequest(testMachineName))
	require.NoError(t, err)
	_, err = reconciler.Reconcile(context.Background(), machineRequest("second"))
	require.NoError(t, err)

	first := getInfraMachine(t, k8sClient, testMachineName)
	second := getInfraMachine(t, k8sClient, "second")
	require.Equal(t, "preprovisioned://test/host-01", first.Spec.ProviderID)
	require.Equal(t, "preprovisioned://test/host-02", second.Spec.ProviderID)
}

func TestMachineReconcilerKeepsAnExistingBinding(t *testing.T) {
	reconciler, k8sClient := newMachineReconcilerForTest(t,
		newInfraCluster(
			infrav1.PreprovisionedHost{
				Name:      "host-01",
				Addresses: []clusterv1.MachineAddress{{Type: clusterv1.MachineInternalIP, Address: "10.0.0.11"}},
			},
			infrav1.PreprovisionedHost{
				Name:      "host-02",
				Addresses: []clusterv1.MachineAddress{{Type: clusterv1.MachineInternalIP, Address: "10.0.0.12"}},
			},
		),
		newCAPICluster(),
		newOwnerMachine(false),
		newInfraMachine(testMachineName, "preprovisioned://test/host-02"),
	)

	_, err := reconciler.Reconcile(context.Background(), machineRequest(testMachineName))
	require.NoError(t, err)

	machine := getInfraMachine(t, k8sClient, testMachineName)
	require.Equal(t, "preprovisioned://test/host-02", machine.Spec.ProviderID)
	require.Equal(t, "host-02", machine.Status.HostName)
	require.Equal(t, "10.0.0.12", machine.Status.Addresses[0].Address)
}

func TestMachineReconcilerHonoursControlPlaneRole(t *testing.T) {
	reconciler, k8sClient := newMachineReconcilerForTest(t,
		newInfraCluster(
			infrav1.PreprovisionedHost{
				Name:      "worker-01",
				Role:      infrav1.HostRoleWorker,
				Addresses: []clusterv1.MachineAddress{{Type: clusterv1.MachineInternalIP, Address: "10.0.0.21"}},
			},
			infrav1.PreprovisionedHost{
				Name:      "cp-01",
				Role:      infrav1.HostRoleControlPlane,
				Addresses: []clusterv1.MachineAddress{{Type: clusterv1.MachineInternalIP, Address: "10.0.0.11"}},
			},
		),
		newCAPICluster(),
		newOwnerMachine(true),
		newInfraMachine(testMachineName, ""),
	)

	_, err := reconciler.Reconcile(context.Background(), machineRequest(testMachineName))
	require.NoError(t, err)

	machine := getInfraMachine(t, k8sClient, testMachineName)
	require.Equal(t, "preprovisioned://test/cp-01", machine.Spec.ProviderID)
}

func TestMachineReconcilerReportsAnExhaustedInventory(t *testing.T) {
	reconciler, k8sClient := newMachineReconcilerForTest(t,
		newInfraCluster(
			infrav1.PreprovisionedHost{
				Name:      "host-01",
				Addresses: []clusterv1.MachineAddress{{Type: clusterv1.MachineInternalIP, Address: "10.0.0.11"}},
			},
		),
		newCAPICluster(),
		newOwnerMachine(false),
		newOwnerMachineNamed("second", false),
		newInfraMachine(testMachineName, "preprovisioned://test/host-01"),
		newInfraMachine("second", ""),
	)

	result, err := reconciler.Reconcile(context.Background(), machineRequest("second"))
	require.NoError(t, err)
	require.Equal(t, retryAfter, result.RequeueAfter)

	machine := getInfraMachine(t, k8sClient, "second")
	require.Empty(t, machine.Spec.ProviderID)
	require.Nil(t, machine.Status.Initialization.Provisioned)
	requireReadyCondition(t, machine.Status.Conditions, metav1.ConditionFalse, infrav1.NoHostAvailableReason)
}

// The role of a machine is unknown until Cluster API sets the owner reference,
// so the machine must not take a host that a control plane machine may need.
func TestMachineReconcilerWaitsForTheOwnerMachine(t *testing.T) {
	infraMachine := newInfraMachine(testMachineName, "")
	infraMachine.OwnerReferences = nil

	reconciler, k8sClient := newMachineReconcilerForTest(t,
		newInfraCluster(
			infrav1.PreprovisionedHost{
				Name:      "host-01",
				Addresses: []clusterv1.MachineAddress{{Type: clusterv1.MachineInternalIP, Address: "10.0.0.11"}},
			},
		),
		newCAPICluster(),
		infraMachine,
	)

	_, err := reconciler.Reconcile(context.Background(), machineRequest(testMachineName))
	require.NoError(t, err)

	machine := getInfraMachine(t, k8sClient, testMachineName)
	require.Empty(t, machine.Spec.ProviderID)
	requireReadyCondition(t, machine.Status.Conditions, metav1.ConditionFalse, infrav1.WaitingForOwnerReason)
}

func TestMachineReconcilerWaitsForTheInfraCluster(t *testing.T) {
	reconciler, k8sClient := newMachineReconcilerForTest(t,
		newCAPICluster(),
		newOwnerMachine(false),
		newInfraMachine(testMachineName, ""),
	)

	result, err := reconciler.Reconcile(context.Background(), machineRequest(testMachineName))
	require.NoError(t, err)
	require.Equal(t, retryAfter, result.RequeueAfter)

	machine := getInfraMachine(t, k8sClient, testMachineName)
	require.Empty(t, machine.Spec.ProviderID)
	requireReadyCondition(t, machine.Status.Conditions, metav1.ConditionFalse, infrav1.WaitingForClusterReason)
}

// An operator removed a host from the inventory while a node still runs on it.
// Releasing the host would hand a running machine to a second one, so the
// binding stays and the problem is reported instead.
func TestMachineReconcilerKeepsABindingToAMissingHost(t *testing.T) {
	reconciler, k8sClient := newMachineReconcilerForTest(t,
		newInfraCluster(
			infrav1.PreprovisionedHost{
				Name:      "host-02",
				Addresses: []clusterv1.MachineAddress{{Type: clusterv1.MachineInternalIP, Address: "10.0.0.12"}},
			},
		),
		newCAPICluster(),
		newOwnerMachine(false),
		newInfraMachine(testMachineName, "preprovisioned://test/host-01"),
	)

	_, err := reconciler.Reconcile(context.Background(), machineRequest(testMachineName))
	require.NoError(t, err)

	machine := getInfraMachine(t, k8sClient, testMachineName)
	require.Equal(t, "preprovisioned://test/host-01", machine.Spec.ProviderID)
	requireReadyCondition(t, machine.Status.Conditions, metav1.ConditionFalse, infrav1.HostNotFoundReason)
}

func TestMachineReconcilerSkipsADeletedMachine(t *testing.T) {
	deleting := newInfraMachine(testMachineName, "")
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	deleting.Finalizers = []string{"test.finalizer"}

	reconciler, k8sClient := newMachineReconcilerForTest(t,
		newInfraCluster(
			infrav1.PreprovisionedHost{
				Name:      "host-01",
				Addresses: []clusterv1.MachineAddress{{Type: clusterv1.MachineInternalIP, Address: "10.0.0.11"}},
			},
		),
		newCAPICluster(),
		newOwnerMachine(false),
		deleting,
	)

	_, err := reconciler.Reconcile(context.Background(), machineRequest(testMachineName))
	require.NoError(t, err)

	machine := getInfraMachine(t, k8sClient, testMachineName)
	require.Empty(t, machine.Spec.ProviderID)
}

func TestMachineReconcilerRejectsAnInfraClusterOfAnotherProvider(t *testing.T) {
	capiCluster := newCAPICluster()
	capiCluster.Spec.InfrastructureRef.Kind = "NicoCluster"

	reconciler, _ := newMachineReconcilerForTest(t,
		capiCluster,
		newOwnerMachine(false),
		newInfraMachine(testMachineName, ""),
	)

	_, err := reconciler.Reconcile(context.Background(), machineRequest(testMachineName))
	require.ErrorContains(t, err, "not PreprovisionedCluster")
}

func newMachineReconcilerForTest(t *testing.T, objects ...client.Object) (*MachineReconciler, client.Client) {
	t.Helper()

	k8sClient := fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithObjects(objects...).
		WithStatusSubresource(&infrav1.PreprovisionedMachine{}, &infrav1.PreprovisionedCluster{}).
		Build()

	return NewMachineReconciler(k8sClient, slog.New(slog.DiscardHandler)), k8sClient
}

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, clusterv1.AddToScheme(scheme))
	require.NoError(t, infrav1.AddToScheme(scheme))
	return scheme
}

func newInfraCluster(hosts ...infrav1.PreprovisionedHost) *infrav1.PreprovisionedCluster {
	return &infrav1.PreprovisionedCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: infrav1.GroupVersion.String(),
			Kind:       "PreprovisionedCluster",
		},
		ObjectMeta: metav1.ObjectMeta{Name: testClusterName, Namespace: testNamespace},
		Spec:       infrav1.PreprovisionedClusterSpec{Hosts: hosts},
	}
}

func newCAPICluster() *clusterv1.Cluster {
	return &clusterv1.Cluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "Cluster",
		},
		ObjectMeta: metav1.ObjectMeta{Name: testClusterName, Namespace: testNamespace},
		Spec: clusterv1.ClusterSpec{
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: infrav1.GroupName,
				Kind:     "PreprovisionedCluster",
				Name:     testClusterName,
			},
		},
	}
}

func newOwnerMachine(controlPlane bool) *clusterv1.Machine {
	return newOwnerMachineNamed(testMachineName, controlPlane)
}

func newOwnerMachineNamed(name string, controlPlane bool) *clusterv1.Machine {
	labels := map[string]string{clusterv1.ClusterNameLabel: testClusterName}
	if controlPlane {
		labels[clusterv1.MachineControlPlaneLabel] = ""
	}

	return &clusterv1.Machine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "Machine",
		},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Labels: labels},
		Spec:       clusterv1.MachineSpec{ClusterName: testClusterName},
	}
}

func newInfraMachine(name, providerID string) *infrav1.PreprovisionedMachine {
	isController := true
	return &infrav1.PreprovisionedMachine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: infrav1.GroupVersion.String(),
			Kind:       "PreprovisionedMachine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
			Labels:    map[string]string{clusterv1.ClusterNameLabel: testClusterName},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: clusterv1.GroupVersion.String(),
				Kind:       "Machine",
				Name:       name,
				Controller: &isController,
			}},
		},
		Spec: infrav1.PreprovisionedMachineSpec{ProviderID: providerID},
	}
}

func machineRequest(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: name}}
}

func getInfraMachine(t *testing.T, k8sClient client.Client, name string) *infrav1.PreprovisionedMachine {
	t.Helper()

	machine := &infrav1.PreprovisionedMachine{}
	require.NoError(t, k8sClient.Get(context.Background(),
		client.ObjectKey{Namespace: testNamespace, Name: name}, machine))
	return machine
}

func requireReadyCondition(t *testing.T, conditions []metav1.Condition, status metav1.ConditionStatus, reason string) {
	t.Helper()

	for _, condition := range conditions {
		if condition.Type != infrav1.ReadyCondition {
			continue
		}
		require.Equal(t, status, condition.Status)
		require.Equal(t, reason, condition.Reason)
		return
	}
	t.Fatalf("no %s condition found in %v", infrav1.ReadyCondition, conditions)
}
