// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
	nvkeutils "github.com/dsx-ai-factory/nvke/agent/pkg/utils"
)

const configuredOVSNetplan = `network:
  version: 2
  ethernets:
    management:
      match: {macaddress: "02:00:00:00:00:01", driver: virtio_net}
      set-name: mgmt0
      dhcp4: true
      nameservers: {addresses: [192.0.2.53]}
    01-fabric:
      match: {macaddress: "02:00:00:00:00:02", driver: virtio_net}
      set-name: eth1
      mtu: 9000
      dhcp4: false
      addresses: [10.80.1.2/24]
      routes:
        - {to: 10.80.0.0/16, via: 10.80.1.1}
`

// configuredOVSRunner models a host with management's default route and a
// separately routed fabric. Only netplan apply changes the modeled networking.
type configuredOVSRunner struct {
	mu                sync.Mutex
	calls             []string
	bridge            bool
	missingRoute      bool
	missingUplink     bool
	virtualKind       string
	extraPortType     string
	applyErr          error
	routes4AfterApply string
	routes6AfterApply string
	bridgeAddresses   string
	bridgeNameInUse   bool
}

func (r *configuredOVSRunner) Run(name string, args []string, _ *nvkeutils.CommandOptions) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	command := strings.Join(append([]string{name}, args...), " ")
	r.calls = append(r.calls, command)
	switch command {
	case "ip -j link show":
		if r.bridgeNameInUse {
			return []byte(`[{"ifname":"br-eth1","mtu":9000}]`), nil
		}
		return []byte(`[{"ifname":"eth1","mtu":9000},{"ifname":"mgmt0","mtu":1500}]`), nil
	case "ip -d -j link show dev eth1", "ip -j link show dev eth1":
		if r.missingUplink {
			return nil, errors.New("no such device")
		}
		return []byte(fmt.Sprintf(`[{"ifname":"eth1","address":"02:00:00:00:00:02","mtu":9000,"linkinfo":{"info_kind":%q}}]`, r.virtualKind)), nil
	case "ip -j link show dev br-eth1":
		return []byte(`[{"ifname":"br-eth1","address":"02:00:00:00:00:02","mtu":9000}]`), nil
	case "ip -N -j -4 route show table all":
		if r.bridge && r.routes4AfterApply != "" {
			return []byte(r.routes4AfterApply), nil
		}
		fabric := "eth1"
		if r.bridge {
			fabric = "br-eth1"
		}
		if r.missingRoute {
			return []byte(`[{"dst":"default","gateway":"192.0.2.1","dev":"mgmt0","metric":100}]`), nil
		}
		return []byte(fmt.Sprintf(`[{"dst":"default","gateway":"192.0.2.1","dev":"mgmt0","metric":100},{"dst":"10.80.0.0/16","gateway":"10.80.1.1","dev":%q}]`, fabric)), nil
	case "ip -N -j -6 route show table all":
		if r.bridge && r.routes6AfterApply != "" {
			return []byte(r.routes6AfterApply), nil
		}
		return []byte(`[]`), nil
	case "dpkg-query --show --showformat=${db:Status-Abbrev} openvswitch-switch":
		return []byte("ii "), nil
	case "systemctl enable --now openvswitch-switch", "systemctl restart ovs-vswitchd", "netplan generate":
		return nil, nil
	case "ovs-vsctl list-br":
		if r.bridge {
			return []byte("br-eth1\n"), nil
		}
		return nil, nil
	case "ovs-vsctl list-ports br-eth1":
		if r.extraPortType != "" {
			return []byte("eth1\nextra\n"), nil
		}
		return []byte("eth1\n"), nil
	case "ovs-vsctl get Interface extra type":
		return []byte(r.extraPortType), nil
	case "ip -j address show dev br-eth1 scope global":
		if r.bridgeAddresses != "" {
			return []byte(r.bridgeAddresses), nil
		}
		return []byte(`[{"addr_info":[{"local":"10.80.1.2","prefixlen":24}]}]`), nil
	case "ip -j address show dev eth1 scope global":
		return []byte(`[{"addr_info":[]}]`), nil
	case "netplan apply":
		if r.applyErr != nil {
			return nil, r.applyErr
		}
		r.bridge = true
		r.missingRoute = false
		return nil, nil
	default:
		return nil, fmt.Errorf("unexpected command %s", command)
	}
}

