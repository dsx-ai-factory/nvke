// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package testutil

import (
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

	"github.com/NVIDIA/nke/controller/forge-vms/internal/netutil"
)

const (
	defaultBGPVIPCIDR      = "172.31.0.0/24"
	bgpControlPlaneCount   = 3
	bgpECMPBackendPort     = 18081
	bgpECMPRequestCount    = 100
	bgpECMPFirstSourcePort = 20001
	bgpECMPNextSourcePort  = 21001
	bgpSSHWait             = 5 * time.Minute
)

// RunBGPECMPFlow is an end-to-end test of the BGP speaker's ECMP route
// management using real VMs running the actual kube-vip container.
//
// It provisions 3 control-plane VMs and a worker-only client VM, SSHes into
// each control-plane VM to run the kube-vip configure script (validating the
// CWE runbook path), then starts kube-vip via kubelet. The test verifies the
// host's ECMP route and worker-originated traffic distribution, then deletes a
// control-plane VM and verifies route and traffic degradation.
//
// Requirements:
//   - FORGE_VMS_TEST_BGP=true
//   - Linux host with bridge + BGP speaker (setup-linux.sh)
//   - CAP_NET_ADMIN on the test binary (for netlink route installation)
//   - Real VM backend (libvirt)
//   - iptables REDIRECT 179 → configured BGP listen port (added automatically)
//   - Internet access from VMs (for apt install + container image pull)
func RunBGPECMPFlow(t *testing.T, env *TestEnv, profile RuntimeProfile) {
	t.Helper()
	test := prepareBGPECMPTest(t, env, profile)

	t.Log("Phase 1: Provisioning 3 CP VMs and a worker client VM...")
	test.provisionTopology()

	t.Log("Phase 2: Configuring kube-vip on each CP VM via CWE runbook...")
	test.configureControlPlanes()

	t.Log("Phase 3: Waiting for kubelet and CP identity backends...")
	test.waitForControlPlaneBackends()

	allControlPlanes := test.controlPlanes

	t.Log("Phase 4: Verifying ECMP route and worker traffic across 3 CPs...")
	test.requireNexthops(allControlPlanes)
	test.assertWorkerRoute()
	test.assertWorkerDistribution(allControlPlanes, bgpECMPFirstSourcePort)
	// Traffic must not install an ICMP redirect that pins the worker to one CP.
	test.assertWorkerRoute()

	withdrawn := allControlPlanes[2]
	survivors := allControlPlanes[:2]

	t.Logf("Phase 5: Deleting CP VM %s (%s), verifying route and traffic degradation...", withdrawn.name, withdrawn.ip)
	test.withdrawControlPlane(withdrawn)
	test.requireNexthops(survivors)
	test.assertWorkerRoute()
	test.assertWorkerDistribution(survivors, bgpECMPNextSourcePort)
	// The surviving distribution sample must not change the worker's route.
	test.assertWorkerRoute()

	t.Log("Phase 6: Cleaning up remaining VMs...")
	test.cleanup(survivors)
}

type bgpTestNode struct {
	id   string
	name string
	ip   string
}

type bgpECMPTest struct {
	t       *testing.T
	env     *TestEnv
	profile RuntimeProfile
	sshCfg  *ssh.ClientConfig

	vip       string
	bridgeGW  string
	scriptB64 string
	routeWait time.Duration

	controlPlanes []bgpTestNode
	worker        bgpTestNode
}

