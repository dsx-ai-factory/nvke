// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustCIDR(t *testing.T, cidr string) *net.IPNet {
	t.Helper()
	_, ipNet, err := net.ParseCIDR(cidr)
	require.NoError(t, err)
	return ipNet
}

func TestConflist(t *testing.T) {
	network := &networkMeta{
		Name: "kind", ID: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", MTU: 1400,
		Subnets: []string{"10.4.0.0/24", "fd77:7777:7777:7777::/64"}, Gateways: []string{"10.4.0.1", "fd77:7777:7777:7777::1"},
	}
	out, err := network.conflist("/var/lib/ctrd/ipam")
	require.NoError(t, err)
	assert.JSONEq(t, `{
	  "cniVersion": "1.0.0",
	  "name": "kind",
	  "plugins": [
	    {
	      "type": "bridge", "bridge": "br-0123456789ab", "isGateway": true, "ipMasq": true, "hairpinMode": true, "mtu": 1400,
	      "capabilities": {"ips": true},
	      "ipam": {
	        "type": "host-local", "dataDir": "/var/lib/ctrd/ipam",
	        "ranges": [[{"subnet": "10.4.0.0/24", "gateway": "10.4.0.1"}], [{"subnet": "fd77:7777:7777:7777::/64", "gateway": "fd77:7777:7777:7777::1"}]],
	        "routes": [{"dst": "0.0.0.0/0"}]
	      }
	    },
	    {"type": "portmap", "capabilities": {"portMappings": true}},
	    {"type": "firewall"}
	  ]
	}`, string(out))
}

