// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/dsx-ai-factory/nke/cluster-api-provider-provisioned/api/v1alpha1"
)

func TestMachineReconcilerBindsFirstFreeHost(t *testing.T) {
	tpmHash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	reconciler, k8sClient := newMachineReconcilerForTest(t,
		newInfraCluster(
			infrav1.ProvisionedHost{
				Hostname:     "host-01",
				IPAddress:    "10.0.0.11",
				TPMEKPubHash: tpmHash,
			},
		),
		newCAPICluster(),
		newOwnerMachine(),
		newInfraMachine(testMachineName, ""),
	)

	_, err := reconciler.Reconcile(context.Background(), machineRequest(testMachineName))
	require.NoError(t, err)

	machine := getInfraMachine(t, k8sClient, testMachineName)
	require.Equal(t, "provisioned://test/10.0.0.11", machine.Spec.ProviderID)
	require.Equal(t, "host-01", machine.Status.HostName)
	require.Equal(t, []clusterv1.MachineAddress{
		{Type: clusterv1.MachineHostName, Address: "host-01"},
		{Type: clusterv1.MachineInternalIP, Address: "10.0.0.11"},
	}, machine.Status.Addresses)
	require.NotNil(t, machine.Status.Initialization.Provisioned)
	require.True(t, *machine.Status.Initialization.Provisioned)
	requireReadyCondition(t, machine.Status.Conditions, metav1.ConditionTrue, infrav1.HostAllocatedReason)

	owner := &clusterv1.Machine{}
	require.NoError(t, k8sClient.Get(context.Background(),
		client.ObjectKey{Namespace: testNamespace, Name: testMachineName}, owner))
	require.Equal(t, tpmSpireAgentPathPrefix+tpmHash,
		owner.Annotations[machineSpireAgentIDPathAnnotation])
}

// Two machines must never share a host. The second machine sees the claim of
// the first because the used set comes from the provider IDs that exist.
func TestMachineReconcilerDoesNotReuseAHeldHost(t *testing.T) {
	reconciler, k8sClient := newMachineReconcilerForTest(t,
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
		newCAPICluster(),
		newOwnerMachine(),
		newOwnerMachineNamed("second"),
		newInfraMachine(testMachineName, ""),
		newInfraMachine("second", ""),
	)

	_, err := reconciler.Reconcile(context.Background(), machineRequest(testMachineName))
	require.NoError(t, err)
	_, err = reconciler.Reconcile(context.Background(), machineRequest("second"))
	require.NoError(t, err)

	first := getInfraMachine(t, k8sClient, testMachineName)
	second := getInfraMachine(t, k8sClient, "second")
	require.Equal(t, "provisioned://test/10.0.0.11", first.Spec.ProviderID)
	require.Equal(t, "provisioned://test/10.0.0.12", second.Spec.ProviderID)
}

// Overlap should cost a host, not the node: the machine moves to the next one.
func TestMachineReconcilerSkipsAHostLockedByAnotherCluster(t *testing.T) {
	first := hostWithAddress("host-01", "10.0.0.11")
	second := hostWithAddress("host-02", "10.0.0.12")
	reconciler, k8sClient := newMachineReconcilerForTest(t,
		newInfraCluster(first, second), newCAPICluster(), newOwnerMachine(), newInfraMachine(testMachineName, ""),
	)

	other := &infrav1.ProvisionedMachine{ObjectMeta: metav1.ObjectMeta{
		Name: "other-cp-1", Namespace: otherNamespace, UID: types.UID("uid-other-cp-1"),
	}}
	require.NoError(t, k8sClient.Create(context.Background(), other))
	requireClaimAcquired(t, reconciler, other, "other", first)

	_, err := reconciler.Reconcile(context.Background(), machineRequest(testMachineName))
	require.NoError(t, err)
	require.Equal(t, "provisioned://test/10.0.0.12",
		getInfraMachine(t, k8sClient, testMachineName).Spec.ProviderID)
}

func TestMachineReconcilerReportsWhenEveryHostIsLockedElsewhere(t *testing.T) {
	host := hostWithAddress("host-01", "10.0.0.11")
	reconciler, k8sClient := newMachineReconcilerForTest(t,
		newInfraCluster(host), newCAPICluster(), newOwnerMachine(), newInfraMachine(testMachineName, ""),
	)

	other := &infrav1.ProvisionedMachine{ObjectMeta: metav1.ObjectMeta{
		Name: "other-cp-1", Namespace: otherNamespace, UID: types.UID("uid-other-cp-1"),
	}}
	require.NoError(t, k8sClient.Create(context.Background(), other))
	requireClaimAcquired(t, reconciler, other, "other", host)

	result, err := reconciler.Reconcile(context.Background(), machineRequest(testMachineName))
	require.NoError(t, err)
	require.Equal(t, retryAfter, result.RequeueAfter)
	requireReadyCondition(t, getInfraMachine(t, k8sClient, testMachineName).Status.Conditions,
		metav1.ConditionFalse, infrav1.HostsClaimedElsewhereReason)
}

