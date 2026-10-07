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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"sigs.k8s.io/yaml"

	pb "github.com/dsx-ai-factory/nke/agent/pkg/agent/proto"
	nkeutils "github.com/dsx-ai-factory/nke/agent/pkg/utils"
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

func TestStartOpenVSwitchReconciliationRejectsNilRequest(t *testing.T) {
	server := &NodeAgentServer{}
	_, err := server.StartOpenVSwitchReconciliation(context.Background(), nil)
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestOpenVSwitchReconciliationReturnsUnchangedForLiveBridge(t *testing.T) {
	runner := newOpenVSwitchTestRunner(t, map[string][]openVSwitchCommandResult{
		"dpkg-query --show --showformat=${db:Status-Abbrev} openvswitch-switch": {{output: []byte("ii ")}},
		"systemctl enable --now openvswitch-switch":                             {{}},
		"ip -j route show default":                                              {{output: []byte(`[{"dst":"default","dev":"breth0"}]`)}},
		"ovs-vsctl list-br":                                                     {{output: []byte("breth0\n")}},
	})
	server := &NodeAgentServer{Runner: runner}

	result, _, err := server.reconcileOpenVSwitchLocked(context.Background())

	require.NoError(t, err)
	assert.Equal(t, pb.OpenVSwitchReconciliationResult_OUTCOME_UNCHANGED, result.GetOutcome())
	assert.Equal(t, "breth0", result.GetBridgeName())
	assert.NotContains(t, runner.calls, "systemctl restart ovs-vswitchd")
}

func TestOpenVSwitchReconciliationTransformsNetplanAndVerifiesBridge(t *testing.T) {
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

	result, _, err := server.reconcileOpenVSwitchLocked(context.Background())

	require.NoError(t, err)
	assert.Equal(t, pb.OpenVSwitchReconciliationResult_OUTCOME_CONFIGURED, result.GetOutcome())
	assert.Equal(t, pb.OpenVSwitchReconciliationResult_NETWORK_BACKEND_NETPLAN, result.GetNetworkBackend())
	assert.Equal(t, "breth0", result.GetBridgeName())
	assert.Equal(t, "eth0", result.GetUplinkInterface())
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

func TestOpenVSwitchNetplanMatchNamePatterns(t *testing.T) {
	link := openVSwitchLink{Name: "enp1s0", Address: "00:11:22:33:44:55"}
	tests := map[string]struct {
		name string
		want bool
	}{
		"exact name":       {name: "enp1s0", want: true},
		"wildcard name":    {name: "e*", want: true},
		"nonmatching name": {name: "eth*", want: false},
		"invalid pattern":  {name: "[", want: false},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			stanza := map[string]any{"match": map[string]any{"name": test.name}}
			assert.Equal(t, test.want, openVSwitchStanzaMatchesLink(stanza, link))
		})
	}
}

func TestOpenVSwitchNetdevDatapathKeepsManagementUplink(t *testing.T) {
	t.Setenv(openVSwitchDatapathTypeEnv, openVSwitchNetdevDatapath)
	runner := newOpenVSwitchTestRunner(t, map[string][]openVSwitchCommandResult{
		"dpkg-query --show --showformat=${db:Status-Abbrev} openvswitch-switch": {{output: []byte("ii ")}},
		"systemctl enable --now openvswitch-switch":                             {{}},
		"ip -j route show default": {{
			output: []byte(`[{"dst":"default","gateway":"10.0.0.1","dev":"eth0"}]`),
		}},
		"ovs-vsctl set Open_vSwitch . external_ids:ovn-bridge-datapath-type=netdev":   {{}},
		"ovs-vsctl --if-exists get Open_vSwitch . other_config:userspace-tso-enable":  {{}},
		"ovs-vsctl set Open_vSwitch . other_config:userspace-tso-enable=true":         {{}},
		"systemctl restart ovs-vswitchd":                                              {{}},
		"ovs-vsctl --may-exist add-br br-ex -- set bridge br-ex datapath_type=netdev": {{}},
	})
	server := &NodeAgentServer{Runner: runner}

	result, plan, err := server.reconcileOpenVSwitchLocked(context.Background())
	require.NoError(t, err)
	assert.Nil(t, plan)
	assert.Equal(t, pb.OpenVSwitchReconciliationResult_OUTCOME_CONFIGURED, result.GetOutcome())
	assert.Empty(t, result.GetBridgeName())
	assert.Equal(t, "eth0", result.GetUplinkInterface())
	assert.Equal(t, pb.OpenVSwitchReconciliationResult_NETWORK_BACKEND_NETPLAN, result.GetNetworkBackend())
	assert.NotContains(t, runner.calls, "netplan apply")
	assert.NotContains(t, runner.calls, "ovs-vsctl --may-exist add-br breth0")
	assert.Contains(t, runner.calls, "ovs-vsctl set Open_vSwitch . other_config:userspace-tso-enable=true")
	assert.Contains(t, runner.calls, "systemctl restart ovs-vswitchd")
	assert.Contains(t, runner.calls, "ovs-vsctl --may-exist add-br br-ex -- set bridge br-ex datapath_type=netdev")
}

