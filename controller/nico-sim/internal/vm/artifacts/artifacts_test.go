// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package artifacts

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"
)

// networkConfigYAML is the subset of cloud-init's netplan-style
// network-config this package cares about.
type networkConfigYAML struct {
	Ethernets struct {
		Eth0 struct {
			Match struct {
				MACAddress string `yaml:"macaddress"`
			} `yaml:"match"`
			SetName        string `yaml:"set-name"`
			DHCPIdentifier string `yaml:"dhcp-identifier"`
		} `yaml:"eth0"`
	} `yaml:"ethernets"`
}

func TestSeedNetworkInterface(t *testing.T) {
	for _, tc := range []struct {
		name      string
		preserve  bool
		ipAddress string // "" selects DHCP mode
	}{
		{"DHCP, rename (Linux default)", false, ""},
		{"DHCP, preserve (darwin-qemu)", true, ""},
		{"Static, rename (Linux default)", false, "192.168.64.10"},
		{"Static, preserve (darwin-qemu)", true, "192.168.64.10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			_, err := (Builder{MockMode: true}).Build(context.Background(), BuildInput{
				WorkingDir:            dir,
				InstanceID:            "test",
				Hostname:              "test",
				NICMAC:                "52:54:00:0a:0b:0c",
				PreserveInterfaceName: tc.preserve,
				IPAddress:             tc.ipAddress,
				GatewayIP:             "192.168.64.1",
				PrefixLength:          24,
			})
			require.NoError(t, err)
			data, err := os.ReadFile(filepath.Join(dir, "network-config"))
			require.NoError(t, err)

			var parsed networkConfigYAML
			require.NoError(t, yaml.Unmarshal(data, &parsed), "network-config must be valid YAML: %s", data)
			require.Equal(t, "52:54:00:0a:0b:0c", parsed.Ethernets.Eth0.Match.MACAddress)

			if tc.preserve {
				require.Empty(t, parsed.Ethernets.Eth0.SetName, "preserve must omit set-name")
			} else {
				require.Equal(t, "eth0", parsed.Ethernets.Eth0.SetName, "non-preserve must rename to eth0")
			}

			if tc.ipAddress == "" {
				// dhcp-identifier only makes sense in DHCP mode.
				if tc.preserve {
					require.Equal(t, "mac", parsed.Ethernets.Eth0.DHCPIdentifier)
				} else {
					require.Empty(t, parsed.Ethernets.Eth0.DHCPIdentifier)
				}
			} else {
				require.Empty(t, parsed.Ethernets.Eth0.DHCPIdentifier, "dhcp-identifier has no meaning in static mode")
			}
		})
	}
}
