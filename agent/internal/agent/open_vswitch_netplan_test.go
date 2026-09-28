// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseOpenVSwitchNetplanDocumentCreatesOptionalSections(t *testing.T) {
	document, err := parseOpenVSwitchNetplanDocument([]byte(`network:
  version: 2
  renderer: networkd
`))

	require.NoError(t, err)
	document.ethernets["eth0"] = map[string]any{"dhcp4": true}
	document.bridges["breth0"] = map[string]any{"interfaces": []string{"eth0"}}

	network := document.root["network"].(map[string]any)
	assert.Equal(t, "networkd", network["renderer"])
	assert.Equal(t, document.ethernets, network["ethernets"])
	assert.Equal(t, document.bridges, network["bridges"])
}

func TestParseOpenVSwitchNetplanDocumentRejectsMalformedSections(t *testing.T) {
	testCases := []struct {
		name        string
		config      string
		errorString string
	}{
		{
			name:        "missing network",
			config:      "renderer: networkd\n",
			errorString: "network section is required",
		},
		{
			name:        "network is not a mapping",
			config:      "network: disabled\n",
			errorString: "network section must be a mapping",
		},
		{
			name:        "ethernets is not a mapping",
			config:      "network:\n  ethernets: []\n",
			errorString: "network.ethernets section must be a mapping",
		},
		{
			name:        "bridges is not a mapping",
			config:      "network:\n  bridges: disabled\n",
			errorString: "network.bridges section must be a mapping",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := parseOpenVSwitchNetplanDocument([]byte(testCase.config))

			require.Error(t, err)
			assert.Contains(t, err.Error(), testCase.errorString)
		})
	}
}

