// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFormatAndParseProviderID(t *testing.T) {
	providerID := FormatProviderID("my-cluster", "host-01")
	require.Equal(t, "preprovisioned://my-cluster/host-01", providerID)

	clusterName, hostName, err := ParseProviderID(providerID)
	require.NoError(t, err)
	require.Equal(t, "my-cluster", clusterName)
	require.Equal(t, "host-01", hostName)
}

// A provider ID of another provider must never yield a host name, or a machine
// of that provider would appear to hold a host of this inventory.
func TestParseProviderIDRejectsOtherProviders(t *testing.T) {
	for _, providerID := range []string{
		"nico://instance-1",
		"docker://cluster/host",
		"",
		"preprovisioned://",
		"preprovisioned://only-cluster",
		"preprovisioned:///host-01",
		"preprovisioned://cluster/",
	} {
		_, _, err := ParseProviderID(providerID)
		require.Error(t, err, "expected %q to be rejected", providerID)
	}
}

func TestHostLookup(t *testing.T) {
	cluster := &PreprovisionedCluster{
		Spec: PreprovisionedClusterSpec{
			Hosts: []PreprovisionedHost{{Name: "host-01"}, {Name: "host-02"}},
		},
	}

	host, found := cluster.Host("host-02")
	require.True(t, found)
	require.Equal(t, "host-02", host.Name)

	_, found = cluster.Host("absent")
	require.False(t, found)
}
