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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/NVIDIA/nke/cluster-api-provider-provisioned/api/v1alpha1"
)

// The second cluster is the reason this lock exists: each cluster carries its
// own inventory in its own namespace, so nothing else can see the overlap.
const otherNamespace = "cluster-other"

func hostWithAddress(name, address string) infrav1.ProvisionedHost {
	return infrav1.ProvisionedHost{
		Hostname:  name,
		IPAddress: address,
	}
}

func TestHostClaimNameIsStableAndDistinct(t *testing.T) {
	// Stable: the same IP address must always resolve to the same lock, or two
	// claimants would never collide.
	first := infrav1.HostClaimName("10.0.0.11")
	require.Equal(t, "hostclaim-8b51baea68b18710dd1d3ffbd6c84816e527cb485baeb58ff4252fdc5630deac", first)

	require.NotEqual(t, first, infrav1.HostClaimName("10.0.0.12"))

	// The hash must also produce a valid object name for IPv6 addresses.
	for _, ipAddress := range []string{"10.0.0.11", "2001:db8::11"} {
		name := infrav1.HostClaimName(ipAddress)
		require.NotEmpty(t, name)
		require.LessOrEqual(t, len(name), 253)
	}
}

func TestClaimHostRejectsInvalidIPAddressBeforeCreatingClaim(t *testing.T) {
	machine := newInfraMachine(testMachineName, "")
	reconciler, k8sClient := newMachineReconcilerForTest(t, machine)

	err := reconciler.claimHost(context.Background(), machine, testClusterName,
		hostWithAddress("host-01", "not-an-ip"))
	require.ErrorContains(t, err, "invalid IP address")

	claims := &infrav1.ProvisionedHostClaimList{}
	require.NoError(t, k8sClient.List(context.Background(), claims))
	require.Empty(t, claims.Items)
}

// The core guarantee: one physical machine, two clusters, one winner.
func TestClaimHostRefusesAHostHeldByAnotherCluster(t *testing.T) {
	host := hostWithAddress("host-01", "10.0.0.11")

	reconciler, k8sClient := newMachineReconcilerForTest(t,
		newInfraCluster(host),
		newCAPICluster(),
		newOwnerMachine(),
		newInfraMachine(testMachineName, ""),
	)

	// Another cluster got there first, under its own host name.
	foreign := hostWithAddress("rack2-node7", "10.0.0.11")
	other := &infrav1.ProvisionedMachine{}
	other.Name = "other-cp-1"
	other.Namespace = otherNamespace
	other.UID = types.UID("uid-other-cp-1")
	require.NoError(t, k8sClient.Create(context.Background(), other))
	require.NoError(t, reconciler.claimHost(context.Background(), other, "other", foreign))

	mine := getInfraMachine(t, k8sClient, testMachineName)
	err := reconciler.claimHost(context.Background(), mine, testClusterName, host)

	require.ErrorIs(t, err, errHostClaimedElsewhere)
	require.ErrorContains(t, err, "other-cp-1")
}

// Without this a provider that died between claiming and binding would strand a
// host forever, which is worse than the double-booking the lock prevents.
func TestClaimHostReclaimsAClaimWhoseHolderIsGone(t *testing.T) {
	host := hostWithAddress("host-01", "10.0.0.11")

	reconciler, k8sClient := newMachineReconcilerForTest(t,
		newInfraCluster(host),
		newCAPICluster(),
		newOwnerMachine(),
		newInfraMachine(testMachineName, ""),
	)

	// A claim naming a machine that does not exist.
	abandoned := &infrav1.ProvisionedHostClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: infrav1.HostClaimName("10.0.0.11"),
		},
		Spec: infrav1.ProvisionedHostClaimSpec{
			IPAddress:        "10.0.0.11",
			ClusterName:      "dead",
			MachineNamespace: otherNamespace,
			MachineName:      "dead-cp-1",
			MachineUID:       "uid-dead-cp-1",
		},
	}
	require.NoError(t, k8sClient.Create(context.Background(), abandoned))

	mine := getInfraMachine(t, k8sClient, testMachineName)
	err := reconciler.claimHost(context.Background(), mine, testClusterName, host)

	// The first pass reclaims and asks to be retried rather than recursing, so
	// one reconcile cannot loop against something recreating the claim.
	require.Error(t, err)
	require.ErrorIs(t, err, errClaimRetry)

	require.NoError(t, reconciler.claimHost(context.Background(), mine, testClusterName, host))
	claim := &infrav1.ProvisionedHostClaim{}
	require.NoError(t, k8sClient.Get(context.Background(),
		client.ObjectKey{Name: infrav1.HostClaimName("10.0.0.11")}, claim))
	require.Equal(t, testMachineName, claim.Spec.MachineName)
}