// prepareBGPECMPTest validates the test prerequisites and prepares the
// shared values used by all six phases.
func prepareBGPECMPTest(t *testing.T, env *TestEnv, profile RuntimeProfile) *bgpECMPTest {
	t.Helper()

	if !IsTrueEnv("FORGE_VMS_TEST_BGP") {
		t.Skip("skipping BGP ECMP test; set FORGE_VMS_TEST_BGP=true to enable")
	}
	if runtime.GOOS != "linux" {
		t.Skip("BGP ECMP flow requires Linux (netlink + iptables)")
	}
	// CAP_NET_ADMIN is required for netlink route installation. Rather than
	// requiring root (which breaks qemu:///session), set CAP_NET_ADMIN on the
	// test binary: sudo setcap cap_net_admin+ep /tmp/bgp-e2e-test
	if profile.Config.EffectiveVMBackend() == VMBackendMock {
		t.Skip("BGP ECMP flow requires a real VM backend")
	}
	if profile.Config.BridgeCIDR == "" {
		t.Skip("BGP ECMP flow requires FORGE_VMS_BRIDGE_CIDR to be set")
	}

	bridgeGW, err := netutil.GatewayIP(profile.Config.BridgeCIDR)
	if err != nil {
		t.Fatalf("derive bridge gateway: %v", err)
	}
	vip := selectOffLinkBGPTestVIP(t, profile.Config.BridgeCIDR, profile.Config.BGPAllowedVIPCIDRs)
	requireLayer4ECMPHashPolicy(t)

	routeWait := 2 * time.Minute
	if holdWait := time.Duration(profile.Config.BGPHoldTime+30) * time.Second; holdWait > routeWait {
		routeWait = holdWait
	}

	// Ensure the privileged BGP port redirects to the configured speaker port.
	ensureBGPIPTablesRedirect(t, profile.Config.BridgeName, bridgeGW, profile.Config.BGPListenPort)

	// Read and base64-encode the shared CWE control-plane kube-vip runbook for
	// embedding in cloud-init write_files (avoids YAML escaping issues). The test
	// exercises the exact runbook CWE ships on CP nodes, not a divergent copy.
	return &bgpECMPTest{
		t:         t,
		env:       env,
		profile:   profile,
		sshCfg:    TestSSHConfig(),
		vip:       vip,
		bridgeGW:  bridgeGW,
		scriptB64: readAndEncodeCWERunbook(t, "configure-kube-vip.sh"),
		routeWait: routeWait,
	}
}

func (f *bgpECMPTest) provisionTopology() {
	f.t.Helper()
	f.controlPlanes = make([]bgpTestNode, bgpControlPlaneCount)

	// Submit every VM before waiting so cloud-init runs concurrently.
	for i := range f.controlPlanes {
		name := UniqueInstanceName(fmt.Sprintf("bgp-cp-%d", i+1))
		f.controlPlanes[i] = f.createNode(name, bgpFlowUserData(f.t, name, f.scriptB64))
	}

	workerName := UniqueInstanceName("bgp-worker")
	f.worker = f.createNode(workerName, bgpClientUserData(workerName))

	for i := range f.controlPlanes {
		f.waitForReady(&f.controlPlanes[i])
	}
	f.waitForReady(&f.worker)
}

func (f *bgpECMPTest) createNode(name, userData string) bgpTestNode {
	f.t.Helper()
	created := f.env.CreateInstance(&forgesdk.InstanceCreateRequest{
		Name:           name,
		TenantId:       "d282929f-49d5-4dec-8d27-21ceb30ade19",
		InstanceTypeId: forgesdk.PtrString("itype-mock-001"),
		VpcId:          "vpc-mock-001",
		Labels:         map[string]string{"nke.nvidia.com/node-id": name},
		Interfaces: []forgesdk.InterfaceCreateRequest{
			{SubnetId: forgesdk.PtrString("subnet-mock-001")},
		},
		UserData:         *forgesdk.NewNullableString(forgesdk.PtrString(userData)),
		PhoneHomeEnabled: forgesdk.PtrBool(true),
	})
	node := bgpTestNode{id: created.GetId(), name: name}
	instanceID := node.id
	f.t.Cleanup(func() {
		PreserveInstanceArtifacts(f.t, f.profile, instanceID)
		CleanupInstanceByID(f.t, f.env, f.profile, instanceID)
	})
	f.t.Logf("VM created: id=%s name=%s", node.id, node.name)
	return node
}

func (f *bgpECMPTest) waitForReady(node *bgpTestNode) {
	f.t.Helper()
	ready := WaitForInstanceState(f.t, f.env, f.profile, node.id, "ready")
	node.ip = InstanceIP(f.t, ready)
	f.t.Logf("VM ready: id=%s name=%s ip=%s", node.id, node.name, node.ip)
}