func TestMachineReconcilerBlocksDeletionAndRetainsClaim(t *testing.T) {
	host := hostWithAddress("host-01", "10.0.0.11")
	reconciler, k8sClient := newMachineReconcilerForTest(t,
		newInfraCluster(host), newCAPICluster(), newOwnerMachine(), newInfraMachine(testMachineName, ""),
	)

	_, err := reconciler.Reconcile(context.Background(), machineRequest(testMachineName))
	require.NoError(t, err)
	machine := getInfraMachine(t, k8sClient, testMachineName)
	require.NoError(t, k8sClient.Delete(context.Background(), machine))

	result, err := reconciler.Reconcile(context.Background(), machineRequest(testMachineName))
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)

	require.NoError(t, k8sClient.Get(context.Background(),
		client.ObjectKey{Namespace: machine.Namespace, Name: machine.Name}, machine))
	require.Contains(t, machine.Finalizers, machineFinalizer)
	requireReadyCondition(t, machine.Status.Conditions,
		metav1.ConditionFalse, infrav1.DeletionNotSupportedReason)
	claim := &infrav1.ProvisionedHostClaim{}
	require.NoError(t, k8sClient.Get(context.Background(),
		client.ObjectKey{Name: infrav1.HostClaimName(host.IPAddress)}, claim))
}

func TestMachineReconcilerAllowsDeletionBeforeAHostIsClaimed(t *testing.T) {
	machine := newInfraMachine(testMachineName, "")
	reconciler, k8sClient := newMachineReconcilerForTest(t, machine)

	require.NoError(t, k8sClient.Delete(context.Background(), machine))
	result, err := reconciler.Reconcile(context.Background(), machineRequest(testMachineName))
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)

	err = k8sClient.Get(context.Background(), client.ObjectKeyFromObject(machine), machine)
	require.True(t, apierrors.IsNotFound(err))
}

func TestMachineReconcilerBlocksDeletionAfterAClaimBeforeProviderIDPersistence(t *testing.T) {
	machine := newInfraMachine(testMachineName, "")
	claim := &infrav1.ProvisionedHostClaim{
		ObjectMeta: metav1.ObjectMeta{Name: infrav1.HostClaimName("10.0.0.11")},
		Spec: infrav1.ProvisionedHostClaimSpec{
			IPAddress:        "10.0.0.11",
			ClusterName:      testClusterName,
			MachineNamespace: machine.Namespace,
			MachineName:      machine.Name,
			MachineUID:       string(machine.UID),
		},
	}
	reconciler, k8sClient := newMachineReconcilerForTest(t, machine, claim)

	require.NoError(t, k8sClient.Delete(context.Background(), machine))
	result, err := reconciler.Reconcile(context.Background(), machineRequest(testMachineName))
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)

	require.NoError(t, k8sClient.Get(context.Background(), client.ObjectKeyFromObject(machine), machine))
	require.Contains(t, machine.Finalizers, machineFinalizer)
	requireReadyCondition(t, machine.Status.Conditions,
		metav1.ConditionFalse, infrav1.DeletionNotSupportedReason)
}

func TestMachineReconcilerRejectsABoundHostClaimedByAnotherMachine(t *testing.T) {
	host := hostWithAddress("host-01", "10.0.0.11")
	machine := newInfraMachine(testMachineName, infrav1.FormatProviderID(testClusterName, host.IPAddress))
	foreignClaim := &infrav1.ProvisionedHostClaim{
		ObjectMeta: metav1.ObjectMeta{Name: infrav1.HostClaimName(host.IPAddress)},
		Spec: infrav1.ProvisionedHostClaimSpec{
			IPAddress:        host.IPAddress,
			ClusterName:      "other",
			MachineNamespace: otherNamespace,
			MachineName:      "other-machine",
			MachineUID:       "uid-other-machine",
		},
	}
	reconciler, k8sClient := newMachineReconcilerForTest(t,
		newInfraCluster(host), newCAPICluster(), newOwnerMachine(), machine, foreignClaim,
	)

	result, err := reconciler.Reconcile(context.Background(), machineRequest(testMachineName))
	require.NoError(t, err)
	require.Equal(t, retryAfter, result.RequeueAfter)

	machine = getInfraMachine(t, k8sClient, testMachineName)
	require.Nil(t, machine.Status.Initialization.Provisioned)
	requireReadyCondition(t, machine.Status.Conditions,
		metav1.ConditionFalse, infrav1.HostsClaimedElsewhereReason)
}