func TestOpenVSwitchUserspaceTSO(t *testing.T) {
	const getTSO = "ovs-vsctl --if-exists get Open_vSwitch . other_config:userspace-tso-enable"
	const setTSO = "ovs-vsctl set Open_vSwitch . other_config:userspace-tso-enable=true"
	const restart = "systemctl restart ovs-vswitchd"
	tests := map[string]struct {
		responses   map[string][]openVSwitchCommandResult
		wantCalls   []string
		errorString string
	}{
		"already enabled leaves ovs-vswitchd running": {
			responses: map[string][]openVSwitchCommandResult{getTSO: {{output: []byte("\"true\"\n")}}},
			wantCalls: []string{getTSO},
		},
		"unset enables and restarts ovs-vswitchd": {
			responses: map[string][]openVSwitchCommandResult{getTSO: {{}}, setTSO: {{}}, restart: {{}}},
			wantCalls: []string{getTSO, setTSO, restart},
		},
		"disabled enables and restarts ovs-vswitchd": {
			responses: map[string][]openVSwitchCommandResult{
				getTSO: {{output: []byte("\"false\"\n")}}, setTSO: {{}}, restart: {{}},
			},
			wantCalls: []string{getTSO, setTSO, restart},
		},
		"restart failure restores an unset value": {
			responses: map[string][]openVSwitchCommandResult{
				getTSO: {{}}, setTSO: {{}}, restart: {{err: errors.New("unit failed")}},
				"ovs-vsctl remove Open_vSwitch . other_config userspace-tso-enable": {{}},
			},
			wantCalls: []string{
				getTSO, setTSO, restart,
				"ovs-vsctl remove Open_vSwitch . other_config userspace-tso-enable",
			},
			errorString: "restart ovs-vswitchd to apply userspace-tso-enable",
		},
		"restart failure restores the previous value": {
			responses: map[string][]openVSwitchCommandResult{
				getTSO: {{output: []byte("\"false\"\n")}}, setTSO: {{}}, restart: {{err: errors.New("unit failed")}},
				"ovs-vsctl set Open_vSwitch . other_config:userspace-tso-enable=false": {{}},
			},
			wantCalls: []string{
				getTSO, setTSO, restart,
				"ovs-vsctl set Open_vSwitch . other_config:userspace-tso-enable=false",
			},
			errorString: "restart ovs-vswitchd to apply userspace-tso-enable",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			runner := newOpenVSwitchTestRunner(t, test.responses)
			server := &NodeAgentServer{Runner: runner}

			err := server.ensureOpenVSwitchUserspaceTSO()

			if test.errorString != "" {
				require.ErrorContains(t, err, test.errorString)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, test.wantCalls, runner.calls)
		})
	}
}

func TestPrepareOpenVSwitchRejectsUnsupportedDatapath(t *testing.T) {
	t.Setenv(openVSwitchDatapathTypeEnv, "system")
	server := &NodeAgentServer{}

	_, _, err := server.reconcileOpenVSwitchDatapathOverride(context.Background())
	require.EqualError(t, err, `unsupported Open vSwitch datapath type "system"`)
}

func TestOpenVSwitchReconciliationResumesWrittenNetplanAfterInterruptedApply(t *testing.T) {
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

	result, _, err := server.reconcileOpenVSwitchLocked(context.Background())

	require.NoError(t, err)
	assert.Equal(t, pb.OpenVSwitchReconciliationResult_OUTCOME_CONFIGURED, result.GetOutcome())
	assert.Equal(t, pb.OpenVSwitchReconciliationResult_NETWORK_BACKEND_NETPLAN, result.GetNetworkBackend())
}

func TestOpenVSwitchReconciliationConfiguresIfupdownAndPinsLocalMAC(t *testing.T) {
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

	result, _, err := server.reconcileOpenVSwitchLocked(context.Background())

	require.NoError(t, err)
	assert.Equal(t, pb.OpenVSwitchReconciliationResult_OUTCOME_CONFIGURED, result.GetOutcome())
	assert.Equal(t, pb.OpenVSwitchReconciliationResult_NETWORK_BACKEND_IFUPDOWN, result.GetNetworkBackend())
	uplink, err := os.ReadFile(uplinkPath)
	require.NoError(t, err)
	assert.Equal(t, "auto eth0\niface eth0 inet manual\n\tmtu 1500\n", string(uplink))
	bridge, err := os.ReadFile(filepath.Join(root, "etc/network/interfaces.d/ifcfg-breth0"))
	require.NoError(t, err)
	assert.Contains(t, string(bridge), "iface breth0 inet static")
	assert.Contains(t, string(bridge), "ovs_ports eth0")
}

