// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/NVIDIA/nke/controller/preprovisioned/api/v1alpha1"
)

// The second cluster is the reason this lock exists: each cluster carries its
// own inventory in its own namespace, so nothing else can see the overlap.
const otherNamespace = "cluster-other"

func hostWithAddress(name, address string) infrav1.PreprovisionedHost {
	return infrav1.PreprovisionedHost{
		Name:      name,
		Addresses: []clusterv1.MachineAddress{{Type: clusterv1.MachineInternalIP, Address: address}},
	}
}

func TestHostIdentitiesCoverEveryDeclaredField(t *testing.T) {
	host := infrav1.PreprovisionedHost{
		Name:      "host-01",
		MachineID: "SERIAL-123",
		Addresses: []clusterv1.MachineAddress{
			{Type: clusterv1.MachineInternalIP, Address: "10.0.0.11"},
			{Type: clusterv1.MachineExternalIP, Address: "192.0.2.11"},
		},
	}

	require.Equal(t, []string{"SERIAL-123", "10.0.0.11", "192.0.2.11"}, host.Identities())

	// A host given only an address still locks something, or the whole scheme
	// would be optional in practice.
	require.Equal(t, []string{"10.0.0.11"}, hostWithAddress("host-01", "10.0.0.11").Identities())
}

func TestHostClaimNameIsStableAndDistinct(t *testing.T) {
	// Stable: the same identity must always resolve to the same lock, or two
	// claimants would never collide.
	first := infrav1.HostClaimName("10.0.0.11")
	require.Equal(t, "hostclaim-8b51baea68b18710dd1d3ffbd6c84816e527cb485baeb58ff4252fdc5630deac", first)

	require.NotEqual(t, first, infrav1.HostClaimName("10.0.0.12"))

	// Identities have no common safe encoding, so the name must tolerate any of
	// them without sanitizing two different values onto one lock.
	for _, identity := range []string{"10.0.0.11", "SERIAL/123", "a b c", "UUID:4c4c-0031"} {
		name := infrav1.HostClaimName(identity)
		require.NotEmpty(t, name)
		require.LessOrEqual(t, len(name), 253)
	}
}

// The core guarantee: one physical machine, two clusters, one winner.
func TestClaimHostRefusesAHostHeldByAnotherCluster(t *testing.T) {
	host := hostWithAddress("host-01", "10.0.0.11")

	reconciler, k8sClient := newMachineReconcilerForTest(t,
		newInfraCluster(host),
		newCAPICluster(),
		newOwnerMachine(false),
		newInfraMachine(testMachineName, ""),
	)

	// Another cluster got there first, under its own host name.
	foreign := hostWithAddress("rack2-node7", "10.0.0.11")
	other := &infrav1.PreprovisionedMachine{}
	other.Name = "other-cp-1"
	other.Namespace = otherNamespace
	require.NoError(t, k8sClient.Create(context.Background(), other))
	require.NoError(t, reconciler.claimHost(context.Background(), other, "other", foreign))

	mine := getInfraMachine(t, k8sClient, testMachineName)
	err := reconciler.claimHost(context.Background(), mine, testClusterName, host)

	require.ErrorIs(t, err, errHostClaimedElsewhere)
	require.ErrorContains(t, err, "other-cp-1")
}

// Overlap should cost a host, not the node: the machine moves to the next one.
func TestMachineReconcilerSkipsAHostLockedByAnotherCluster(t *testing.T) {
	first := hostWithAddress("host-01", "10.0.0.11")
	second := hostWithAddress("host-02", "10.0.0.12")

	reconciler, k8sClient := newMachineReconcilerForTest(t,
		newInfraCluster(first, second),
		newCAPICluster(),
		newOwnerMachine(false),
		newInfraMachine(testMachineName, ""),
	)

	other := &infrav1.PreprovisionedMachine{}
	other.Name = "other-cp-1"
	other.Namespace = otherNamespace
	require.NoError(t, k8sClient.Create(context.Background(), other))
	require.NoError(t, reconciler.claimHost(context.Background(), other, "other", first))

	_, err := reconciler.Reconcile(context.Background(), machineRequest(testMachineName))
	require.NoError(t, err)

	machine := getInfraMachine(t, k8sClient, testMachineName)
	require.Equal(t, "preprovisioned://test/host-02", machine.Spec.ProviderID,
		"the machine should have stepped over the locked host")
}