func (f *bgpECMPTest) configureControlPlanes() {
	f.t.Helper()
	for i, node := range f.controlPlanes {
		// CP_HEALTH_CHECK_ADDRESS="" disables kube-vip's control-plane health
		// check for this test: these BGP-only VMs run no kube-apiserver, so the
		// runbook's default https://NODE_IP:6443/readyz probe would never pass and
		// kube-vip would never announce the VIP. This test exercises the BGP
		// announce/withdraw + ECMP path.
		execCmd := fmt.Sprintf(
			`sudo env CP_HEALTH_CHECK_ADDRESS="" VIP_ADDRESS=%s BGP_AS=%d NODE_IP=%s BGP_PEER_ADDRESS=%s BGP_PEER_AS=%d /opt/configure-kube-vip.sh`,
			f.vip, f.profile.Config.BGPPeerASN, node.ip, f.bridgeGW, f.profile.Config.BGPLocalASN,
		)
		WaitForSSHCommand(f.t, node.ip, f.sshCfg, execCmd, bgpSSHWait)

		manifest := WaitForSSHCommand(f.t, node.ip, f.sshCfg,
			"cat /etc/kubernetes/manifests/kube-vip.yaml", 10*time.Second)
		if !strings.Contains(manifest, f.vip) {
			f.t.Fatalf("VM %d (%s): kube-vip manifest missing VIP %s", i+1, node.ip, f.vip)
		}
		f.t.Logf("VM %d (%s): kube-vip manifest created and verified", i+1, node.ip)
	}
}

func (f *bgpECMPTest) waitForControlPlaneBackends() {
	f.t.Helper()
	for i, node := range f.controlPlanes {
		WaitForSSHCommand(f.t, node.ip, f.sshCfg,
			"systemctl is-active kubelet > /dev/null 2>&1", bgpSSHWait)
		backendCheck := fmt.Sprintf(
			"test \"$(curl -fsS http://127.0.0.1:%d/node-id)\" = %q",
			bgpECMPBackendPort, node.name,
		)
		WaitForSSHCommand(f.t, node.ip, f.sshCfg, backendCheck, bgpSSHWait)
		f.t.Logf("VM %d (%s): kubelet and identity backend are ready", i+1, node.ip)
	}
}

func (f *bgpECMPTest) requireNexthops(nodes []bgpTestNode) {
	f.t.Helper()
	want := bgpNodeIPs(nodes)
	waitForECMPNexthops(f.t, f.vip, want, f.routeWait)
	f.t.Logf("ECMP route confirmed: %s/32 via %v", f.vip, want)
}

func (f *bgpECMPTest) assertWorkerRoute() {
	f.t.Helper()
	assertClientVIPRouteViaGateway(f.t, f.worker.ip, f.vip, f.bridgeGW, f.sshCfg)
}

func (f *bgpECMPTest) assertWorkerDistribution(nodes []bgpTestNode, firstSourcePort int) {
	f.t.Helper()
	assertECMPTrafficDistribution(
		f.t,
		f.worker.ip,
		f.vip,
		bgpNodeNames(nodes),
		firstSourcePort,
		f.sshCfg,
	)
}

func (f *bgpECMPTest) withdrawControlPlane(node bgpTestNode) {
	f.t.Helper()
	PreserveInstanceArtifacts(f.t, f.profile, node.id)
	f.env.DeleteInstance(node.id)
	WaitForInstanceDeletion(f.t, f.env, f.profile, node.id)
	f.t.Logf("VM deleted: id=%s name=%s ip=%s", node.id, node.name, node.ip)
}

func (f *bgpECMPTest) cleanup(survivors []bgpTestNode) {
	f.t.Helper()
	if KeepVMs() {
		f.t.Log("FORGE_VMS_E2E_TESTS_KEEP_VMS: keeping surviving CP VMs and worker client; the withdrawn CP was deleted as part of the test")
		return
	}
	for _, node := range survivors {
		PreserveInstanceArtifacts(f.t, f.profile, node.id)
		DeleteAndWait(f.t, f.env, f.profile, node.id, node.ip)
	}
	waitForRouteRemoved(f.t, f.vip, f.routeWait)
	PreserveInstanceArtifacts(f.t, f.profile, f.worker.id)
	DeleteAndWait(f.t, f.env, f.profile, f.worker.id, f.worker.ip)
	f.t.Log("All routes removed and test VMs deleted after cleanup")
}