func TestConfiguredOpenVSwitchNetplanPreservesFabricIdentityAndManagement(t *testing.T) {
	config := `network:
  version: 2
  renderer: networkd
  ethernets:
    01-eth1:
      match:
        macaddress: "00:11:22:33:44:55"
        driver: virtio_net
      set-name: eth1
      mtu: 9000
      wakeonlan: true
      receive-checksum-offload: false
      optional: true
      dhcp4: false
      dhcp6: false
      addresses: [172.31.1.10/24]
      routes:
        - to: 172.31.0.0/16
          via: 172.31.1.1
      nameservers:
        addresses: [172.31.1.53]
      routing-policy:
        - from: 172.31.1.10
          table: 100
      accept-ra: false
    00-mgmt0:
      match:
        macaddress: "00:11:22:33:44:66"
      set-name: mgmt0
      dhcp4: true
      dhcp4-overrides:
        route-metric: 10
    all-en:
      match:
        name: "*"
      dhcp4: true
  vlans:
    management:
      id: 100
      link: 00-mgmt0
      addresses: [192.0.2.10/24]
`
	server, paths := newOpenVSwitchNetplanTestServer(t, map[string]string{"10-fabric.yaml": config})
	writeOpenVSwitchTestDriver(t, server.RootPath, "eth1", "virtio_net")
	link := openVSwitchLink{Name: "eth1", Address: "00:11:22:33:44:55", MTU: 1500}

	plan, err := server.buildConfiguredOpenVSwitchNetplanPlan(link, "br-eth1")

	require.NoError(t, err)
	assert.False(t, plan.configMatchesPlan)
	assert.Equal(t, 9000, plan.uplinkMTU)
	assert.Equal(t, "eth1", plan.uplinkName)
	assert.Empty(t, plan.defaultGateway)
	result, err := parseOpenVSwitchNetplanDocument(plan.config)
	require.NoError(t, err)
	original, err := parseOpenVSwitchNetplanDocument([]byte(config))
	require.NoError(t, err)
	assert.Equal(t, original.ethernets["00-mgmt0"], result.ethernets["00-mgmt0"])
	assert.Equal(t, original.ethernets["all-en"], result.ethernets["all-en"])
	assert.Equal(t, original.root["network"].(map[string]any)["vlans"], result.root["network"].(map[string]any)["vlans"])
	assert.NotContains(t, result.ethernets, "eth1")
	physical := result.ethernets["01-eth1"].(map[string]any)
	assert.Equal(t, map[string]any{"macaddress": link.Address, "driver": "virtio_net"}, physical["match"])
	assert.Equal(t, "eth1", physical["set-name"])
	assert.Equal(t, true, physical["wakeonlan"])
	assert.Equal(t, false, physical["receive-checksum-offload"])
	assert.Equal(t, true, physical["optional"])
	assert.Equal(t, float64(9000), physical["mtu"])
	assert.NotContains(t, physical, "addresses")
	assert.NotContains(t, physical, "routes")
	assert.NotContains(t, physical, "dhcp4")
	bridge := result.bridges["br-eth1"].(map[string]any)
	assert.Equal(t, []any{"01-eth1"}, bridge["interfaces"])
	assert.Equal(t, float64(9000), bridge["mtu"])
	assert.Equal(t, false, bridge["dhcp4"])
	assert.Equal(t, false, bridge["dhcp6"])
	assert.Equal(t, []any{"172.31.1.10/24"}, bridge["addresses"])
	assert.Equal(t, []any{map[string]any{openVSwitchRouteTo: "172.31.0.0/16", openVSwitchRouteVia: "172.31.1.1"}}, bridge["routes"])
	assert.Equal(t, map[string]any{"addresses": []any{"172.31.1.53"}}, bridge["nameservers"])
	assert.Equal(t, []any{map[string]any{"from": "172.31.1.10", "table": float64(100)}}, bridge["routing-policy"])
	assert.Equal(t, false, bridge["accept-ra"])
	assert.NotContains(t, bridge, "match")
	assert.NotContains(t, bridge, "set-name")
	assert.NotContains(t, bridge, "wakeonlan")
	assert.NotContains(t, bridge, "receive-checksum-offload")
	assert.Less(t, strings.Index(string(plan.config), "01-eth1:"), strings.Index(string(plan.config), "all-en:"))
	onDisk, err := os.ReadFile(paths["10-fabric.yaml"])
	require.NoError(t, err)
	assert.Equal(t, config, string(onDisk), "building a plan must not write it")

	// An interrupted apply leaves the logical Netplan ID in bridge.interfaces.
	// Recover that same bridge without duplicating or renaming its physical NIC.
	require.NoError(t, os.WriteFile(paths["10-fabric.yaml"], append([]byte("# Operator comment\n"), plan.config...), 0o600))
	recovered, err := server.buildConfiguredOpenVSwitchNetplanPlan(link, "br-eth1")
	require.NoError(t, err)
	assert.Equal(t, plan.config, recovered.config)
	assert.True(t, recovered.configMatchesPlan, "formatting must not trigger another apply")
	assert.Equal(t, plan.expected, recovered.expected)
}

func TestConfiguredOpenVSwitchNetplanMovesDHCPAndPreservesObservedMTU(t *testing.T) {
	server, _ := newOpenVSwitchNetplanTestServer(t, map[string]string{"10-uplink.yaml": `network:
  version: 2
  ethernets:
    eth0:
      dhcp4: true
      dhcp6: true
      dhcp-identifier: mac
      dhcp4-overrides:
        route-metric: 25
        use-dns: false
      dhcp6-overrides:
        route-metric: 25
        use-dns: false
      link-local: [ipv6]
      macaddress: "00:11:22:33:44:55"
      renderer: networkd
`})

	plan, err := server.buildConfiguredOpenVSwitchNetplanPlan(openVSwitchLink{Name: "eth0", MTU: 8500}, "breth0")

	require.NoError(t, err)
	result, err := parseOpenVSwitchNetplanDocument(plan.config)
	require.NoError(t, err)
	physical := result.ethernets["eth0"].(map[string]any)
	bridge := result.bridges["breth0"].(map[string]any)
	assert.Equal(t, float64(8500), physical["mtu"])
	assert.Equal(t, float64(8500), bridge["mtu"])
	assert.Equal(t, "00:11:22:33:44:55", physical["macaddress"])
	assert.NotContains(t, bridge, "macaddress")
	assert.Equal(t, "networkd", physical["renderer"])
	assert.Equal(t, "networkd", bridge["renderer"])
	assert.Equal(t, true, bridge["dhcp4"])
	assert.Equal(t, true, bridge["dhcp6"])
	assert.Equal(t, "mac", bridge["dhcp-identifier"])
	assert.Equal(t, map[string]any{"route-metric": float64(25), "use-dns": false}, bridge["dhcp4-overrides"])
	assert.Equal(t, bridge["dhcp4-overrides"], bridge["dhcp6-overrides"])
	assert.Equal(t, []any{"ipv6"}, bridge["link-local"])
	assert.NotContains(t, physical, "dhcp4-overrides")
	assert.NotContains(t, physical, "dhcp6-overrides")
	assert.NotContains(t, physical, "link-local")
}

