// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"sigs.k8s.io/yaml"

	pb "github.com/NVIDIA/nke/agent/pkg/agent/proto"
	nkeutils "github.com/NVIDIA/nke/agent/pkg/utils"
)

type openVSwitchCommandResult struct {
	output []byte
	err    error
}

type openVSwitchTestRunner struct {
	t         *testing.T
	responses map[string][]openVSwitchCommandResult
	calls     []string
}

func (r *openVSwitchTestRunner) Run(name string, args []string, _ *nkeutils.CommandOptions) ([]byte, error) {
	r.t.Helper()
	key := strings.Join(append([]string{name}, args...), " ")
	r.calls = append(r.calls, key)
	responses := r.responses[key]
	if len(responses) == 0 {
		r.t.Fatalf("unexpected command %q", key)
	}
	r.responses[key] = responses[1:]
	return responses[0].output, responses[0].err
}

func (r *openVSwitchTestRunner) RunSplitOutput(name string, args []string, opts *nkeutils.CommandOptions) (nkeutils.CommandOutput, error) {
	output, err := r.Run(name, args, opts)
	return nkeutils.CommandOutput{Stdout: output}, err
}

func (*openVSwitchTestRunner) RunDetached(string, []string, *nkeutils.CommandOptions) error {
	return errors.New("unexpected detached command")
}