func bgpNodeIPs(nodes []bgpTestNode) []string {
	result := make([]string, len(nodes))
	for i, node := range nodes {
		result[i] = node.ip
	}
	return result
}

func bgpNodeNames(nodes []bgpTestNode) []string {
	result := make([]string, len(nodes))
	for i, node := range nodes {
		result[i] = node.name
	}
	return result
}

// ── Helpers ─────────────────────────────────────────────────────────────────

// bgpFlowUserData returns a cloud-config that:
//   - enables password-based SSH access
//   - installs kubelet, containerd, jq, and a small HTTP responder
//   - creates a dummy kubeconfig and service account token (kube-vip needs
//     these to initialize its k8s client before starting BGP — same as
//     production where kube-vip starts before the API server is up)
//   - embeds the configure-kube-vip.sh script via write_files
//   - configures kubelet for standalone static-pod mode
//   - enables phone_home so the engine knows when cloud-init is fully done
func bgpFlowUserData(t *testing.T, hostname, scriptB64 string) string {
	t.Helper()
	return fmt.Sprintf(`#cloud-config
hostname: %s
manage_etc_hosts: true
ssh_pwauth: true
users:
  - name: %s
    plain_text_passwd: %s
    lock_passwd: false
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
packages:
  - jq
  - containerd
  - python3
write_files:
  - path: /opt/configure-kube-vip.sh
    permissions: "0755"
    encoding: b64
    content: %s
  - path: /var/lib/forge-vms-ecmp/node-id
    permissions: "0644"
    content: |
      %s
  - path: /etc/kubernetes/super-admin.conf
    permissions: "0644"
    content: |
      apiVersion: v1
      kind: Config
      clusters:
      - cluster:
          server: https://127.0.0.1:6443
        name: kubernetes
      contexts:
      - context:
          cluster: kubernetes
        name: default
      current-context: default
  - path: /var/run/secrets/kubernetes.io/serviceaccount/token
    permissions: "0644"
    content: dummy-token
  - path: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt
    permissions: "0644"
    content: dummy-ca
  - path: /var/run/secrets/kubernetes.io/serviceaccount/namespace
    permissions: "0644"
    content: kube-system
  - path: /etc/systemd/system/kubelet.service
    permissions: "0644"
    content: |
      [Unit]
      Description=kubelet
      After=containerd.service
      Requires=containerd.service
      [Service]
      ExecStart=/usr/local/bin/kubelet \
        --pod-manifest-path=/etc/kubernetes/manifests \
        --fail-swap-on=false \
        --container-runtime-endpoint=unix:///run/containerd/containerd.sock
      Restart=always
      RestartSec=10
      [Install]
      WantedBy=multi-user.target
  - path: /etc/systemd/system/forge-vms-ecmp-backend.service
    permissions: "0644"
    content: |
      [Unit]
      Description=forge-vms ECMP test backend
      After=network-online.target
      [Service]
      Type=simple
      ExecStart=/usr/bin/python3 -m http.server %d --bind 0.0.0.0 --directory /var/lib/forge-vms-ecmp
      Restart=always
      RestartSec=1
      [Install]
      WantedBy=multi-user.target
runcmd:
  - curl -fsSL https://dl.k8s.io/release/v1.30.0/bin/linux/amd64/kubelet -o /usr/local/bin/kubelet
  - chmod +x /usr/local/bin/kubelet
  - systemctl enable --now containerd
  - systemctl daemon-reload
  - systemctl enable --now forge-vms-ecmp-backend
  - systemctl enable --now kubelet
`+PhoneHomeYAML, hostname, sshUser, sshPassword, scriptB64, hostname, bgpECMPBackendPort)
}

// bgpClientUserData returns a cloud-config for the worker-only traffic source.
// It intentionally does not run kube-vip or advertise the test VIP.
func bgpClientUserData(hostname string) string {
	return fmt.Sprintf(`#cloud-config
hostname: %s
manage_etc_hosts: true
ssh_pwauth: true
users:
  - name: %s
    plain_text_passwd: %s
    lock_passwd: false
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
packages:
  - curl
`+PhoneHomeYAML, hostname, sshUser, sshPassword)
}