func TestConfiguredOpenVSwitchNetplanRejectsUnsafeLayouts(t *testing.T) {
	const exactUplink = `network:
  version: 2
  ethernets:
    01-eth1:
      match:
        macaddress: "00:11:22:33:44:55"
      set-name: eth1
      addresses: [172.31.1.10/24]
`
	tests := []struct {
		name        string
		config      string
		additional  string
		errorString string
	}{
		{
			name: "wildcard group cannot be migrated in explicit mode",
			config: `network:
  ethernets:
    all-en:
      match:
        name: "*"
      dhcp4: true
`,
			errorString: "expected one netplan configuration",
		},
		{
			name: "multiple specific matches",
			config: exactUplink + `    second:
      match:
        macaddress: "00:11:22:33:44:55"
`,
			errorString: "got 2",
		},
		{
			name: "wildcard has earlier networkd priority",
			config: exactUplink + `    00-all:
      match:
        name: "*"
      dhcp4: true
`,
			errorString: "wildcard 00-all precedes interface 01-eth1",
		},
		{
			name:   "logical ID spans multiple files",
			config: exactUplink,
			additional: `network:
  ethernets:
    01-eth1:
      dhcp4: true
`,
			errorString: "defined in multiple files",
		},
		{
			name: "target bridge has another member",
			config: exactUplink + `  bridges:
    br-eth1:
      interfaces: [mgmt0]
      openvswitch: {}
`,
			errorString: "does not exclusively use interface",
		},
		{
			name: "target bridge has extra members",
			config: exactUplink + `  bridges:
    br-eth1:
      interfaces: [01-eth1, mgmt0]
      openvswitch: {}
`,
			errorString: "does not exclusively use interface",
		},
		{
			name: "uplink already belongs to a different bridge",
			config: exactUplink + `  bridges:
    another:
      interfaces: [01-eth1, mgmt0]
      openvswitch: {}
`,
			errorString: "already belongs to bridges another",
		},
		{
			name:   "uplink already belongs to a bond in another file",
			config: exactUplink,
			additional: `network:
  bonds:
    bond0:
      interfaces: [01-eth1, eth2]
`,
			errorString: "already belongs to bonds bond0",
		},
		{
			name: "uplink is a VLAN parent by logical ID",
			config: exactUplink + `  vlans:
    management:
      id: 100
      link: 01-eth1
      addresses: [192.0.2.10/24]
`,
			errorString: "netplan interface 01-eth1 is the parent link of vlan management",
		},
		{
			name:   "uplink is a VLAN parent in another file",
			config: exactUplink,
			additional: `network:
  vlans:
    management:
      id: 100
      link: 01-eth1
      addresses: [192.0.2.10/24]
`,
			errorString: "netplan interface 01-eth1 is the parent link of vlan management",
		},
		{
			name: "target is a Linux bridge",
			config: exactUplink + `  bridges:
    br-eth1:
      interfaces: [01-eth1]
`,
			errorString: "not an Open vSwitch bridge",
		},
		{
			name:   "bridge split across files",
			config: exactUplink,
			additional: `network:
  bridges:
    br-eth1:
      interfaces: [01-eth1]
      openvswitch: {}
`,
			errorString: "defined outside the uplink configuration",
		},
		{
			name:        "MTU is not a positive integer",
			config:      exactUplink + "      mtu: -1\n",
			errorString: "must be a positive integer",
		},
		{
			name: "target name is already an ethernet definition",
			config: exactUplink + `    br-eth1:
      dhcp4: true
`,
			errorString: "conflicts with an ethernet definition",
		},
		{
			name: "existing bridge and uplink have conflicting MTUs",
			config: exactUplink + `      mtu: 9000
  bridges:
    br-eth1:
      interfaces: [01-eth1]
      openvswitch: {}
      mtu: 1500
`,
			errorString: "MTU differs from interface",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			files := map[string]string{"10-fabric.yaml": test.config}
			if test.additional != "" {
				files["90-overrides.yaml"] = test.additional
			}
			server, paths := newOpenVSwitchNetplanTestServer(t, files)

			_, err := server.buildConfiguredOpenVSwitchNetplanPlan(openVSwitchLink{Name: "eth1", Address: "00:11:22:33:44:55", MTU: 1500}, "br-eth1")

			require.ErrorContains(t, err, test.errorString)
			for name, path := range paths {
				data, readErr := os.ReadFile(path)
				require.NoError(t, readErr)
				assert.Equal(t, files[name], string(data))
			}
		})
	}
}

