// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"testing"

	"github.com/stretchr/testify/require"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	infrav1 "github.com/NVIDIA/nke/controller/preprovisioned/api/v1alpha1"
)

func host(name string, role infrav1.HostRole, labels map[string]string) infrav1.PreprovisionedHost {
	return infrav1.PreprovisionedHost{
		Name:      name,
		Role:      role,
		Labels:    labels,
		Addresses: []clusterv1.MachineAddress{{Type: clusterv1.MachineInternalIP, Address: "10.0.0.1"}},
	}
}

func machineWithProviderID(name, providerID string) infrav1.PreprovisionedMachine {
	machine := infrav1.PreprovisionedMachine{}
	machine.Name = name
	machine.Spec.ProviderID = providerID
	return machine
}

func TestAllocationsFromMachines(t *testing.T) {
	machines := []infrav1.PreprovisionedMachine{
		machineWithProviderID("m1", infrav1.FormatProviderID("c1", "host-01")),
		machineWithProviderID("m2", infrav1.FormatProviderID("c1", "host-02")),
		// No provider ID yet: holds no host.
		machineWithProviderID("m3", ""),
		// Another provider: cannot name a host of this inventory.
		machineWithProviderID("m4", "nico://instance-9"),
		// Another cluster: its host names are not ours.
		machineWithProviderID("m5", infrav1.FormatProviderID("c2", "host-03")),
	}

	allocations := allocationsFromMachines("c1", machines)

	require.Equal(t, hostAllocations{"host-01": "m1", "host-02": "m2"}, allocations)
}

func TestSelectHostsKeepsInventoryOrder(t *testing.T) {
	hosts := []infrav1.PreprovisionedHost{
		host("host-01", "", nil),
		host("host-02", "", nil),
		host("host-03", "", nil),
	}

	usable := selectHosts(hosts, hostAllocations{"host-01": "m1"}, false, nil)

	require.Len(t, usable, 2)
	require.Equal(t, "host-02", usable[0].Name, "the first candidate should follow inventory order")
	require.Equal(t, "host-03", usable[1].Name)
}

func TestSelectHostsRespectsRole(t *testing.T) {
	hosts := []infrav1.PreprovisionedHost{
		host("worker-only", infrav1.HostRoleWorker, nil),
		host("cp-only", infrav1.HostRoleControlPlane, nil),
		host("any", "", nil),
	}

	controlPlaneHosts := selectHosts(hosts, hostAllocations{}, true, nil)
	require.Len(t, controlPlaneHosts, 2)
	require.Equal(t, "cp-only", controlPlaneHosts[0].Name)
	require.Equal(t, "any", controlPlaneHosts[1].Name)

	workerHosts := selectHosts(hosts, hostAllocations{}, false, nil)
	require.Len(t, workerHosts, 2)
	require.Equal(t, "worker-only", workerHosts[0].Name)
}

func TestSelectHostsFallsBackToUnroledHost(t *testing.T) {
	hosts := []infrav1.PreprovisionedHost{
		host("cp-only", infrav1.HostRoleControlPlane, nil),
		host("any", "", nil),
	}

	usable := selectHosts(hosts, hostAllocations{"cp-only": "m1"}, true, nil)

	require.Len(t, usable, 1)
	require.Equal(t, "any", usable[0].Name)
}

func TestSelectHostsRespectsSelector(t *testing.T) {
	hosts := []infrav1.PreprovisionedHost{
		host("gpu-less", "", map[string]string{"rack": "a"}),
		host("gpu", "", map[string]string{"rack": "b", "gpu": "h100"}),
	}

	usable := selectHosts(hosts, hostAllocations{}, false, map[string]string{"gpu": "h100"})

	require.Len(t, usable, 1)
	require.Equal(t, "gpu", usable[0].Name)
}

func TestSelectHostsReportsExhaustedInventory(t *testing.T) {
	hosts := []infrav1.PreprovisionedHost{host("host-01", "", nil)}

	require.Empty(t, selectHosts(hosts, hostAllocations{"host-01": "m1"}, false, nil))
}

func TestSelectHostsRejectsUnknownRole(t *testing.T) {
	hosts := []infrav1.PreprovisionedHost{host("odd", infrav1.HostRole("storage"), nil)}

	require.Empty(t, selectHosts(hosts, hostAllocations{}, false, nil))
}

func TestValidateInventory(t *testing.T) {
	tests := []struct {
		name    string
		hosts   []infrav1.PreprovisionedHost
		wantErr string
	}{
		{
			name:  "valid",
			hosts: []infrav1.PreprovisionedHost{host("host-01", "", nil), host2("host-02", "10.0.0.2")},
		},
		{
			name:    "repeated name",
			hosts:   []infrav1.PreprovisionedHost{host("host-01", "", nil), host2("host-01", "10.0.0.2")},
			wantErr: `host name "host-01" is used more than once`,
		},
		{
			name:    "repeated address",
			hosts:   []infrav1.PreprovisionedHost{host("host-01", "", nil), host2("host-02", "10.0.0.1")},
			wantErr: `address "10.0.0.1" is used by host "host-01" and by host "host-02"`,
		},
		{
			name:    "no address",
			hosts:   []infrav1.PreprovisionedHost{{Name: "host-01"}},
			wantErr: `host "host-01" has no address`,
		},
		{
			name:    "no name",
			hosts:   []infrav1.PreprovisionedHost{host2("", "10.0.0.1")},
			wantErr: "every host must have a name",
		},
		{
			name: "empty address string",
			hosts: []infrav1.PreprovisionedHost{{
				Name:      "host-01",
				Addresses: []clusterv1.MachineAddress{{Type: clusterv1.MachineInternalIP}},
			}},
			wantErr: `host "host-01" has an empty address`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateInventory(test.hosts)
			if test.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.EqualError(t, err, test.wantErr)
		})
	}
}

func TestAvailableHostCount(t *testing.T) {
	hosts := []infrav1.PreprovisionedHost{
		host("host-01", "", nil),
		host2("host-02", "10.0.0.2"),
		host2("host-03", "10.0.0.3"),
	}

	require.Equal(t, int32(2), availableHostCount(hosts, hostAllocations{"host-01": "m1"}))
}

func TestSortedAllocationsIsStable(t *testing.T) {
	allocations := hostAllocations{"host-03": "m3", "host-01": "m1", "host-02": "m2"}

	require.Equal(t, []infrav1.HostAllocation{
		{HostName: "host-01", MachineName: "m1"},
		{HostName: "host-02", MachineName: "m2"},
		{HostName: "host-03", MachineName: "m3"},
	}, sortedAllocations(allocations))

	require.Nil(t, sortedAllocations(hostAllocations{}))
}

// host2 builds a host with an explicit address, for cases that need two hosts
// that do not collide.
func host2(name, address string) infrav1.PreprovisionedHost {
	return infrav1.PreprovisionedHost{
		Name:      name,
		Addresses: []clusterv1.MachineAddress{{Type: clusterv1.MachineInternalIP, Address: address}},
	}
}