func requireLayer4ECMPHashPolicy(t *testing.T) {
	t.Helper()

	const path = "/proc/sys/net/ipv4/fib_multipath_hash_policy"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v; run scripts/setup-linux.sh before the BGP ECMP test", path, err)
	}
	if got := strings.TrimSpace(string(raw)); got != "1" {
		t.Fatalf("%s=%s, want 1 for per-connection Layer 4 ECMP hashing; run scripts/setup-linux.sh", path, got)
	}
}

func selectOffLinkBGPTestVIP(t *testing.T, bridgeCIDR string, allowedVIPCIDRs []string) string {
	t.Helper()

	_, bridgeNet, err := net.ParseCIDR(bridgeCIDR)
	if err != nil {
		t.Fatalf("parse bridge CIDR: %v", err)
	}
	candidates := allowedVIPCIDRs
	if len(candidates) == 0 {
		candidates = []string{defaultBGPVIPCIDR}
	}

	for _, cidr := range candidates {
		ip, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatalf("parse allowed BGP VIP CIDR %s: %v", cidr, err)
		}
		vip := ip.To4()
		if vip == nil {
			t.Fatalf("allowed BGP VIP CIDR %s is not IPv4", cidr)
		}
		ones, bits := ipNet.Mask.Size()
		if bits != 32 {
			t.Fatalf("allowed BGP VIP CIDR %s has unexpected address size %d", cidr, bits)
		}
		if ones < bits {
			candidate, err := netutil.GatewayIP(cidr)
			if err != nil {
				t.Fatalf("derive test VIP from %s: %v", cidr, err)
			}
			vip = net.ParseIP(candidate).To4()
		}
		if !bridgeNet.Contains(vip) {
			return vip.String()
		}
	}

	t.Skipf("worker-originated BGP ECMP flow requires an off-link VIP; configured allowed VIP CIDRs are %v", candidates)
	return ""
}

func assertClientVIPRouteViaGateway(
	t *testing.T,
	clientIP, vip, gateway string,
	sshCfg *ssh.ClientConfig,
) {
	t.Helper()

	route := WaitForSSHCommand(
		t,
		clientIP,
		sshCfg,
		fmt.Sprintf("ip -4 route get %s", vip),
		10*time.Second,
	)
	fields := strings.Fields(route)
	usesGateway := false
	for i, field := range fields {
		if field == "via" && i+1 < len(fields) && fields[i+1] == gateway {
			usesGateway = true
			break
		}
	}
	if !usesGateway {
		t.Fatalf("worker route to VIP %s does not traverse bridge gateway %s: %s", vip, gateway, strings.TrimSpace(route))
	}
}

func assertECMPTrafficDistribution(
	t *testing.T,
	clientIP, vip string,
	expectedNodeIDs []string,
	firstSourcePort int,
	sshCfg *ssh.ClientConfig,
) {
	t.Helper()

	cmd := fmt.Sprintf(`set -eu
i=0
while [ "$i" -lt %d ]; do
  port=$((%d + i))
  curl --noproxy '*' --http1.0 --local-port "$port" --fail --silent --show-error --connect-timeout 2 --max-time 5 -H 'Connection: close' "http://%s:%d/node-id"
  printf '\n'
  i=$((i + 1))
done`, bgpECMPRequestCount, firstSourcePort, vip, bgpECMPBackendPort)

	out, err := SSHRun(clientIP, sshCfg, cmd)
	if err != nil {
		t.Fatalf("send %d fresh worker connections to %s: %v (output: %s)", bgpECMPRequestCount, vip, err, out)
	}

	responses := strings.Fields(out)
	if len(responses) != bgpECMPRequestCount {
		t.Fatalf("worker traffic to %s returned %d responses, want %d (output: %s)", vip, len(responses), bgpECMPRequestCount, out)
	}

	counts := make(map[string]int, len(expectedNodeIDs))
	for _, nodeID := range expectedNodeIDs {
		counts[nodeID] = 0
	}
	for _, response := range responses {
		if _, ok := counts[response]; !ok {
			t.Fatalf("worker traffic to %s reached unexpected backend %q; expected one of %v", vip, response, expectedNodeIDs)
		}
		counts[response]++
	}
	for _, nodeID := range expectedNodeIDs {
		if counts[nodeID] == 0 {
			t.Fatalf("worker traffic to %s never reached backend %s; distribution=%v", vip, nodeID, counts)
		}
	}
	t.Logf("%d fresh worker connections to %s distributed across backends: %v", bgpECMPRequestCount, vip, counts)
}