func (r *configuredOVSRunner) RunSplitOutput(name string, args []string, options *nvkeutils.CommandOptions) (nvkeutils.CommandOutput, error) {
	output, err := r.Run(name, args, options)
	return nvkeutils.CommandOutput{Stdout: output}, err
}

func (*configuredOVSRunner) RunDetached(string, []string, *nvkeutils.CommandOptions) error {
	return errors.New("unexpected detached command")
}

func (r *configuredOVSRunner) commands() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func newConfiguredOVSServer(t *testing.T) (*NodeAgentServer, *configuredOVSRunner, string) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "etc/netplan/60-fabric.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(configuredOVSNetplan), 0o600))
	writeOpenVSwitchTestDriver(t, root, "eth1", "virtio_net")
	runner := &configuredOVSRunner{}
	return &NodeAgentServer{
		Runner: runner, Persister: &recordingPersister{}, NodeStatus: &NodeStatus{}, RootPath: root,
		openVSwitchSettleDelay: time.Nanosecond, openVSwitchConvergenceDelay: time.Millisecond,
	}, runner, path
}

func TestConfiguredOpenVSwitchPreservesManagementAndRepairsFabric(t *testing.T) {
	server, runner, path := newConfiguredOVSServer(t)
	input := configuredOpenVSwitchInput{UplinkInterface: "eth1", BridgeName: "br-eth1"}
	result, _, err := server.reconcileConfiguredOpenVSwitchLocked(t.Context(), input)
	require.NoError(t, err)
	assert.Equal(t, pb.OpenVSwitchReconciliationResult_OUTCOME_CONFIGURED, result.Outcome)
	assert.Equal(t, "eth1", result.UplinkInterface)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	original, err := parseOpenVSwitchNetplanDocument([]byte(configuredOVSNetplan))
	require.NoError(t, err)
	current, err := parseOpenVSwitchNetplanDocument(data)
	require.NoError(t, err)
	assert.Equal(t, original.ethernets["management"], current.ethernets["management"])
	assert.Contains(t, current.ethernets, "01-fabric")
	assert.Contains(t, current.bridges, "br-eth1")

	result, _, err = server.reconcileConfiguredOpenVSwitchLocked(t.Context(), input)
	require.NoError(t, err)
	assert.Equal(t, pb.OpenVSwitchReconciliationResult_OUTCOME_UNCHANGED, result.Outcome)
	assert.Equal(t, 1, countConfiguredOVSCommand(runner.commands(), "netplan apply"))

	runner.missingRoute = true
	result, _, err = server.reconcileConfiguredOpenVSwitchLocked(t.Context(), input)
	require.NoError(t, err)
	assert.Equal(t, pb.OpenVSwitchReconciliationResult_OUTCOME_CONFIGURED, result.Outcome)
	assert.Equal(t, 2, countConfiguredOVSCommand(runner.commands(), "netplan apply"))
	assert.NotContains(t, runner.commands(), "ip -j route show default")
}

func TestConfiguredOpenVSwitchPersistsAlreadyLiveBridge(t *testing.T) {
	server, runner, path := newConfiguredOVSServer(t)
	runner.bridge = true
	result, _, err := server.reconcileConfiguredOpenVSwitchLocked(t.Context(), configuredOpenVSwitchInput{"eth1", "br-eth1"})
	require.NoError(t, err)
	assert.Equal(t, pb.OpenVSwitchReconciliationResult_OUTCOME_CONFIGURED, result.Outcome)
	assert.Equal(t, 1, countConfiguredOVSCommand(runner.commands(), "netplan apply"))
	assert.Equal(t, 1, countConfiguredOVSCommand(runner.commands(), "ip -j address show dev br-eth1 scope global"),
		"verify runtime networking only after applying a changed configuration")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "br-eth1:")
}