func TestMachineReconcilerReportsWhenEveryHostIsLockedElsewhere(t *testing.T) {
	host := hostWithAddress("host-01", "10.0.0.11")

	reconciler, k8sClient := newMachineReconcilerForTest(t,
		newInfraCluster(host),
		newCAPICluster(),
		newOwnerMachine(false),
		newInfraMachine(testMachineName, ""),
	)

	other := &infrav1.PreprovisionedMachine{}
	other.Name = "other-cp-1"
	other.Namespace = otherNamespace
	require.NoError(t, k8sClient.Create(context.Background(), other))
	require.NoError(t, reconciler.claimHost(context.Background(), other, "other", host))

	result, err := reconciler.Reconcile(context.Background(), machineRequest(testMachineName))
	require.NoError(t, err)
	require.Equal(t, retryAfter, result.RequeueAfter)

	machine := getInfraMachine(t, k8sClient, testMachineName)
	require.Empty(t, machine.Spec.ProviderID)
	requireReadyCondition(t, machine.Status.Conditions,
		metav1.ConditionFalse, infrav1.HostsClaimedElsewhereReason)
}

// Without this a provider that died between claiming and binding would strand a
// host forever, which is worse than the double-booking the lock prevents.
func TestClaimHostReclaimsAClaimWhoseHolderIsGone(t *testing.T) {
	host := hostWithAddress("host-01", "10.0.0.11")

	reconciler, k8sClient := newMachineReconcilerForTest(t,
		newInfraCluster(host),
		newCAPICluster(),
		newOwnerMachine(false),
		newInfraMachine(testMachineName, ""),
	)

	// A claim naming a machine that does not exist.
	abandoned := &infrav1.PreprovisionedHostClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: infrav1.HostClaimName("10.0.0.11"),
			Labels: map[string]string{
				infrav1.ClaimClusterLabel:          "dead",
				infrav1.ClaimMachineNamespaceLabel: otherNamespace,
				infrav1.ClaimMachineLabel:          "dead-cp-1",
			},
		},
		Spec: infrav1.PreprovisionedHostClaimSpec{
			Identity:         "10.0.0.11",
			ClusterName:      "dead",
			MachineNamespace: otherNamespace,
			MachineName:      "dead-cp-1",
		},
	}
	require.NoError(t, k8sClient.Create(context.Background(), abandoned))

	mine := getInfraMachine(t, k8sClient, testMachineName)
	err := reconciler.claimHost(context.Background(), mine, testClusterName, host)

	// The first pass reclaims and asks to be retried rather than recursing, so
	// one reconcile cannot loop against something recreating the claim.
	require.Error(t, err)
	require.NotErrorIs(t, err, errHostClaimedElsewhere)

	require.NoError(t, reconciler.claimHost(context.Background(), mine, testClusterName, host))
	claim := &infrav1.PreprovisionedHostClaim{}
	require.NoError(t, k8sClient.Get(context.Background(),
		client.ObjectKey{Name: infrav1.HostClaimName("10.0.0.11")}, claim))
	require.Equal(t, testMachineName, claim.Spec.MachineName)
}

func TestClaimHostIsIdempotentForItsOwnHolder(t *testing.T) {
	host := hostWithAddress("host-01", "10.0.0.11")

	reconciler, k8sClient := newMachineReconcilerForTest(t,
		newInfraCluster(host),
		newCAPICluster(),
		newOwnerMachine(false),
		newInfraMachine(testMachineName, ""),
	)

	mine := getInfraMachine(t, k8sClient, testMachineName)
	require.NoError(t, reconciler.claimHost(context.Background(), mine, testClusterName, host))
	require.NoError(t, reconciler.claimHost(context.Background(), mine, testClusterName, host))
}

