// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"testing"

	"github.com/stretchr/testify/require"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	infrav1 "github.com/dsx-ai-factory/nvke/cluster-api-provider-provisioned/api/v1alpha1"
)

func host(hostname, ipAddress string, labels map[string]string) infrav1.ProvisionedHost {
	return infrav1.ProvisionedHost{
		Hostname:  hostname,
		IPAddress: ipAddress,
		Labels:    labels,
	}
}

func machineWithProviderID(name, providerID string) infrav1.ProvisionedMachine {
	machine := infrav1.ProvisionedMachine{}
	machine.Name = name
	machine.Spec.ProviderID = providerID
	return machine
}

func TestAllocationsFromMachines(t *testing.T) {
	machines := []infrav1.ProvisionedMachine{
		machineWithProviderID("m1", infrav1.FormatProviderID("c1", "10.0.0.1")),
		machineWithProviderID("m2", infrav1.FormatProviderID("c1", "10.0.0.2")),
		machineWithProviderID("m3", ""),
		machineWithProviderID("m4", "nico://instance-9"),
		machineWithProviderID("m5", infrav1.FormatProviderID("c2", "10.0.0.3")),
	}

	require.Equal(t, hostAllocations{"10.0.0.1": "m1", "10.0.0.2": "m2"},
		allocationsFromMachines("c1", machines))
}

func TestSelectHostsKeepsInventoryOrder(t *testing.T) {
	hosts := []infrav1.ProvisionedHost{
		host("host-01", "10.0.0.1", nil),
		host("host-02", "10.0.0.2", nil),
		host("host-03", "10.0.0.3", nil),
	}

	usable := selectHosts(hosts, hostAllocations{"10.0.0.1": "m1"}, nil)

	require.Len(t, usable, 2)
	require.Equal(t, "host-02", usable[0].Hostname)
	require.Equal(t, "host-03", usable[1].Hostname)
}

func TestSelectHostsRespectsSelector(t *testing.T) {
	hosts := []infrav1.ProvisionedHost{
		host("gpu-less", "10.0.0.1", map[string]string{"rack": "a"}),
		host("gpu", "10.0.0.2", map[string]string{"rack": "b", "gpu": "h100"}),
	}

	usable := selectHosts(hosts, hostAllocations{}, map[string]string{"gpu": "h100"})
	require.Len(t, usable, 1)
	require.Equal(t, "gpu", usable[0].Hostname)
}

func TestSelectHostsReportsExhaustedInventory(t *testing.T) {
	hosts := []infrav1.ProvisionedHost{host("host-01", "10.0.0.1", nil)}
	require.Empty(t, selectHosts(hosts, hostAllocations{"10.0.0.1": "m1"}, nil))
}

func TestValidateInventory(t *testing.T) {
	tests := []struct {
		name    string
		hosts   []infrav1.ProvisionedHost
		wantErr string
	}{
		{
			name: "valid",
			hosts: []infrav1.ProvisionedHost{
				{Hostname: "host-01", IPAddress: "10.0.0.1", TPMEKPubHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
				host("host-02.example.test", "2001:db8::2", nil),
			},
		},
		{
			name:    "invalid TPM EK public key hash",
			hosts:   []infrav1.ProvisionedHost{{Hostname: "host-01", IPAddress: "10.0.0.1", TPMEKPubHash: "not-a-sha256"}},
			wantErr: `host "host-01" has invalid TPM EK public key hash "not-a-sha256"`,
		},
		{
			name: "repeated hostname",
			hosts: []infrav1.ProvisionedHost{
				host("host-01", "10.0.0.1", nil),
				host("host-01", "10.0.0.2", nil),
			},
			wantErr: `hostname "host-01" is used more than once`,
		},
		{
			name: "repeated canonical IP address",
			hosts: []infrav1.ProvisionedHost{
				host("host-01", "2001:db8::1", nil),
				host("host-02", "2001:0db8:0:0::1", nil),
			},
			wantErr: `IP address "2001:db8::1" is used by host "host-01" and by host "host-02"`,
		},
		{
			name:    "missing IP address",
			hosts:   []infrav1.ProvisionedHost{{Hostname: "host-01"}},
			wantErr: `host "host-01" has invalid IP address "": ParseAddr(""): unable to parse IP`,
		},
		{
			name:    "missing hostname",
			hosts:   []infrav1.ProvisionedHost{{IPAddress: "10.0.0.1"}},
			wantErr: "every host must have a hostname",
		},
		{
			name:    "invalid hostname",
			hosts:   []infrav1.ProvisionedHost{host("Host_01", "10.0.0.1", nil)},
			wantErr: `host "Host_01" has an invalid hostname: a lowercase RFC 1123 subdomain must consist of lower case alphanumeric characters, '-' or '.', and must start and end with an alphanumeric character (e.g. 'example.com', regex used for validation is '[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*')`,
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
	hosts := []infrav1.ProvisionedHost{
		host("host-01", "10.0.0.1", nil),
		host("host-02", "10.0.0.2", nil),
		host("host-03", "10.0.0.3", nil),
	}
	require.Equal(t, int32(2), availableHostCount(hosts, hostAllocations{"10.0.0.1": "m1"}))
}

func TestSortedAllocationsIsStable(t *testing.T) {
	hosts := []infrav1.ProvisionedHost{
		host("host-03", "10.0.0.3", nil),
		host("host-01", "10.0.0.1", nil),
		host("host-02", "10.0.0.2", nil),
	}
	allocations := hostAllocations{"10.0.0.3": "m3", "10.0.0.1": "m1", "10.0.0.2": "m2"}

	require.Equal(t, []infrav1.HostAllocation{
		{HostName: "host-01", MachineName: "m1"},
		{HostName: "host-02", MachineName: "m2"},
		{HostName: "host-03", MachineName: "m3"},
	}, sortedAllocations(hosts, allocations))
	require.Nil(t, sortedAllocations(hosts, hostAllocations{}))
}

func TestMachineAddressesContainHostnameAndCanonicalInternalIP(t *testing.T) {
	require.Equal(t, []clusterv1.MachineAddress{
		{Type: clusterv1.MachineHostName, Address: "host-01.example.test"},
		{Type: clusterv1.MachineInternalIP, Address: "2001:db8::1"},
	}, machineAddresses(host("host-01.example.test", "2001:0db8:0:0::1", nil)))
}