func TestConfiguredOpenVSwitchRejectsUnsafeHostBeforeMutation(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*testing.T, *NodeAgentServer, *configuredOVSRunner)
	}{
		{"missing NIC", func(_ *testing.T, _ *NodeAgentServer, r *configuredOVSRunner) { r.missingUplink = true }},
		{"virtual bridge", func(_ *testing.T, _ *NodeAgentServer, r *configuredOVSRunner) { r.virtualKind = "bridge" }},
		{"bridge name already in use", func(_ *testing.T, _ *NodeAgentServer, r *configuredOVSRunner) { r.bridgeNameInUse = true }},
		{"another physical port", func(_ *testing.T, _ *NodeAgentServer, r *configuredOVSRunner) {
			r.bridge = true
			r.extraPortType = "system"
		}},
		{"ifupdown", func(t *testing.T, s *NodeAgentServer, _ *configuredOVSRunner) {
			path := filepath.Join(s.RootPath, "etc/network/interfaces")
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
			require.NoError(t, os.WriteFile(path, []byte("auto eth0\niface eth0 inet dhcp\n"), 0o600))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, runner, path := newConfiguredOVSServer(t)
			test.setup(t, server, runner)
			_, _, err := server.reconcileConfiguredOpenVSwitchLocked(t.Context(), configuredOpenVSwitchInput{"eth1", "br-eth1"})
			require.Error(t, err)
			assert.Equal(t, codes.FailedPrecondition, status.Code(err))
			assert.NotContains(t, runner.commands(), "netplan apply")
			assert.NotContains(t, runner.commands(), "systemctl restart ovs-vswitchd")
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, configuredOVSNetplan, string(data))
		})
	}
}

func TestConfiguredOpenVSwitchRejectsDependentVLANBeforeMutation(t *testing.T) {
	server, runner, path := newConfiguredOVSServer(t)
	vlanPath := filepath.Join(filepath.Dir(path), "70-management-vlan.yaml")
	const vlanConfig = `network:
  version: 2
  vlans:
    management-vlan:
      id: 100
      link: 01-fabric
      addresses: [192.0.2.10/24]
`
	require.NoError(t, os.WriteFile(vlanPath, []byte(vlanConfig), 0o600))
	_, _, err := server.reconcileConfiguredOpenVSwitchLocked(t.Context(), configuredOpenVSwitchInput{"eth1", "br-eth1"})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Contains(t, err.Error(), "netplan interface 01-fabric is the parent link of vlan management-vlan")
	for _, command := range []string{
		"systemctl enable --now openvswitch-switch", "systemctl restart ovs-vswitchd", "netplan generate", "netplan apply",
	} {
		assert.NotContains(t, runner.commands(), command)
	}
	for file, original := range map[string]string{path: configuredOVSNetplan, vlanPath: vlanConfig} {
		data, err := os.ReadFile(file)
		require.NoError(t, err)
		assert.Equal(t, original, string(data))
	}
}