// A host with several identities must be locked under all of them or it stays
// reachable through the ones that were missed.
func TestClaimHostLocksEveryIdentityOrNone(t *testing.T) {
	host := infrav1.PreprovisionedHost{
		Name:      "host-01",
		MachineID: "SERIAL-123",
		Addresses: []clusterv1.MachineAddress{{Type: clusterv1.MachineInternalIP, Address: "10.0.0.11"}},
	}

	reconciler, k8sClient := newMachineReconcilerForTest(t,
		newInfraCluster(host),
		newCAPICluster(),
		newOwnerMachine(false),
		newInfraMachine(testMachineName, ""),
	)

	mine := getInfraMachine(t, k8sClient, testMachineName)
	require.NoError(t, reconciler.claimHost(context.Background(), mine, testClusterName, host))

	for _, identity := range []string{"SERIAL-123", "10.0.0.11"} {
		claim := &infrav1.PreprovisionedHostClaim{}
		require.NoError(t, k8sClient.Get(context.Background(),
			client.ObjectKey{Name: infrav1.HostClaimName(identity)}, claim), "identity %q", identity)
	}

	// Another cluster describing the same machine by serial alone still
	// collides, even though it shares no address and no name.
	other := &infrav1.PreprovisionedMachine{}
	other.Name = "other-cp-1"
	other.Namespace = otherNamespace
	require.NoError(t, k8sClient.Create(context.Background(), other))
	err := reconciler.claimHost(context.Background(), other, "other",
		infrav1.PreprovisionedHost{Name: "elsewhere", MachineID: "SERIAL-123"})
	require.ErrorIs(t, err, errHostClaimedElsewhere)
}

// Partial acquisition must not leave a host half-locked.
func TestClaimHostReleasesWhatItTookOnFailure(t *testing.T) {
	reconciler, k8sClient := newMachineReconcilerForTest(t,
		newCAPICluster(),
		newOwnerMachine(false),
		newInfraMachine(testMachineName, ""),
	)

	// Another cluster already holds the second identity of this host.
	other := &infrav1.PreprovisionedMachine{}
	other.Name = "other-cp-1"
	other.Namespace = otherNamespace
	require.NoError(t, k8sClient.Create(context.Background(), other))
	require.NoError(t, reconciler.claimHost(context.Background(), other, "other",
		hostWithAddress("theirs", "10.0.0.11")))

	host := infrav1.PreprovisionedHost{
		Name:      "host-01",
		MachineID: "SERIAL-123",
		Addresses: []clusterv1.MachineAddress{{Type: clusterv1.MachineInternalIP, Address: "10.0.0.11"}},
	}
	mine := getInfraMachine(t, k8sClient, testMachineName)
	require.ErrorIs(t, reconciler.claimHost(context.Background(), mine, testClusterName, host),
		errHostClaimedElsewhere)

	// The serial was taken first and must have been given back.
	claim := &infrav1.PreprovisionedHostClaim{}
	err := k8sClient.Get(context.Background(),
		client.ObjectKey{Name: infrav1.HostClaimName("SERIAL-123")}, claim)
	require.Error(t, err, "the machineID lock should have been released")
}

// A deleted machine must leave nothing behind, or its hosts stay locked.
func TestReconcileReleasesClaimsWhenTheMachineIsGone(t *testing.T) {
	host := hostWithAddress("host-01", "10.0.0.11")

	reconciler, k8sClient := newMachineReconcilerForTest(t,
		newInfraCluster(host),
		newCAPICluster(),
		newOwnerMachine(false),
		newInfraMachine(testMachineName, ""),
	)

	_, err := reconciler.Reconcile(context.Background(), machineRequest(testMachineName))
	require.NoError(t, err)
	claim := &infrav1.PreprovisionedHostClaim{}
	require.NoError(t, k8sClient.Get(context.Background(),
		client.ObjectKey{Name: infrav1.HostClaimName("10.0.0.11")}, claim))

	machine := getInfraMachine(t, k8sClient, testMachineName)
	require.NoError(t, k8sClient.Delete(context.Background(), machine))

	_, err = reconciler.Reconcile(context.Background(), machineRequest(testMachineName))
	require.NoError(t, err)

	require.Error(t, k8sClient.Get(context.Background(),
		client.ObjectKey{Name: infrav1.HostClaimName("10.0.0.11")}, claim),
		"the host should be free once its machine is gone")
}
