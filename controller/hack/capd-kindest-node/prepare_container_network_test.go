// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capdkindestnode

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const staleNetplan = `network:
  version: 2
  renderer: networkd
  ethernets:
    eth0:
      addresses:
      - 172.18.0.6/16
      routes:
      - to: default
        via: 172.18.0.1
        on-link: true
      mtu: 1500
`

const staleBridgeNetplan = `network:
  version: 2
  renderer: networkd
  ethernets:
    eth0:
      match:
        macaddress: 02:42:ac:12:00:06
      mtu: 1500
  bridges:
    breth0:
      openvswitch: {}
      interfaces:
      - eth0
      addresses:
      - 172.18.0.6/16
      routes:
      - to: default
        via: 172.18.0.1
        on-link: true
      mtu: 1500
`

// A fake ip and netplan replay what Docker left on the node, and log every
// command that would change it.
const fakeIP = `#!/bin/sh
case "$*" in
  "-j -4 route show default") cat "$FAKE_STATE/routes.json" ;;
  "-j -4 address show dev eth0") cat "$FAKE_STATE/address.json" ;;
  "-j link show dev eth0") cat "$FAKE_STATE/link.json" ;;
  *) echo "ip $*" >> "$FAKE_STATE/calls" ;;
esac
`

const fakeNetplan = `#!/bin/sh
echo "netplan $*" >> "$FAKE_STATE/calls"
`

const dockerAddress = `[{"ifname":"eth0","mtu":1500,"addr_info":[{"family":"inet","local":"172.18.0.4","prefixlen":16,"scope":"global"}]}]`

func TestPrepareContainerNetwork(t *testing.T) {
	if err := exec.CommandContext(t.Context(), "python3", "-c", "import yaml").Run(); err != nil {
		t.Skip("python3 with PyYAML is not available")
	}

	physicalRoute := `[{"dst":"default","gateway":"172.18.0.1","dev":"eth0"}]`
	bridgeRoute := `[{"dst":"default","gateway":"172.18.0.1","dev":"breth0"}]`

	tests := []struct {
		name          string
		args          []string
		persisted     string
		routes        string
		wantAddress   string
		wantMAC       string
		wantUntouched bool
		wantCalls     string
	}{
		{
			name:        "restart refreshes a stale ethernet address",
			args:        []string{"--refresh-persisted"},
			persisted:   staleNetplan,
			routes:      physicalRoute,
			wantAddress: "172.18.0.4/16",
			wantCalls:   "netplan generate\n",
		},
		{
			name:        "restart refreshes a stale bridge address and MAC without touching the link",
			args:        []string{"--refresh-persisted"},
			persisted:   staleBridgeNetplan,
			routes:      physicalRoute,
			wantAddress: "172.18.0.4/16",
			wantMAC:     "02:42:ac:12:00:04",
			wantCalls:   "netplan generate\n",
		},
		{
			name:          "refresh leaves a bridge that already carries the default route alone",
			args:          []string{"--refresh-persisted"},
			persisted:     staleBridgeNetplan,
			routes:        bridgeRoute,
			wantUntouched: true,
		},
		{
			name:      "first boot has nothing to refresh",
			args:      []string{"--refresh-persisted"},
			routes:    physicalRoute,
			wantCalls: "",
		},
		{
			name:        "boot persists an ethernet address and applies it",
			routes:      physicalRoute,
			wantAddress: "172.18.0.4/16",
			wantCalls:   "netplan apply\n",
		},
		{
			name:        "boot restoring a bridge moves Docker's IPv4 state to it",
			persisted:   staleBridgeNetplan,
			routes:      physicalRoute,
			wantAddress: "172.18.0.4/16",
			wantMAC:     "02:42:ac:12:00:04",
			wantCalls: "ip -4 route del default via 172.18.0.1 dev eth0\n" +
				"ip -4 address flush dev eth0\n" +
				"netplan apply\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			state := filepath.Join(dir, "state")
			bin := filepath.Join(dir, "bin")
			require.NoError(t, os.MkdirAll(state, 0o700))
			require.NoError(t, os.MkdirAll(bin, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(bin, "ip"), []byte(fakeIP), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(bin, "netplan"), []byte(fakeNetplan), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(state, "routes.json"), []byte(tt.routes), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(state, "address.json"), []byte(dockerAddress), 0o600))
			link := `[{"ifname":"eth0","mtu":1500,"address":"02:42:ac:12:00:04"}]`
			require.NoError(t, os.WriteFile(filepath.Join(state, "link.json"), []byte(link), 0o600))

			config := filepath.Join(dir, "netplan", "50-nke-container.yaml")
			if tt.persisted != "" {
				require.NoError(t, os.MkdirAll(filepath.Dir(config), 0o700))
				require.NoError(t, os.WriteFile(config, []byte(tt.persisted), 0o600))
			}

			cmd := exec.CommandContext(t.Context(), "python3", append([]string{"scripts/prepare-container-network"}, tt.args...)...)
			cmd.Env = append(os.Environ(),
				"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"FAKE_STATE="+state,
				"PREPARE_CONTAINER_NETWORK_CONFIG="+config,
			)
			output, err := cmd.CombinedOutput()
			require.NoError(t, err, string(output))

			calls, err := os.ReadFile(filepath.Join(state, "calls"))
			if os.IsNotExist(err) {
				calls = nil
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.wantCalls, string(calls))

			got, err := os.ReadFile(config)
			if tt.persisted == "" && tt.wantAddress == "" {
				require.True(t, os.IsNotExist(err), "first-boot refresh must not create the file")
				return
			}
			require.NoError(t, err)
			if tt.wantUntouched {
				assert.Equal(t, tt.persisted, string(got))
				return
			}
			assert.Contains(t, string(got), "- "+tt.wantAddress)
			assert.NotContains(t, string(got), "172.18.0.6")
			assert.Contains(t, string(got), "via: 172.18.0.1")
			if tt.wantMAC != "" {
				assert.Contains(t, string(got), "macaddress: "+tt.wantMAC)
			}
		})
	}
}

func TestPrepareContainerNetworkRejectsUnknownArguments(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "python3", "scripts/prepare-container-network", "--bogus")
	output, err := cmd.CombinedOutput()
	require.Error(t, err)
	assert.Contains(t, string(output), "usage:")
}