// readAndEncodeCWERunbook reads a shared CWE control-plane runbook (the same
// script CWE executes on real CP nodes) and returns its base64-encoded content
// for embedding in cloud-init write_files. This keeps the integration test on a
// single source of truth rather than a divergent copy. FindRepoRoot resolves the
// forge-vms module root; the CWE runbooks live in the parent module one level up.
func readAndEncodeCWERunbook(t *testing.T, filename string) string {
	t.Helper()
	moduleRoot, err := FindRepoRoot()
	if err != nil {
		t.Fatalf("find repo root: %v", err)
	}
	path := filepath.Join(moduleRoot, "..", "internal", "runbook", "scripts", filename)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read CWE runbook %s: %v", filename, err)
	}
	return base64.StdEncoding.EncodeToString(content)
}

// ensureBGPIPTablesRedirect adds the iptables PREROUTING rule that redirects
// port 179 to the configured unprivileged BGP listen port, if not already present.
func ensureBGPIPTablesRedirect(t *testing.T, bridgeName, bridgeGW string, listenPort int) {
	t.Helper()
	listenPortString := fmt.Sprintf("%d", listenPort)
	args := []string{
		"iptables", "-t", "nat", "-C", "PREROUTING",
		"-i", bridgeName, "-d", bridgeGW,
		"-p", "tcp", "--dport", "179",
		"-j", "REDIRECT", "--to-port", listenPortString,
	}
	if exec.Command("sudo", args...).Run() == nil {
		t.Log("iptables BGP redirect already present")
		return
	}
	args[3] = "-A"
	out, err := exec.Command("sudo", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("add iptables BGP redirect: %v (output: %s)", err, string(out))
	}
	t.Logf("Added iptables REDIRECT: port 179 → %d on %s (dest=%s)", listenPort, bridgeName, bridgeGW)
}

// ── Route verification helpers ──────────────────────────────────────────────

func waitForECMPNexthops(t *testing.T, vip string, want []string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		got := getRouteNexthops(t, vip)
		if equalNexthopSets(got, want) {
			return
		}
		t.Logf("route %s/32: got nexthops %v, want %v (retrying)", vip, got, want)
		time.Sleep(500 * time.Millisecond)
	}
	got := getRouteNexthops(t, vip)
	t.Fatalf("route %s/32: got nexthops %v, want %v", vip, got, want)
}

func equalNexthopSets(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	wantCounts := make(map[string]int, len(want))
	for _, nexthop := range want {
		wantCounts[nexthop]++
	}
	for _, nexthop := range got {
		if wantCounts[nexthop] == 0 {
			return false
		}
		wantCounts[nexthop]--
	}
	return true
}

func waitForRouteRemoved(t *testing.T, vip string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		nexthops := getRouteNexthops(t, vip)
		if len(nexthops) == 0 {
			return
		}
		t.Logf("route %s/32: still has %d nexthops (retrying)", vip, len(nexthops))
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("route %s/32 was not removed within timeout", vip)
}

func getRouteNexthops(t *testing.T, vip string) []string {
	t.Helper()
	out, err := exec.Command("ip", "route", "show", vip+"/32").Output()
	if err != nil {
		return nil
	}
	var nexthops []string
	for _, line := range strings.Split(string(out), "\n") {
		parts := strings.Fields(strings.TrimSpace(line))
		for i, p := range parts {
			if p == "via" && i+1 < len(parts) {
				nexthops = append(nexthops, parts[i+1])
			}
		}
	}
	return nexthops
}