func TestOpenVSwitchReconciliationDoesNotHideOperationalErrors(t *testing.T) {
	runner := newOpenVSwitchTestRunner(t, map[string][]openVSwitchCommandResult{
		"dpkg-query --show --showformat=${db:Status-Abbrev} openvswitch-switch": {{output: []byte("ii ")}},
		"systemctl enable --now openvswitch-switch":                             {{err: errors.New("systemd unavailable")}},
	})
	server := &NodeAgentServer{Runner: runner}

	_, _, err := server.reconcileOpenVSwitchLocked(context.Background())

	require.Error(t, err)
	assert.Equal(t, codes.Internal, status.Code(err))
	assert.Contains(t, err.Error(), "systemd unavailable")
}

func TestStartOpenVSwitchReconciliationPersistsBeforeRunning(t *testing.T) {
	runner := newOpenVSwitchTestRunner(t, map[string][]openVSwitchCommandResult{
		"dpkg-query --show --showformat=${db:Status-Abbrev} openvswitch-switch": {{output: []byte("ii ")}},
		"systemctl enable --now openvswitch-switch":                             {{}},
		"ip -j route show default":                                              {{output: []byte(`[{"dst":"default","dev":"breth0"}]`)}},
		"ovs-vsctl list-br":                                                     {{output: []byte("breth0\n")}},
	})
	persister := &recordingPersister{}
	server := &NodeAgentServer{
		Runner:     runner,
		Persister:  persister,
		NodeStatus: &NodeStatus{},
	}

	response, err := server.StartOpenVSwitchReconciliation(
		context.Background(),
		&pb.StartOpenVSwitchReconciliationRequest{OperationId: "op-1"},
	)

	require.NoError(t, err)
	require.NotNil(t, response.GetOperation())
	assert.Equal(t, pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_RUNNING, response.GetOperation().GetOperation().GetPhase())
	snapshots := persister.Snapshots()
	require.NotEmpty(t, snapshots)
	assert.Equal(t, NodeAgentOperationPhaseRunning, snapshots[0].NodeAgentOperations["op-1"].Phase)

	require.Eventually(t, func() bool {
		statusResponse, statusErr := server.GetOpenVSwitchReconciliationStatus(
			context.Background(),
			&pb.GetOpenVSwitchReconciliationStatusRequest{OperationId: "op-1"},
		)
		return statusErr == nil &&
			statusResponse.GetOperation().GetOperation().GetPhase() == pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED
	}, time.Second, time.Millisecond)
	statusResponse, err := server.GetOpenVSwitchReconciliationStatus(
		context.Background(),
		&pb.GetOpenVSwitchReconciliationStatusRequest{OperationId: "op-1"},
	)
	require.NoError(t, err)
	assert.Equal(t, pb.OpenVSwitchReconciliationResult_OUTCOME_UNCHANGED, statusResponse.GetOperation().GetResult().GetOutcome())
	assert.Equal(t, "breth0", statusResponse.GetOperation().GetResult().GetBridgeName())
}

func TestStartOpenVSwitchReconciliationDoesNotRunWhenPersistenceFails(t *testing.T) {
	runner := newOpenVSwitchTestRunner(t, nil)
	server := &NodeAgentServer{
		Runner:     runner,
		Persister:  &recordingPersister{err: errors.New("disk unavailable")},
		NodeStatus: &NodeStatus{},
	}

	_, err := server.StartOpenVSwitchReconciliation(
		context.Background(),
		&pb.StartOpenVSwitchReconciliationRequest{OperationId: "op-1"},
	)

	require.Error(t, err)
	assert.Equal(t, codes.Internal, status.Code(err))
	assert.Empty(t, runner.calls)
}

func TestStartOpenVSwitchReconciliationRecoversPersistedRunningOperation(t *testing.T) {
	runner := newOpenVSwitchTestRunner(t, map[string][]openVSwitchCommandResult{
		"dpkg-query --show --showformat=${db:Status-Abbrev} openvswitch-switch": {{output: []byte("ii ")}},
		"systemctl enable --now openvswitch-switch":                             {{}},
		"ip -j route show default":                                              {{output: []byte(`[{"dst":"default","dev":"breth0"}]`)}},
		"ovs-vsctl list-br":                                                     {{output: []byte("breth0\n")}},
	})
	server := &NodeAgentServer{
		Runner:    runner,
		Persister: &recordingPersister{},
		NodeStatus: &NodeStatus{NodeAgentOperations: map[string]NodeAgentOperation{
			"op-1": {
				ID:    "op-1",
				Type:  operationTypeOpenVSwitchReconciliation,
				Phase: NodeAgentOperationPhaseRunning,
				Step:  "waiting_for_network",
			},
		}},
	}

	_, err := server.StartOpenVSwitchReconciliation(
		context.Background(),
		&pb.StartOpenVSwitchReconciliationRequest{OperationId: "op-1"},
	)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		statusResponse, statusErr := server.GetOpenVSwitchReconciliationStatus(
			context.Background(),
			&pb.GetOpenVSwitchReconciliationStatusRequest{OperationId: "op-1"},
		)
		return statusErr == nil &&
			statusResponse.GetOperation().GetOperation().GetPhase() == pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED
	}, time.Second, time.Millisecond)
}