func TestClaimHostIsIdempotentForItsOwnHolder(t *testing.T) {
	host := hostWithAddress("host-01", "10.0.0.11")

	reconciler, k8sClient := newMachineReconcilerForTest(t,
		newInfraCluster(host),
		newCAPICluster(),
		newOwnerMachine(),
		newInfraMachine(testMachineName, ""),
	)

	mine := getInfraMachine(t, k8sClient, testMachineName)
	require.NoError(t, reconciler.claimHost(context.Background(), mine, testClusterName, host))
	require.NoError(t, reconciler.claimHost(context.Background(), mine, testClusterName, host))

	claim := &infrav1.ProvisionedHostClaim{}
	require.NoError(t, k8sClient.Get(context.Background(),
		client.ObjectKey{Name: infrav1.HostClaimName(host.IPAddress)}, claim))
	require.Empty(t, claim.Labels)
	require.Equal(t, string(mine.UID), claim.Spec.MachineUID)
}

func TestClaimHostDoesNotAdoptClaimFromPreviousMachineUID(t *testing.T) {
	host := hostWithAddress("host-01", "10.0.0.11")
	machine := newInfraMachine(testMachineName, "")
	oldClaim := &infrav1.ProvisionedHostClaim{
		ObjectMeta: metav1.ObjectMeta{Name: infrav1.HostClaimName(host.IPAddress)},
		Spec: infrav1.ProvisionedHostClaimSpec{
			IPAddress:        host.IPAddress,
			ClusterName:      testClusterName,
			MachineNamespace: machine.Namespace,
			MachineName:      machine.Name,
			MachineUID:       "uid-previous-incarnation",
			HostName:         host.Hostname,
		},
	}
	reconciler, k8sClient := newMachineReconcilerForTest(t, machine, oldClaim)

	err := reconciler.claimHost(context.Background(), machine, testClusterName, host)
	require.ErrorIs(t, err, errClaimRetry)
	require.NoError(t, reconciler.claimHost(context.Background(), machine, testClusterName, host))

	claim := &infrav1.ProvisionedHostClaim{}
	require.NoError(t, k8sClient.Get(context.Background(), client.ObjectKey{Name: oldClaim.Name}, claim))
	require.Equal(t, string(machine.UID), claim.Spec.MachineUID)
}

func TestMachineExistsUsesLiveReader(t *testing.T) {
	holder := newInfraMachine("holder", "")
	reconciler, _ := newMachineReconcilerForTest(t)
	reconciler.apiReader = fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithObjects(holder).
		Build()

	alive, err := reconciler.machineExists(context.Background(), holder.Namespace, holder.Name, holder.UID)
	require.NoError(t, err)
	require.True(t, alive)
}

func TestMachineExistsTreatsTerminatingHolderAsGone(t *testing.T) {
	holder := newInfraMachine("holder", "")
	now := metav1.Now()
	holder.DeletionTimestamp = &now
	holder.Finalizers = []string{"test.finalizer"}
	reconciler, _ := newMachineReconcilerForTest(t, holder)

	alive, err := reconciler.machineExists(context.Background(), holder.Namespace, holder.Name, holder.UID)
	require.NoError(t, err)
	require.False(t, alive)
}

func TestReleaseClaimsDeletesClaimsHeldByMachine(t *testing.T) {
	host := hostWithAddress("host-01", "10.0.0.11")
	reconciler, k8sClient := newMachineReconcilerForTest(t,
		newInfraCluster(host),
		newCAPICluster(),
		newOwnerMachine(),
		newInfraMachine(testMachineName, ""),
	)

	machine := getInfraMachine(t, k8sClient, testMachineName)
	require.NoError(t, reconciler.claimHost(context.Background(), machine, testClusterName, host))
	require.NoError(t, reconciler.releaseClaims(context.Background(), machine))

	claim := &infrav1.ProvisionedHostClaim{}
	err := k8sClient.Get(context.Background(),
		client.ObjectKey{Name: infrav1.HostClaimName(host.IPAddress)}, claim)
	require.True(t, apierrors.IsNotFound(err))
}

func TestReleaseClaimsKeepsClaimOfPreviousMachineUID(t *testing.T) {
	host := hostWithAddress("host-01", "10.0.0.11")
	machine := newInfraMachine(testMachineName, "")
	claim := &infrav1.ProvisionedHostClaim{
		ObjectMeta: metav1.ObjectMeta{Name: infrav1.HostClaimName(host.IPAddress)},
		Spec: infrav1.ProvisionedHostClaimSpec{
			IPAddress:        host.IPAddress,
			ClusterName:      testClusterName,
			MachineNamespace: machine.Namespace,
			MachineName:      machine.Name,
			MachineUID:       "uid-previous-incarnation",
		},
	}
	reconciler, k8sClient := newMachineReconcilerForTest(t, machine, claim)

	require.NoError(t, reconciler.releaseClaims(context.Background(), machine))
	require.NoError(t, k8sClient.Get(context.Background(), client.ObjectKey{Name: claim.Name}, claim))
}