func TestOpenVSwitchNetplanLegacyWildcardKeepsOtherInterfaces(t *testing.T) {
	config := `network:
  version: 2
  ethernets:
    all-en:
      match:
        name: "en*"
      dhcp4: true
      mtu: 9000
`
	server, paths := newOpenVSwitchNetplanTestServer(t, map[string]string{"50-cloud-init.yaml": config})
	link := openVSwitchLink{Name: "ens3", Address: "00:11:22:33:44:55", MTU: 9000}

	plan, err := server.buildOpenVSwitchNetplanPlan(link, "10.0.0.1", "brens3")

	require.NoError(t, err)
	result, err := parseOpenVSwitchNetplanDocument(plan.config)
	require.NoError(t, err)
	original, err := parseOpenVSwitchNetplanDocument([]byte(config))
	require.NoError(t, err)
	assert.Equal(t, original.ethernets["all-en"], result.ethernets["all-en"])
	physical := result.ethernets["00-nvke-ens3"].(map[string]any)
	assert.Equal(t, map[string]any{openVSwitchNetplanMatchName: "ens3", "macaddress": link.Address}, physical["match"])
	assert.NotContains(t, physical, "dhcp4")
	bridge := result.bridges["brens3"].(map[string]any)
	assert.Equal(t, []any{"00-nvke-ens3"}, bridge["interfaces"])
	assert.Equal(t, true, bridge["dhcp4"])
	assert.Equal(t, "10.0.0.1", plan.defaultGateway)
	require.NoError(t, os.WriteFile(paths["50-cloud-init.yaml"], plan.config, 0o600))
	recovered, err := server.buildOpenVSwitchNetplanPlan(link, "10.0.0.1", "brens3")
	require.NoError(t, err)
	assert.Equal(t, plan.config, recovered.config)
}

func TestOpenVSwitchNetplanMatchingUsesAllKnownConstraints(t *testing.T) {
	link := openVSwitchLink{Name: "eth1", Address: "00:11:22:33:44:55"}
	tests := []struct {
		name   string
		stanza map[string]any
		exact  bool
	}{
		{name: "literal kernel name", stanza: map[string]any{}, exact: true},
		{name: "matching MAC", stanza: map[string]any{"match": map[string]any{"macaddress": "00:11:22:33:44:55"}}, exact: true},
		{name: "wrong MAC overrides logical name", stanza: map[string]any{"match": map[string]any{"macaddress": "00:11:22:33:44:66"}}},
		{name: "wrong MAC overrides set-name", stanza: map[string]any{"set-name": "eth1", "match": map[string]any{"macaddress": "00:11:22:33:44:66"}}},
		{name: "matching driver and MAC", stanza: map[string]any{"match": map[string]any{"macaddress": link.Address, "driver": "virtio_net"}}, exact: true},
		{name: "wrong driver overrides MAC and set-name", stanza: map[string]any{"set-name": "eth1", "match": map[string]any{"macaddress": link.Address, "driver": "ixgbe"}}},
		{name: "all match constraints must hold", stanza: map[string]any{"match": map[string]any{openVSwitchNetplanMatchName: "eth2", "macaddress": link.Address}}},
		{name: "post-rename name", stanza: map[string]any{"set-name": "eth1", "match": map[string]any{openVSwitchNetplanMatchName: "en*", "macaddress": link.Address}}, exact: true},
		{name: "would rename requested interface", stanza: map[string]any{"set-name": "eth2", "match": map[string]any{"macaddress": link.Address}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			exact, _ := openVSwitchNetplanMatchesLink("eth1", test.stanza, link, "virtio_net")
			assert.Equal(t, test.exact, exact)
		})
	}
}