func TestConfiguredOpenVSwitchChecksIfupdownContent(t *testing.T) {
	for _, test := range []struct {
		name, main, fragment string
		wantError            bool
	}{
		{name: "empty files"},
		{name: "comments only", main: "# Managed by Netplan\n\n", fragment: "  # No interfaces defined here\n"},
		{name: "loopback only", main: "auto\tlo\niface lo inet loopback\niface lo inet6 loopback\n"},
		{name: "standard glob include", main: "source /etc/network/interfaces.d/*\n", fragment: "allow-auto lo\niface lo inet loopback\n"},
		{name: "standard directory include", main: "source-directory /etc/network/interfaces.d\n", fragment: "# Managed by Netplan\n"},
		{name: "relative glob include", main: "source interfaces.d/*\n", fragment: "iface lo inet loopback\n"},
		{name: "relative directory include", main: "source-directory interfaces.d\n", fragment: "iface lo inet6 loopback\n"},
		{name: "real interface in main file", main: "auto eth1\niface eth1 inet dhcp\n", wantError: true},
		{name: "real interface in included file", main: "source /etc/network/interfaces.d/*\n", fragment: "iface eth1 inet dhcp\n", wantError: true},
		{name: "external include", main: "source /etc/other-network-config/*\n", wantError: true},
		{name: "include in fragment", main: "source /etc/network/interfaces.d/*\n", fragment: "source interfaces.d/*\n", wantError: true},
		{name: "loopback command", main: "auto lo\niface lo inet loopback\n  post-up ip link set eth1 down\n", wantError: true},
		{name: "additional auto interface", main: "auto lo eth1\niface lo inet loopback\n", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, runner, netplanPath := newConfiguredOVSServer(t)
			files := map[string]string{
				filepath.Join(server.RootPath, "etc/network/interfaces"):               test.main,
				filepath.Join(server.RootPath, "etc/network/interfaces.d/placeholder"): test.fragment,
			}
			for path, content := range files {
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
			}
			result, _, err := server.reconcileConfiguredOpenVSwitchLocked(t.Context(), configuredOpenVSwitchInput{"eth1", "br-eth1"})
			if test.wantError {
				require.Error(t, err)
				assert.Equal(t, codes.FailedPrecondition, status.Code(err))
				assert.Contains(t, err.Error(), "ifupdown configuration")
				assert.Empty(t, runner.commands())
				data, err := os.ReadFile(netplanPath)
				require.NoError(t, err)
				assert.Equal(t, configuredOVSNetplan, string(data))
			} else {
				require.NoError(t, err)
				assert.Equal(t, pb.OpenVSwitchReconciliationResult_OUTCOME_CONFIGURED, result.Outcome)
			}
			for path, content := range files {
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, content, string(data))
			}
		})
	}
}

func TestConfiguredOpenVSwitchRetainsOVNPatchPorts(t *testing.T) {
	server, runner, _ := newConfiguredOVSServer(t)
	runner.bridge = true
	runner.extraPortType = "patch"
	_, _, err := server.reconcileConfiguredOpenVSwitchLocked(t.Context(), configuredOpenVSwitchInput{"eth1", "br-eth1"})
	require.NoError(t, err)
}

func TestStartConfiguredOpenVSwitchValidatesBeforeAcceptance(t *testing.T) {
	for _, name := range []string{"", loopbackInterface, openVSwitchParentDirectory, "-eth1", "eth/1", "eth 1", strings.Repeat("x", 16), "br-eth1"} {
		t.Run(fmt.Sprintf("uplink=%q", name), func(t *testing.T) {
			server, runner, _ := newConfiguredOVSServer(t)
			_, err := server.StartConfiguredOpenVSwitchReconciliation(t.Context(), &pb.StartConfiguredOpenVSwitchReconciliationRequest{
				OperationId: "configured", UplinkInterface: name, BridgeName: "br-eth1",
			})
			require.Error(t, err)
			assert.Equal(t, codes.InvalidArgument, status.Code(err))
			assert.Empty(t, runner.commands())
			assert.Empty(t, server.NodeStatus.NodeAgentOperations)
		})
	}
}

func TestStartConfiguredOpenVSwitchReusesCompletedOperation(t *testing.T) {
	server, runner, _ := newConfiguredOVSServer(t)
	req := &pb.StartConfiguredOpenVSwitchReconciliationRequest{OperationId: "configured", UplinkInterface: "eth1", BridgeName: "br-eth1"}
	_, err := server.StartConfiguredOpenVSwitchReconciliation(t.Context(), req)
	require.NoError(t, err)
	waitForConfiguredOVS(t, server, req.OperationId)
	_, err = server.StartConfiguredOpenVSwitchReconciliation(t.Context(), req)
	require.NoError(t, err)
	assert.Equal(t, 1, countConfiguredOVSCommand(runner.commands(), "netplan apply"))
}