func TestFreeIPv4Subnet(t *testing.T) {
	tests := []struct {
		name    string
		used    []string
		want    string
		wantErr bool
	}{
		{name: "the first subnet of the pool when nothing uses it", want: "10.4.0.0/24"},
		{name: "the next one when the first is taken", used: []string{"10.4.0.0/24"}, want: "10.4.1.0/24"},
		{name: "an interface address inside a subnet takes it", used: []string{"10.4.0.9/24", "10.4.1.1/30"}, want: "10.4.2.0/24"},
		{name: "a larger network that holds the pool leaves nothing", used: []string{"10.0.0.0/8"}, wantErr: true},
		{name: "networks outside the pool do not matter", used: []string{"192.168.1.0/24", "172.18.0.0/16"}, want: "10.4.0.0/24"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var used []*net.IPNet
			for _, cidr := range tt.used {
				used = append(used, mustCIDR(t, cidr))
			}
			got, err := freeIPv4Subnet(used)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestFirstAddress(t *testing.T) {
	assert.Equal(t, "10.4.1.1", firstAddress(mustCIDR(t, "10.4.1.0/24")))
	assert.Equal(t, "fd77:7777:7777:7777::1", firstAddress(mustCIDR(t, "fd77:7777:7777:7777::/64")))
}

func TestNetworkCommands(t *testing.T) {
	h := newHarness(t)

	require.Equal(t, 0, h.run("network", "create", "-d=bridge", "-o", "com.docker.network.driver.mtu=1400", "--ipv6", "--subnet", "fd77:7777:7777:7777::/64", "kind"), h.stderr.String())
	id := h.stdout.String()
	assert.Regexp(t, `^[0-9a-f]{64}\n$`, id)

	network, err := h.eng.loadNetwork("kind")
	require.NoError(t, err)
	require.Len(t, network.Subnets, 2)
	assert.Equal(t, 1400, network.MTU)
	ipv4 := mustCIDR(t, network.Subnets[0])
	assert.NotNil(t, ipv4.IP.To4(), "an IPv4 subnet was chosen first")
	assert.Equal(t, firstAddress(ipv4), network.Gateways[0])
	assert.Equal(t, "fd77:7777:7777:7777::/64", network.Subnets[1])
	assert.FileExists(t, filepath.Join(h.root, "networks", "kind.conflist"))

	t.Run("what the callers read", func(t *testing.T) {
		require.Equal(t, 0, h.run("network", "inspect", "kind", "--format={{.Name}}"))
		assert.Equal(t, "kind\n", h.stdout.String())
		require.Equal(t, 0, h.run("network", "inspect", "--format", "{{.Id}}", "kind"))
		assert.Equal(t, id, h.stdout.String())
		require.Equal(t, 0, h.run("network", "inspect", "kind", "-f", "{{range .IPAM.Config}}{{println .Gateway}}{{end}}"))
		assert.Contains(t, h.stdout.String(), network.Gateways[0]+"\n")
		require.Equal(t, 0, h.run("network", "inspect", "kind", "-f", "{{range (index (index . \"IPAM\") \"Config\")}}{{index . \"Subnet\"}} {{end}}"))
		assert.Equal(t, network.Subnets[0]+" fd77:7777:7777:7777::/64 \n", h.stdout.String())
		require.Equal(t, 0, h.run("network", "inspect", "kind", "-f", `{{ index .Options "com.docker.network.driver.mtu" }}`))
		assert.Equal(t, "1400\n", h.stdout.String())
	})

	t.Run("an existing name is refused", func(t *testing.T) {
		require.Equal(t, 1, h.run("network", "create", "kind"))
		assert.Contains(t, h.stderr.String(), "already exists")
	})

	t.Run("a subnet that is taken is reported the way docker does, which kind retries on", func(t *testing.T) {
		require.Equal(t, 1, h.run("network", "create", "--subnet", network.Subnets[1], "other"))
		assert.True(t, strings.HasPrefix(h.stderr.String(), "Error response from daemon: Pool overlaps"), h.stderr.String())
	})

	t.Run("a network that is not there", func(t *testing.T) {
		require.Equal(t, 1, h.run("network", "inspect", "nope"))
		assert.Contains(t, h.stderr.String(), "network nope not found")
	})

	t.Run("a network with an attached container is not removed", func(t *testing.T) {
		require.NoError(t, os.MkdirAll(h.eng.containerDir("c1"), 0o755))
		require.NoError(t, h.eng.saveContainer(&containerMeta{ID: "c1", Name: "c1", Network: "kind", IPv4: "10.4.0.2"}))
		require.Equal(t, 1, h.run("network", "rm", "kind"))
		assert.Contains(t, h.stderr.String(), "active endpoints")
		require.NoError(t, os.RemoveAll(h.eng.containerDir("c1")))
	})

	t.Run("removing it removes its files and its leases", func(t *testing.T) {
		require.NoError(t, os.MkdirAll(filepath.Join(h.root, "ipam", "kind"), 0o755))
		require.Equal(t, 0, h.run("network", "rm", "kind"), h.stderr.String())
		assert.NoFileExists(t, filepath.Join(h.root, "networks", "kind.json"))
		assert.NoFileExists(t, filepath.Join(h.root, "networks", "kind.conflist"))
		assert.NoDirExists(t, filepath.Join(h.root, "ipam", "kind"))
	})

	t.Run("only bridge networks", func(t *testing.T) {
		require.Equal(t, 1, h.run("network", "create", "-d", "overlay", "x"))
		assert.Contains(t, h.stderr.String(), "not supported by this engine")
	})
}

func TestNetworkConflistRoundTrips(t *testing.T) {
	h := newHarness(t)
	require.Equal(t, 0, h.run("network", "create", "--subnet", "10.77.0.0/24", "n"))
	data, err := os.ReadFile(filepath.Join(h.root, "networks", "n.conflist"))
	require.NoError(t, err)
	var list struct {
		Plugins []map[string]any `json:"plugins"`
	}
	require.NoError(t, json.Unmarshal(data, &list))
	require.Len(t, list.Plugins, 3)
	ipam, _ := list.Plugins[0]["ipam"].(map[string]any)
	assert.Equal(t, filepath.Join(h.root, "ipam"), ipam["dataDir"], "leases live under the state directory")
}