func TestMachineReconcilerReportsABoundHostClaimedByAPreviousMachine(t *testing.T) {
	host := hostWithAddress("host-01", "10.0.0.11")
	machine := newInfraMachine(testMachineName, infrav1.FormatProviderID(testClusterName, host.IPAddress))
	staleClaim := &infrav1.ProvisionedHostClaim{
		ObjectMeta: metav1.ObjectMeta{Name: infrav1.HostClaimName(host.IPAddress)},
		Spec: infrav1.ProvisionedHostClaimSpec{
			IPAddress:        host.IPAddress,
			ClusterName:      testClusterName,
			MachineNamespace: machine.Namespace,
			MachineName:      machine.Name,
			MachineUID:       "uid-previous-incarnation",
		},
	}
	reconciler, k8sClient := newMachineReconcilerForTest(t,
		newInfraCluster(host), newCAPICluster(), newOwnerMachine(), machine, staleClaim,
	)

	result, err := reconciler.Reconcile(context.Background(), machineRequest(testMachineName))
	require.NoError(t, err)
	require.Equal(t, retryAfter, result.RequeueAfter)

	machine = getInfraMachine(t, k8sClient, testMachineName)
	require.Nil(t, machine.Status.Initialization.Provisioned)
	requireReadyCondition(t, machine.Status.Conditions,
		metav1.ConditionFalse, infrav1.StaleHostClaimReason)
}

func TestMachineReconcilerTreatsAClaimWithNoHolderAsPermanent(t *testing.T) {
	host := hostWithAddress("host-01", "10.0.0.11")
	abandoned := &infrav1.ProvisionedHostClaim{
		ObjectMeta: metav1.ObjectMeta{Name: infrav1.HostClaimName(host.IPAddress)},
		Spec: infrav1.ProvisionedHostClaimSpec{
			IPAddress:        host.IPAddress,
			ClusterName:      "deleted-cluster",
			MachineNamespace: otherNamespace,
			MachineName:      "deleted-machine",
			MachineUID:       "uid-deleted-machine",
		},
	}
	reconciler, k8sClient := newMachineReconcilerForTest(t,
		newInfraCluster(host), newCAPICluster(), newOwnerMachine(), newInfraMachine(testMachineName, ""), abandoned,
	)

	result, err := reconciler.Reconcile(context.Background(), machineRequest(testMachineName))
	require.NoError(t, err)
	require.Equal(t, retryAfter, result.RequeueAfter)

	claim := &infrav1.ProvisionedHostClaim{}
	require.NoError(t, k8sClient.Get(context.Background(), client.ObjectKey{Name: abandoned.Name}, claim))
	require.Equal(t, abandoned.Spec.MachineUID, claim.Spec.MachineUID)
}

func TestMachineReconcilerRequeuesAVanishedClaimWithoutAnError(t *testing.T) {
	host := hostWithAddress("host-01", "10.0.0.11")
	claim := &infrav1.ProvisionedHostClaim{
		ObjectMeta: metav1.ObjectMeta{Name: infrav1.HostClaimName(host.IPAddress)},
		Spec: infrav1.ProvisionedHostClaimSpec{
			IPAddress:        host.IPAddress,
			ClusterName:      "other",
			MachineNamespace: otherNamespace,
			MachineName:      "other-machine",
			MachineUID:       "uid-other-machine",
		},
	}
	reconciler, _ := newMachineReconcilerForTest(t,
		newInfraCluster(host), newCAPICluster(), newOwnerMachine(), newInfraMachine(testMachineName, ""), claim,
	)
	// Create observes the existing claim, but the live reader observes that it
	// was deleted before the subsequent Get.
	reconciler.apiReader = fake.NewClientBuilder().WithScheme(newTestScheme(t)).Build()

	result, err := reconciler.Reconcile(context.Background(), machineRequest(testMachineName))
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{Requeue: true}, result)
}