func TestConfiguredOpenVSwitchReportsInterruptedOperationAfterAgentRestart(t *testing.T) {
	server, runner, path := newConfiguredOVSServer(t)
	interrupted := NodeAgentOperation{
		ID: "interrupted", Type: operationTypeOpenVSwitchReconciliation, Phase: NodeAgentOperationPhaseRunning, Step: "waiting_for_network",
	}
	server.NodeStatus.NodeAgentOperations = map[string]NodeAgentOperation{
		interrupted.ID: interrupted,
	}
	_, err := server.StartConfiguredOpenVSwitchReconciliation(t.Context(), &pb.StartConfiguredOpenVSwitchReconciliationRequest{
		OperationId: interrupted.ID, UplinkInterface: "eth1", BridgeName: "br-eth1",
	})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Contains(t, err.Error(), "no worker is active")
	assert.Equal(t, interrupted, server.NodeStatus.NodeAgentOperations[interrupted.ID])
	assert.Empty(t, server.activeOperations)
	assert.Empty(t, runner.commands())
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, configuredOVSNetplan, string(data))
}

func TestConfiguredOpenVSwitchAllowsDynamicDefaultRouteChanges(t *testing.T) {
	for _, test := range []struct {
		name    string
		dhcp4   bool
		routes4 string
		routes6 string
	}{
		{
			name:    "DHCP supplies a default route on the bridge",
			dhcp4:   true,
			routes4: `[{"dst":"default","gateway":"192.0.2.1","dev":"mgmt0","metric":100},{"dst":"default","gateway":"10.80.1.1","dev":"br-eth1","protocol":"dhcp"},{"dst":"10.80.0.0/16","gateway":"10.80.1.1","dev":"br-eth1"}]`,
		},
		{
			name:    "management DHCP gateway and metric change",
			routes4: `[{"dst":"default","gateway":"192.0.2.254","dev":"mgmt0","metric":50},{"dst":"10.80.0.0/16","gateway":"10.80.1.1","dev":"br-eth1"}]`,
		},
		{
			name:    "management default route is temporarily absent",
			routes4: `[{"dst":"10.80.0.0/16","gateway":"10.80.1.1","dev":"br-eth1"}]`,
		},
		{
			name:    "IPv6 router advertisement adds a management default route",
			routes6: `[{"dst":"default","gateway":"fe80::1","dev":"mgmt0","protocol":"ra","metric":1024}]`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, runner, path := newConfiguredOVSServer(t)
			if test.dhcp4 {
				config := strings.Replace(configuredOVSNetplan, "dhcp4: false\n      addresses: [10.80.1.2/24]", "dhcp4: true", 1)
				require.NoError(t, os.WriteFile(path, []byte(config), 0o600))
			}
			runner.routes4AfterApply = test.routes4
			runner.routes6AfterApply = test.routes6
			input := configuredOpenVSwitchInput{"eth1", "br-eth1"}
			result, _, err := server.reconcileConfiguredOpenVSwitchLocked(t.Context(), input)
			require.NoError(t, err)
			assert.Equal(t, pb.OpenVSwitchReconciliationResult_OUTCOME_CONFIGURED, result.Outcome)

			result, _, err = server.reconcileConfiguredOpenVSwitchLocked(t.Context(), input)
			require.NoError(t, err)
			assert.Equal(t, pb.OpenVSwitchReconciliationResult_OUTCOME_UNCHANGED, result.Outcome)
			assert.Equal(t, 1, countConfiguredOVSCommand(runner.commands(), "netplan apply"))
		})
	}
}

func TestConfiguredOpenVSwitchRequiresConfiguredDefaultRouteOnBridge(t *testing.T) {
	server, runner, path := newConfiguredOVSServer(t)
	config := configuredOVSNetplan + "        - {to: default, via: 10.80.1.1}\n"
	require.NoError(t, os.WriteFile(path, []byte(config), 0o600))
	input := configuredOpenVSwitchInput{"eth1", "br-eth1"}
	_, plan, err := server.reconcileConfiguredOpenVSwitchLocked(t.Context(), input)
	require.ErrorIs(t, err, errOpenVSwitchNotConverged)
	assert.Contains(t, err.Error(), "missing preserved route default")
	require.NotNil(t, plan)

	runner.routes4AfterApply = `[{"dst":"default","gateway":"192.0.2.1","dev":"mgmt0","metric":100},{"dst":"default","gateway":"10.80.1.1","dev":"br-eth1"},{"dst":"10.80.0.0/16","gateway":"10.80.1.1","dev":"br-eth1"}]`
	require.NoError(t, server.verifyOpenVSwitchPlan(*plan))
}