// A FAILED reconciliation must run again on the next Start: the controller
// retries with the same operation ID, and the failure that stuck here in CI was
// one systemctl call made while systemd re-executed itself.
func TestStartOpenVSwitchReconciliationRestartsPersistedFailedOperation(t *testing.T) {
	runner := newOpenVSwitchTestRunner(t, map[string][]openVSwitchCommandResult{
		"dpkg-query --show --showformat=${db:Status-Abbrev} openvswitch-switch": {{output: []byte("ii ")}},
		"systemctl enable --now openvswitch-switch":                             {{}},
		"ip -j route show default":                                              {{output: []byte(`[{"dst":"default","dev":"breth0"}]`)}},
		"ovs-vsctl list-br":                                                     {{output: []byte("breth0\n")}},
	})
	server := &NodeAgentServer{
		Runner:    runner,
		Persister: &recordingPersister{},
		NodeStatus: &NodeStatus{NodeAgentOperations: map[string]NodeAgentOperation{
			"op-1": {
				ID:    "op-1",
				Type:  operationTypeOpenVSwitchReconciliation,
				Phase: NodeAgentOperationPhaseFailed,
				Step:  "reconciling",
			},
		}},
	}

	_, err := server.StartOpenVSwitchReconciliation(
		context.Background(),
		&pb.StartOpenVSwitchReconciliationRequest{OperationId: "op-1"},
	)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		statusResponse, statusErr := server.GetOpenVSwitchReconciliationStatus(
			context.Background(),
			&pb.GetOpenVSwitchReconciliationStatusRequest{OperationId: "op-1"},
		)
		return statusErr == nil &&
			statusResponse.GetOperation().GetOperation().GetPhase() == pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED
	}, time.Second, time.Millisecond)
}

func TestOpenVSwitchOperationWaitsForTransientDefaultRouteGap(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "etc/netplan"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "etc/openvswitch"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "etc/netplan/50-cloud-init.yaml"), []byte(`network:
  version: 2
  ethernets:
    eth0:
      dhcp4: true
`), 0o600))
	runner := newOpenVSwitchTestRunner(t, map[string][]openVSwitchCommandResult{
		"dpkg-query --show --showformat=${db:Status-Abbrev} openvswitch-switch": {{output: []byte("ii ")}},
		"systemctl enable --now openvswitch-switch":                             {{}},
		"ip -j route show default": {
			{output: []byte(`[{"dst":"default","gateway":"10.0.0.1","dev":"eth0"}]`)},
			{output: []byte(`[]`)},
			{output: []byte(`[{"dst":"default","gateway":"10.0.0.1","dev":"breth0"}]`)},
		},
		"ovs-vsctl list-br": {
			{output: nil},
			{output: []byte("breth0\n")},
			{output: []byte("breth0\n")},
		},
		"ip -j link show dev eth0":       {{output: []byte(`[{"ifname":"eth0","address":"00:11:22:33:44:55","mtu":1500}]`)}},
		"systemctl restart ovs-vswitchd": {{}},
		"netplan generate":               {{}},
		"netplan apply":                  {{}},
	})
	server := &NodeAgentServer{
		Runner:                      runner,
		Persister:                   &recordingPersister{},
		NodeStatus:                  &NodeStatus{},
		RootPath:                    root,
		openVSwitchSettleDelay:      time.Nanosecond,
		openVSwitchConvergenceDelay: time.Nanosecond,
	}

	_, err := server.StartOpenVSwitchReconciliation(
		context.Background(),
		&pb.StartOpenVSwitchReconciliationRequest{OperationId: "op-route-gap"},
	)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		statusResponse, statusErr := server.GetOpenVSwitchReconciliationStatus(
			context.Background(),
			&pb.GetOpenVSwitchReconciliationStatusRequest{OperationId: "op-route-gap"},
		)
		return statusErr == nil &&
			statusResponse.GetOperation().GetOperation().GetPhase() == pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED
	}, time.Second, time.Millisecond)
}

func newOpenVSwitchTestRunner(t *testing.T, responses map[string][]openVSwitchCommandResult) *openVSwitchTestRunner {
	t.Helper()
	return &openVSwitchTestRunner{t: t, responses: responses}
}