func TestOpenVSwitchNetplanDriverMatches(t *testing.T) {
	for _, test := range []struct {
		name    string
		pattern any
		driver  string
		matches bool
	}{
		{name: "literal", pattern: "virtio_net", driver: "virtio_net", matches: true},
		{name: "glob", pattern: "virtio*", driver: "virtio_net", matches: true},
		{name: "different driver", pattern: "ixgbe", driver: "virtio_net"},
		{name: "list with matching glob", pattern: []any{"ixgbe", "virtio*"}, driver: "virtio_net", matches: true},
		{name: "list without match", pattern: []any{"ixgbe", "ena"}, driver: "virtio_net"},
		{name: "unknown driver", pattern: "*"},
		{name: "invalid glob", pattern: "[", driver: "virtio_net"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.matches, openVSwitchNetplanDriverMatches(test.pattern, test.driver))
		})
	}
}

func TestOpenVSwitchNetplanDriverWildcardPrecedence(t *testing.T) {
	for _, strict := range []bool{false, true} {
		for _, test := range []struct {
			name, pattern, driver, wantError string
		}{
			{name: "unrelated rule", pattern: "ixgbe", driver: "virtio_net"},
			{name: "matching rule", pattern: "virtio*", driver: "virtio_net", wantError: "wildcard 00-storage precedes interface eth1"},
			{name: "matching list", pattern: "[ixgbe, virtio*]", driver: "virtio_net", wantError: "wildcard 00-storage precedes interface eth1"},
			{name: "missing driver", pattern: "ixgbe", wantError: "read driver for netplan interface eth1"},
		} {
			t.Run(fmt.Sprintf("strict=%t/%s", strict, test.name), func(t *testing.T) {
				config := fmt.Sprintf(`network:
  version: 2
  ethernets:
    00-storage:
      match:
        driver: %s
      dhcp4: true
    eth1:
      addresses: [192.0.2.10/24]
`, test.pattern)
				server, _ := newOpenVSwitchNetplanTestServer(t, map[string]string{"10-uplink.yaml": config})
				if test.driver != "" {
					writeOpenVSwitchTestDriver(t, server.RootPath, "eth1", test.driver)
				}
				_, err := server.buildOpenVSwitchNetplanPlanWithMode(openVSwitchLink{Name: "eth1", MTU: 1500}, "", "br-eth1", strict)
				if test.wantError != "" {
					require.ErrorContains(t, err, test.wantError)
				} else {
					require.NoError(t, err)
				}
			})
		}
	}
}

func writeOpenVSwitchTestDriver(t *testing.T, root, name, driver string) {
	t.Helper()
	path := filepath.Join(root, "sys/class/net", name, "device/driver")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.Symlink(filepath.Join("../../../bus/virtio/drivers", driver), path))
}