func TestConfiguredOpenVSwitchRespectsOptionalDHCPAddresses(t *testing.T) {
	const ipv4Only = `[{"addr_info":[{"local":"10.80.1.2","prefixlen":24}]}]`
	const ipv6Only = `[{"addr_info":[{"local":"2001:db8::2","prefixlen":64}]}]`
	const noAddresses = `[{"addr_info":[]}]`
	for _, test := range []struct {
		name        string
		extraConfig string
		addresses   string
		wantError   string
	}{
		{
			name: "optional IPv6 with working IPv4", extraConfig: "      optional-addresses: [ipv4-ll, dhcp6]\n",
			addresses: ipv4Only,
		},
		{
			name: "optional IPv4 with working IPv6", extraConfig: "      optional-addresses: [dhcp4]\n",
			addresses: ipv6Only,
		},
		{
			name: "required IPv6 is still missing", addresses: ipv4Only, wantError: "waiting for DHCP",
		},
		{
			name: "required IPv4 is still missing", addresses: ipv6Only, wantError: "waiting for DHCP",
		},
		{
			name: "both families optional without acquired addresses", extraConfig: "      optional-addresses: [dhcp4, dhcp6]\n",
			addresses: noAddresses,
		},
		{
			name: "optional IPv6 does not excuse missing IPv4", extraConfig: "      optional-addresses: [dhcp6]\n",
			addresses: noAddresses, wantError: "waiting for DHCP",
		},
		{
			name:        "optional DHCP does not excuse a missing static address",
			extraConfig: "      optional-addresses: [dhcp4, dhcp6]\n      addresses: [10.80.1.2/24]\n",
			addresses:   noAddresses, wantError: "missing address 10.80.1.2/24",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, runner, path := newConfiguredOVSServer(t)
			prefix, _, found := strings.Cut(configuredOVSNetplan, "      dhcp4: false\n")
			require.True(t, found)
			config := prefix + "      dhcp4: true\n      dhcp6: true\n" + test.extraConfig
			require.NoError(t, os.WriteFile(path, []byte(config), 0o600))
			runner.bridgeAddresses = test.addresses
			result, _, err := server.reconcileConfiguredOpenVSwitchLocked(t.Context(), configuredOpenVSwitchInput{"eth1", "br-eth1"})
			if test.wantError != "" {
				require.ErrorIs(t, err, errOpenVSwitchNotConverged)
				assert.Contains(t, err.Error(), test.wantError)
			} else {
				require.NoError(t, err)
				assert.Equal(t, pb.OpenVSwitchReconciliationResult_OUTCOME_CONFIGURED, result.Outcome)
			}
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			document, err := parseOpenVSwitchNetplanDocument(data)
			require.NoError(t, err)
			bridge := document.bridges["br-eth1"].(map[string]any)
			assert.Equal(t, true, bridge["dhcp4"])
			assert.Equal(t, true, bridge["dhcp6"])
			assert.NotContains(t, runner.commands(), "ip -N -j -4 route show table all")
			assert.NotContains(t, runner.commands(), "ip -N -j -6 route show table all")
		})
	}
}

func TestConfiguredOpenVSwitchRouteMatchesHostRoutesAndNumericTables(t *testing.T) {
	for _, test := range []struct{ address, prefix string }{
		{"192.0.2.10", "192.0.2.10/32"},
		{"2001:db8::10", "2001:db8::10/128"},
	} {
		t.Run(test.address, func(t *testing.T) {
			actual := openVSwitchRoute{Destination: test.address, Device: "br-eth1", Table: float64(100)}
			expected := openVSwitchRoute{Destination: test.prefix, Device: "br-eth1", Table: 100}
			assert.True(t, openVSwitchRouteMatches(actual, expected))
			actual.Table = float64(101)
			assert.False(t, openVSwitchRouteMatches(actual, expected))
		})
	}
}