func TestReconcileOpenVSwitchRejectsNilRequest(t *testing.T) {
	server := &NodeAgentServer{}
	_, err := server.ReconcileOpenVSwitch(context.Background(), nil)
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestReconcileOpenVSwitchReturnsUnchangedForLiveBridge(t *testing.T) {
	runner := newOpenVSwitchTestRunner(t, map[string][]openVSwitchCommandResult{
		"dpkg-query --show --showformat=${db:Status-Abbrev} openvswitch-switch": {{output: []byte("ii ")}},
		"systemctl enable --now openvswitch-switch":                             {{}},
		"ip -j route show default":                                              {{output: []byte(`[{"dst":"default","dev":"breth0"}]`)}},
		"ovs-vsctl list-br":                                                     {{output: []byte("breth0\n")}},
	})
	server := &NodeAgentServer{Runner: runner}

	response, err := server.ReconcileOpenVSwitch(context.Background(), &pb.ReconcileOpenVSwitchRequest{})

	require.NoError(t, err)
	assert.Equal(t, pb.ReconcileOpenVSwitchResponse_OUTCOME_UNCHANGED, response.GetOutcome())
	assert.Equal(t, "breth0", response.GetBridgeName())
	assert.NotContains(t, runner.calls, "systemctl restart ovs-vswitchd")
}

func TestReconcileOpenVSwitchTransformsNetplanAndVerifiesBridge(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "etc/netplan"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "etc/openvswitch"), 0o755))
	configPath := filepath.Join(root, "etc/netplan/50-cloud-init.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`network:
  version: 2
  ethernets:
    uplink:
      match:
        macaddress: "00:11:22:33:44:55"
      set-name: eth0
      dhcp4: true
      nameservers:
        addresses: [10.0.0.53]
`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "etc/openvswitch/system-id.conf"), []byte("duplicate"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "etc/openvswitch/install_uuid.conf"), []byte("duplicate"), 0o600))

	runner := newOpenVSwitchTestRunner(t, map[string][]openVSwitchCommandResult{
		"dpkg-query --show --showformat=${db:Status-Abbrev} openvswitch-switch": {{output: []byte("ii ")}},
		"systemctl enable --now openvswitch-switch":                             {{}},
		"ip -j route show default": {
			{output: []byte(`[{"dst":"default","gateway":"10.0.0.1","dev":"eth0"}]`)},
			{output: []byte(`[{"dst":"default","gateway":"10.0.0.1","dev":"breth0"}]`)},
		},
		"ovs-vsctl list-br": {
			{output: nil},
			{output: []byte("breth0\n")},
		},
		"ip -j link show dev eth0":       {{output: []byte(`[{"ifname":"eth0","address":"00:11:22:33:44:55","mtu":9000}]`)}},
		"systemctl restart ovs-vswitchd": {{}},
		"netplan generate":               {{}},
		"netplan apply":                  {{}},
	})
	server := &NodeAgentServer{Runner: runner, RootPath: root, openVSwitchSettleDelay: 1}

	response, err := server.ReconcileOpenVSwitch(context.Background(), &pb.ReconcileOpenVSwitchRequest{})

	require.NoError(t, err)
	assert.Equal(t, pb.ReconcileOpenVSwitchResponse_OUTCOME_CONFIGURED, response.GetOutcome())
	assert.Equal(t, pb.ReconcileOpenVSwitchResponse_NETWORK_BACKEND_NETPLAN, response.GetNetworkBackend())
	assert.Equal(t, "breth0", response.GetBridgeName())
	assert.Equal(t, "eth0", response.GetUplinkInterface())
	_, err = os.Stat(filepath.Join(root, "etc/openvswitch/system-id.conf"))
	assert.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(filepath.Join(root, "etc/openvswitch/install_uuid.conf"))
	assert.ErrorIs(t, err, os.ErrNotExist)

	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	config := map[string]any{}
	require.NoError(t, yaml.Unmarshal(data, &config))
	network := config["network"].(map[string]any)
	ethernets := network["ethernets"].(map[string]any)
	bridges := network["bridges"].(map[string]any)
	assert.NotContains(t, ethernets, "uplink")
	physical := ethernets["eth0"].(map[string]any)
	assert.Equal(t, float64(9000), physical["mtu"])
	bridge := bridges["breth0"].(map[string]any)
	assert.Equal(t, true, bridge["dhcp4"])
	assert.NotContains(t, bridge, "match")
	assert.NotContains(t, bridge, "set-name")
	assert.Contains(t, bridge, "openvswitch")
	assert.Equal(t, []any{"eth0"}, bridge["interfaces"])
	assert.Equal(t, map[string]any{"addresses": []any{"10.0.0.53"}}, bridge["nameservers"])
}

func TestReconcileOpenVSwitchResumesWrittenNetplanAfterInterruptedApply(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "etc/netplan"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "etc/netplan/50-cloud-init.yaml"), []byte(`network:
  version: 2
  ethernets:
    eth0:
      mtu: 1500
      match:
        macaddress: 00:11:22:33:44:55
  bridges:
    breth0:
      dhcp4: true
      interfaces: [eth0]
      openvswitch: {}
`), 0o600))
	runner := newOpenVSwitchTestRunner(t, map[string][]openVSwitchCommandResult{
		"dpkg-query --show --showformat=${db:Status-Abbrev} openvswitch-switch": {{output: []byte("ii ")}},
		"systemctl enable --now openvswitch-switch":                             {{}},
		"ip -j route show default": {
			{output: []byte(`[{"dst":"default","gateway":"10.0.0.1","dev":"eth0"}]`)},
			{output: []byte(`[{"dst":"default","gateway":"10.0.0.1","dev":"breth0"}]`)},
		},
		"ovs-vsctl list-br": {
			// The bridge may exist before the default route moves when the
			// previous RPC is interrupted mid-apply. Presence alone is not
			// convergence; the retry must apply and verify the written config.
			{output: []byte("breth0\n")},
			{output: []byte("breth0\n")},
		},
		"ip -j link show dev eth0":       {{output: []byte(`[{"ifname":"eth0","address":"00:11:22:33:44:55","mtu":1500}]`)}},
		"systemctl restart ovs-vswitchd": {{}},
		"netplan generate":               {{}},
		"netplan apply":                  {{}},
	})
	server := &NodeAgentServer{Runner: runner, RootPath: root, openVSwitchSettleDelay: 1}

	response, err := server.ReconcileOpenVSwitch(context.Background(), &pb.ReconcileOpenVSwitchRequest{})

	require.NoError(t, err)
	assert.Equal(t, pb.ReconcileOpenVSwitchResponse_OUTCOME_CONFIGURED, response.GetOutcome())
	assert.Equal(t, pb.ReconcileOpenVSwitchResponse_NETWORK_BACKEND_NETPLAN, response.GetNetworkBackend())
}

func TestReconcileOpenVSwitchConfiguresIfupdownAndPinsLocalMAC(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "etc/network/interfaces.d"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "etc/openvswitch"), 0o755))
	uplinkPath := filepath.Join(root, "etc/network/interfaces.d/ifcfg-eth0")
	require.NoError(t, os.WriteFile(uplinkPath, []byte(`auto eth0
iface eth0 inet static
  address 10.0.0.10
  netmask 255.255.255.0
  gateway 10.0.0.1
`), 0o600))
	runner := newOpenVSwitchTestRunner(t, map[string][]openVSwitchCommandResult{
		"dpkg-query --show --showformat=${db:Status-Abbrev} openvswitch-switch": {{output: []byte("ii ")}},
		"systemctl enable --now openvswitch-switch":                             {{}},
		"ip -j route show default": {
			{output: []byte(`[{"dst":"default","gateway":"10.0.0.1","dev":"eth0"}]`)},
			{output: []byte(`[{"dst":"default","gateway":"10.0.0.1","dev":"breth0"}]`)},
		},
		"ovs-vsctl list-br": {
			{output: nil},
			{output: []byte("breth0\n")},
		},
		"ip -j link show dev eth0":                                          {{output: []byte(`[{"ifname":"eth0","address":"02:11:22:33:44:55","mtu":1500}]`)}},
		"systemctl restart ovs-vswitchd":                                    {{}},
		"ovs-vsctl --may-exist add-br breth0":                               {{}},
		"ovs-vsctl set bridge breth0 other-config:hwaddr=02:11:22:33:44:55": {{}},
		"ovs-vsctl --may-exist add-port breth0 eth0":                        {{}},
		"ip addr replace 10.0.0.10/24 dev breth0":                           {{}},
		"ip link set breth0 up":                                             {{}},
		"ip link set breth0 mtu 1500":                                       {{}},
		"ip route replace default via 10.0.0.1 dev breth0 onlink":           {{}},
		"ip addr flush dev eth0":                                            {{}},
	})
	server := &NodeAgentServer{Runner: runner, RootPath: root}

	response, err := server.ReconcileOpenVSwitch(context.Background(), &pb.ReconcileOpenVSwitchRequest{})

	require.NoError(t, err)
	assert.Equal(t, pb.ReconcileOpenVSwitchResponse_OUTCOME_CONFIGURED, response.GetOutcome())
	assert.Equal(t, pb.ReconcileOpenVSwitchResponse_NETWORK_BACKEND_IFUPDOWN, response.GetNetworkBackend())
	uplink, err := os.ReadFile(uplinkPath)
	require.NoError(t, err)
	assert.Equal(t, "auto eth0\niface eth0 inet manual\n\tmtu 1500\n", string(uplink))
	bridge, err := os.ReadFile(filepath.Join(root, "etc/network/interfaces.d/ifcfg-breth0"))
	require.NoError(t, err)
	assert.Contains(t, string(bridge), "iface breth0 inet static")
	assert.Contains(t, string(bridge), "ovs_ports eth0")
}

func TestReconcileOpenVSwitchDoesNotHideOperationalErrors(t *testing.T) {
	runner := newOpenVSwitchTestRunner(t, map[string][]openVSwitchCommandResult{
		"dpkg-query --show --showformat=${db:Status-Abbrev} openvswitch-switch": {{output: []byte("ii ")}},
		"systemctl enable --now openvswitch-switch":                             {{err: errors.New("systemd unavailable")}},
	})
	server := &NodeAgentServer{Runner: runner}

	_, err := server.ReconcileOpenVSwitch(context.Background(), &pb.ReconcileOpenVSwitchRequest{})

	require.Error(t, err)
	assert.Equal(t, codes.Internal, status.Code(err))
	assert.Contains(t, err.Error(), "systemd unavailable")
}

func newOpenVSwitchTestRunner(t *testing.T, responses map[string][]openVSwitchCommandResult) *openVSwitchTestRunner {
	t.Helper()
	return &openVSwitchTestRunner{t: t, responses: responses}
}