func TestConfiguredOpenVSwitchNetplanRejectsConflictingLayers(t *testing.T) {
	const uplink = `network:
  version: 2
  ethernets:
    01-fabric:
      match: {macaddress: "00:11:22:33:44:55"}
      set-name: eth1
      addresses: [172.31.1.10/24]
`
	const global = "network:\n  version: 2\n  renderer: networkd\n"
	for _, test := range []struct {
		name       string
		persistent string
		external   string
		config     string
		wantError  string
	}{
		{
			name: "runtime overrides logical ID", persistent: uplink, external: "run/netplan/90-overrides.yaml",
			config: "network:\n  ethernets:\n    01-fabric:\n      dhcp4: true\n", wantError: "defined in multiple files",
		},
		{
			name: "vendor contributes same logical ID", persistent: uplink, external: "lib/netplan/00-vendor.yaml",
			config: "network:\n  ethernets:\n    01-fabric:\n      mtu: 9000\n", wantError: "defined in multiple files",
		},
		{
			name: "runtime defines requested bridge", persistent: uplink, external: "run/netplan/90-bridge.yaml",
			config: "network:\n  bridges:\n    br-eth1:\n      interfaces: [01-fabric]\n      openvswitch: {}\n", wantError: "defined outside the uplink configuration",
		},
		{
			name: "runtime file shadows persistent file", persistent: uplink, external: "run/netplan/10-fabric.yaml",
			config: global, wantError: "exists in multiple configuration directories",
		},
		{
			name: "runtime only interface is not persistent", persistent: global, external: "run/netplan/90-fabric.yaml",
			config: uplink, wantError: "requires a persistent definition under /etc/netplan",
		},
		{
			name: "vendor interface must not be modified", persistent: global, external: "lib/netplan/10-vendor.yaml",
			config: uplink, wantError: "requires a persistent definition under /etc/netplan",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, paths := newOpenVSwitchNetplanTestServer(t, map[string]string{"10-fabric.yaml": test.persistent})
			externalPath := filepath.Join(server.RootPath, test.external)
			require.NoError(t, os.MkdirAll(filepath.Dir(externalPath), 0o755))
			require.NoError(t, os.WriteFile(externalPath, []byte(test.config), 0o600))

			_, err := server.buildConfiguredOpenVSwitchNetplanPlan(openVSwitchLink{Name: "eth1", Address: "00:11:22:33:44:55", MTU: 1500}, "br-eth1")

			require.ErrorContains(t, err, test.wantError)
			for path, want := range map[string]string{paths["10-fabric.yaml"]: test.persistent, externalPath: test.config} {
				data, readErr := os.ReadFile(path)
				require.NoError(t, readErr)
				assert.Equal(t, want, string(data), "planning must not modify %s", path)
			}
		})
	}
}

func TestConfiguredOpenVSwitchNetplanAllowsUnrelatedLayers(t *testing.T) {
	server, paths := newOpenVSwitchNetplanTestServer(t, map[string]string{"10-fabric.yaml": `network:
  version: 2
  ethernets:
    eth1:
      addresses: [172.31.1.10/24]
`})
	layers := map[string]string{
		"lib/netplan/00-defaults.yaml":   "network:\n  version: 2\n  renderer: networkd\n",
		"run/netplan/90-management.yaml": "network:\n  ethernets:\n    mgmt0:\n      dhcp4: true\n",
	}
	for name, config := range layers {
		path := filepath.Join(server.RootPath, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(config), 0o600))
	}

	plan, err := server.buildConfiguredOpenVSwitchNetplanPlan(openVSwitchLink{Name: "eth1", MTU: 1500}, "br-eth1")

	require.NoError(t, err)
	assert.Equal(t, paths["10-fabric.yaml"], plan.configPath)
	for name, want := range layers {
		data, readErr := os.ReadFile(filepath.Join(server.RootPath, name))
		require.NoError(t, readErr)
		assert.Equal(t, want, string(data))
	}
}

func TestOpenVSwitchNetplanLegacyIgnoresOtherDirectories(t *testing.T) {
	config := "network:\n  ethernets:\n    eth0:\n      dhcp4: true\n"
	server, paths := newOpenVSwitchNetplanTestServer(t, map[string]string{"10-uplink.yaml": config})
	runtimePath := filepath.Join(server.RootPath, "run/netplan/10-uplink.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(runtimePath), 0o755))
	require.NoError(t, os.WriteFile(runtimePath, []byte(config), 0o600))

	plan, err := server.buildOpenVSwitchNetplanPlan(openVSwitchLink{Name: "eth0", MTU: 1500}, "192.0.2.1", "breth0")

	require.NoError(t, err)
	assert.Equal(t, paths["10-uplink.yaml"], plan.configPath)
	data, err := os.ReadFile(runtimePath)
	require.NoError(t, err)
	assert.Equal(t, config, string(data))
}

func newOpenVSwitchNetplanTestServer(t *testing.T, files map[string]string) (*NodeAgentServer, map[string]string) {
	t.Helper()
	root := t.TempDir()
	netplan := filepath.Join(root, "etc/netplan")
	require.NoError(t, os.MkdirAll(netplan, 0o755))
	paths := make(map[string]string, len(files))
	for name, config := range files {
		path := filepath.Join(netplan, name)
		require.NoError(t, os.WriteFile(path, []byte(config), 0o600))
		paths[name] = path
	}
	return &NodeAgentServer{RootPath: root}, paths
}