func TestConfiguredOpenVSwitchRouteMatchesGatewayAddresses(t *testing.T) {
	for _, test := range []struct {
		name, actualGateway, expectedGateway string
		matches                              bool
	}{
		{name: "expanded IPv6", actualGateway: "2001:db8::1", expectedGateway: "2001:0db8:0:0:0:0:0:1", matches: true},
		{name: "uppercase IPv6", actualGateway: "2001:db8::1", expectedGateway: "2001:DB8::1", matches: true},
		{name: "different IPv6 gateways", actualGateway: "2001:db8::1", expectedGateway: "2001:db8::2"},
		{name: "same IPv4 gateway", actualGateway: "192.0.2.1", expectedGateway: "192.0.2.1", matches: true},
		{name: "different IPv4 gateways", actualGateway: "192.0.2.1", expectedGateway: "192.0.2.2"},
		{name: "no gateway", matches: true},
		{name: "missing gateway", expectedGateway: "2001:db8::1"},
		{name: "unexpected gateway", actualGateway: "2001:db8::1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			actual := openVSwitchRoute{Destination: openVSwitchDefaultRoute, Gateway: test.actualGateway, Device: "br-eth1"}
			expected := openVSwitchRoute{Destination: openVSwitchDefaultRoute, Gateway: test.expectedGateway, Device: "br-eth1"}
			assert.Equal(t, test.matches, openVSwitchRouteMatches(actual, expected))
		})
	}
}

func TestConfiguredOpenVSwitchRoutePreservesAddressFamily(t *testing.T) {
	for _, test := range []struct {
		name, destination, gateway, family string
		invalid                            bool
	}{
		{name: "IPv4 gatewayless default", destination: openVSwitchIPv4DefaultPrefix, family: openVSwitchIPv4Family},
		{name: "IPv6 gatewayless default", destination: openVSwitchIPv6DefaultPrefix, family: openVSwitchIPv6Family},
		{name: "IPv4 default gateway", destination: openVSwitchDefaultRoute, gateway: "192.0.2.1", family: openVSwitchIPv4Family},
		{name: "IPv6 default gateway", destination: openVSwitchDefaultRoute, gateway: "2001:db8::1", family: openVSwitchIPv6Family},
		{name: "ambiguous gatewayless default", destination: openVSwitchDefaultRoute, invalid: true},
		{name: "gateway family mismatch", destination: openVSwitchIPv6DefaultPrefix, gateway: "192.0.2.1", invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			expected, err := configuredOpenVSwitchRoute(map[string]any{openVSwitchRouteTo: test.destination, openVSwitchRouteVia: test.gateway}, "br-eth1")
			if test.invalid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.family, expected.Family)
			actual := expected
			assert.True(t, openVSwitchRouteMatches(actual, expected))
			if actual.Family == openVSwitchIPv4Family {
				actual.Family = openVSwitchIPv6Family
			} else {
				actual.Family = openVSwitchIPv4Family
			}
			assert.False(t, openVSwitchRouteMatches(actual, expected), "opposite-family defaults cannot satisfy preserved intent")
		})
	}
}

func waitForConfiguredOVS(t *testing.T, server *NodeAgentServer, id string) {
	t.Helper()
	require.Eventually(t, func() bool {
		response, err := server.GetOpenVSwitchReconciliationStatus(t.Context(), &pb.GetOpenVSwitchReconciliationStatusRequest{OperationId: id})
		return err == nil && response.GetOperation().GetOperation().GetPhase() == pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED
	}, time.Second, time.Millisecond)
}

func countConfiguredOVSCommand(commands []string, want string) int {
	count := 0
	for _, command := range commands {
		if command == want {
			count++
		}
	}
	return count
}