func TestMachineReconcilerKeepsAnExistingBinding(t *testing.T) {
	reconciler, k8sClient := newMachineReconcilerForTest(t,
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
		newCAPICluster(),
		newOwnerMachine(),
		newInfraMachine(testMachineName, "provisioned://test/10.0.0.12"),
	)

	result, err := reconciler.Reconcile(context.Background(), machineRequest(testMachineName))
	require.NoError(t, err)
	require.Equal(t, retryAfter, result.RequeueAfter)

	machine := getInfraMachine(t, k8sClient, testMachineName)
	require.Equal(t, "provisioned://test/10.0.0.12", machine.Spec.ProviderID)
	require.Equal(t, "host-02", machine.Status.HostName)
	require.Equal(t, "10.0.0.12", machine.Status.Addresses[1].Address)
}

func TestMachineReconcilerReportsAnExhaustedInventory(t *testing.T) {
	reconciler, k8sClient := newMachineReconcilerForTest(t,
		newInfraCluster(
			infrav1.ProvisionedHost{
				Hostname:  "host-01",
				IPAddress: "10.0.0.11",
			},
		),
		newCAPICluster(),
		newOwnerMachine(),
		newOwnerMachineNamed("second"),
		newInfraMachine(testMachineName, "provisioned://test/10.0.0.11"),
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
			infrav1.ProvisionedHost{
				Hostname:  "host-01",
				IPAddress: "10.0.0.11",
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

func TestMachineReconcilerAddsFinalizerBeforeAllocation(t *testing.T) {
	machine := newInfraMachine(testMachineName, "")
	machine.Finalizers = nil
	reconciler, k8sClient := newMachineReconcilerForTest(t,
		newInfraCluster(hostWithAddress("host-01", "10.0.0.11")),
		newCAPICluster(),
		newOwnerMachine(),
		machine,
	)

	result, err := reconciler.Reconcile(context.Background(), machineRequest(testMachineName))
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{Requeue: true}, result)

	machine = getInfraMachine(t, k8sClient, testMachineName)
	require.Contains(t, machine.Finalizers, machineFinalizer)
	require.Empty(t, machine.Spec.ProviderID)
}

func TestMachineReconcilerWaitsForTheInfraCluster(t *testing.T) {
	reconciler, k8sClient := newMachineReconcilerForTest(t,
		newCAPICluster(),
		newOwnerMachine(),
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
			infrav1.ProvisionedHost{
				Hostname:  "host-02",
				IPAddress: "10.0.0.12",
			},
		),
		newCAPICluster(),
		newOwnerMachine(),
		newInfraMachine(testMachineName, "provisioned://test/10.0.0.11"),
	)

	_, err := reconciler.Reconcile(context.Background(), machineRequest(testMachineName))
	require.NoError(t, err)

	machine := getInfraMachine(t, k8sClient, testMachineName)
	require.Equal(t, "provisioned://test/10.0.0.11", machine.Spec.ProviderID)
	requireReadyCondition(t, machine.Status.Conditions, metav1.ConditionFalse, infrav1.HostNotFoundReason)
}

func TestMachineReconcilerDoesNotAllocateFromInvalidInventory(t *testing.T) {
	reconciler, k8sClient := newMachineReconcilerForTest(t,
		newInfraCluster(hostWithAddress("host-01", "not-an-ip")),
		newCAPICluster(),
		newOwnerMachine(),
		newInfraMachine(testMachineName, ""),
	)

	result, err := reconciler.Reconcile(context.Background(), machineRequest(testMachineName))
	require.NoError(t, err)
	require.Equal(t, retryAfter, result.RequeueAfter)

	machine := getInfraMachine(t, k8sClient, testMachineName)
	require.Empty(t, machine.Spec.ProviderID)
	requireReadyCondition(t, machine.Status.Conditions,
		metav1.ConditionFalse, infrav1.InventoryInvalidReason)
	claims := &infrav1.ProvisionedHostClaimList{}
	require.NoError(t, k8sClient.List(context.Background(), claims))
	require.Empty(t, claims.Items)
}

func TestMachineReconcilerRejectsAnInfraClusterOfAnotherProvider(t *testing.T) {
	capiCluster := newCAPICluster()
	capiCluster.Spec.InfrastructureRef.Kind = "NicoCluster"

	reconciler, _ := newMachineReconcilerForTest(t,
		capiCluster,
		newOwnerMachine(),
		newInfraMachine(testMachineName, ""),
	)

	_, err := reconciler.Reconcile(context.Background(), machineRequest(testMachineName))
	require.ErrorContains(t, err, "not ProvisionedCluster")
}

func machineRequest(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: name}}
}
