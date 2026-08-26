// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFormatAndParseProviderID(t *testing.T) {
	providerID := FormatProviderID("my-cluster", "2001:0db8:0:0::11")
	require.Equal(t, "provisioned://my-cluster/2001:db8::11", providerID)

	clusterName, ipAddress, err := ParseProviderID(providerID)
	require.NoError(t, err)
	require.Equal(t, "my-cluster", clusterName)
	require.Equal(t, "2001:db8::11", ipAddress)
}

// A provider ID of another provider or one without an IP address must never
// yield an allocation key for this inventory.
func TestParseProviderIDRejectsOtherProviders(t *testing.T) {
	for _, providerID := range []string{
		"nico://instance-1",
		"docker://cluster/host",
		"",
		"provisioned://",
		"provisioned://only-cluster",
		"provisioned:///10.0.0.11",
		"provisioned://cluster/",
		"provisioned://cluster/host-01",
	} {
		_, _, err := ParseProviderID(providerID)
		require.Error(t, err, "expected %q to be rejected", providerID)
	}
}

func TestHostLookupByIPAddress(t *testing.T) {
	cluster := &ProvisionedCluster{
		Spec: ProvisionedClusterSpec{
			Hosts: []ProvisionedHost{
				{Hostname: "host-01", IPAddress: "10.0.0.11"},
				{Hostname: "host-02", IPAddress: "2001:0db8:0:0::12"},
			},
		},
	}

	host, found := cluster.HostByIPAddress("2001:db8::12")
	require.True(t, found)
	require.Equal(t, "host-02", host.Hostname)

	_, found = cluster.HostByIPAddress("10.0.0.99")
	require.False(t, found)
}
