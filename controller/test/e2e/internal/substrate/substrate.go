// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package substrate stands up docker containers that stand in for
// already-provisioned bare-metal hosts.
//
// Each container runs systemd with nke-agent listening on the node-agent port,
// exactly as a pre-provisioned host would, and CWE is never told how it came to
// exist. The suite owns the containers end to end: it derives the node image,
// starts them on the management cluster's docker network, waits for the agent,
// and destroys them afterwards.
//
// Two choices differ from the nkectl integration harness this is modelled on,
// both because CWE runs inside a kind cluster rather than on the host:
//
//   - The containers join the *existing* kind network instead of a private
//     bridge. CWE runs as a pod in that cluster and CAPD tenant nodes are
//     already siblings there, so pod-to-container reachability is a property
//     the repo relies on today rather than something new to arrange.
//   - Addresses are discovered after start rather than assigned. kind's network
//     has its own IPAM, so hand-picked addresses risk collision — and
//     discovering them is the more faithful test anyway: a real host's address
//     is a fact to be told about, not something the orchestrator chose.
//
// One fiction the substrate has to maintain: these hosts are containers, so
// kubeadm needs the container-mode contract and CWE's preflight looks for the
// bootstrap-success sentinel rather than a cloud-init status. The sentinel is
// written here (see markBootstrapComplete); the container-mode kubeadm config
// comes from LOCAL_DEV_FORCE_DOCKER_MACHINE_BOOTSTRAP, which
// hack/local-preprovisioned-e2e.sh sets on the CWE deployment. Both are
// properties of running the hosts as containers, not of the preprovisioned path
// itself — real hardware needs neither.
package substrate

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	// bootstrapSuccessPath is the sentinel CWE's preflight looks for on a
	// container-backed node. See markBootstrapComplete.
	bootstrapSuccessPath = "/run/cluster-api/bootstrap-success.complete"

	// bgpPort is where kube-vip's BGP peer listens. See kubeVIPUsable.
	bgpPort = 179
)

// Logf is a minimal logging sink so the substrate can report progress without
// depending on a particular test framework.
type Logf func(format string, args ...any)

// Config describes the docker-level substrate.
type Config struct {
	// Network is the docker network to join. It must already exist: this is
	// the management cluster's network, created by `kind create cluster`.
	Network string

	// Gateway replaces the containers' default gateway when set. The local BGP
	// harness uses this to make its bridge-attached FRR container the kube-vip
	// peer on Docker versions that protect host services on bridge gateways.
	Gateway string

	// NodeCount is how many host containers to start.
	NodeCount int

	// BaseImage is the image the node image is derived from.
	BaseImage string

	// Image overrides the derived node image tag. Empty means derive one.
	Image string

	// AgentPort is the nke-agent gRPC port.
	AgentPort string

	// NamePrefix prefixes both the Kubernetes node name and the container name.
	NamePrefix string

	// KubeVIP decides whether the substrate offers a control-plane VIP:
	// "auto" (probe), "true" (require) or "false" (never).
	//
	// kube-vip announces the VIP over BGP to a peer on the hosts' default
	// gateway. Docker 28 and later stop a container from reaching a host
	// service on the bridge gateway, so a host-network FRR is listening yet
	// unreachable, and kube-vip sits in BGP state ACTIVE announcing into a
	// session that never establishes. "auto" detects exactly that and drops the
	// VIP rather than let the suite fail on an environment limitation.
	KubeVIP string

	// Keep leaves the substrate running after the suite, for debugging.
	Keep bool

	// Reuse attaches to an already-running substrate instead of recreating it.
	Reuse bool

	// ReadyTimeout bounds how long to wait for each agent to answer.
	ReadyTimeout time.Duration
}

// DefaultConfig reads the substrate's knobs from the environment. Every value
// has a working default for a local kind + tilt environment.
func DefaultConfig() Config {
	return Config{
		Network:      envOr("CWE_E2E_PREPROV_NETWORK", "kind"),
		Gateway:      os.Getenv("CWE_E2E_PREPROV_GATEWAY"),
		NodeCount:    envInt("CWE_E2E_PREPROV_NODES", 3),
		BaseImage:    envOr("CWE_E2E_PREPROV_BASE_IMAGE", "cwe-capd-kindest-node:local"),
		Image:        os.Getenv("CWE_E2E_PREPROV_IMAGE"),
		AgentPort:    envOr("CWE_E2E_PREPROV_AGENT_PORT", "15005"),
		NamePrefix:   envOr("CWE_E2E_PREPROV_NAME_PREFIX", "preprov"),
		KubeVIP:      envOr("CWE_E2E_PREPROV_KUBE_VIP", "auto"),
		Keep:         os.Getenv("CWE_E2E_PREPROV_KEEP") == "1",
		Reuse:        os.Getenv("CWE_E2E_PREPROV_REUSE") == "1",
		ReadyTimeout: envDuration("CWE_E2E_PREPROV_READY_TIMEOUT", 3*time.Minute),
	}
}

// Node is one host container standing in for a pre-provisioned machine.
type Node struct {
	// Name identifies the host in the PreprovisionedCluster inventory. It does
	// NOT become the Kubernetes node name: CWE generates that and passes it to
	// kubeadm, so the two are deliberately different and the specs assert on
	// the address rather than the name.
	Name string

	// IP is the address discovered from docker after the container started.
	// It is what CWE dials for node-agent and what kubeadm advertises.
	IP string

	// Container is the docker container name.
	Container string

	// AgentPort is the nke-agent gRPC port.
	AgentPort string
}

// Substrate is the running set of host containers.
type Substrate struct {
	Config Config
	Image  string

	nodes []Node
	vip   string
	log   Logf
}

// Start derives the node image, starts the containers and waits for every
// agent to answer. The caller must Stop it.
func Start(ctx context.Context, cfg Config, log Logf) (*Substrate, error) {
	if log == nil {
		log = func(string, ...any) {}
	}

	s := &Substrate{Config: cfg, log: log}
	s.nodes = s.planNodes()

	if err := s.checkNetwork(ctx); err != nil {
		return nil, err
	}

	if cfg.Reuse && s.allRunning(ctx) {
		log("substrate: reusing running containers (CWE_E2E_PREPROV_REUSE=1)")
		if err := s.configureGateways(ctx); err != nil {
			return nil, err
		}
		// /run is a tmpfs, so a container that restarted since the last run has
		// lost the sentinel. Rewriting it is cheap and idempotent.
		for _, node := range s.nodes {
			if err := s.markBootstrapComplete(ctx, node); err != nil {
				return nil, err
			}
		}
		if err := s.discoverAddresses(ctx); err != nil {
			return nil, err
		}
		return s, s.waitReady(ctx)
	}

	image, err := EnsureNodeImage(ctx, cfg, log)
	if err != nil {
		return nil, err
	}
	s.Image = image

	// Always start from a clean slate. A container left behind by a killed run
	// would otherwise be adopted silently, and its stale kubeadm state would
	// fail a later step in a way that looks like a product bug.
	s.teardown(ctx)

	for i := range s.nodes {
		if err := s.startNode(ctx, s.nodes[i]); err != nil {
			s.teardown(ctx)
			return nil, err
		}
	}

	if err := s.discoverAddresses(ctx); err != nil {
		s.teardown(ctx)
		return nil, err
	}

	if err := s.waitReady(ctx); err != nil {
		s.DumpDiagnostics(ctx)
		s.teardown(ctx)
		return nil, err
	}

	log("substrate: up — %d node(s) on network %q", len(s.nodes), cfg.Network)
	for _, n := range s.nodes {
		log("substrate:   %s -> %s:%s (%s)", n.Name, n.IP, n.AgentPort, n.Container)
	}
	return s, nil
}

// Nodes returns the running hosts, with addresses populated.
func (s *Substrate) Nodes() []Node {
	out := make([]Node, len(s.nodes))
	copy(out, s.nodes)
	return out
}

// ControlPlaneVIP returns an unused address near the end of the substrate
// network. kube-vip announces this address; Docker must not assign it to a
// container.
//
// It is empty when the substrate could not offer a usable VIP. A caller must
// then create the cluster without one: CWE skips kube-vip entirely, and the
// API server is reached on the node address instead.
func (s *Substrate) ControlPlaneVIP() string {
	return s.vip
}

// Stop tears the substrate down unless Keep is set.
func (s *Substrate) Stop(ctx context.Context) {
	if s.Config.Keep {
		s.log("substrate: CWE_E2E_PREPROV_KEEP=1 — leaving %d container(s) running", len(s.nodes))
		return
	}
	s.log("substrate: tearing down %d container(s)", len(s.nodes))
	s.teardown(ctx)
}

func (s *Substrate) planNodes() []Node {
	nodes := make([]Node, 0, s.Config.NodeCount)
	for i := range s.Config.NodeCount {
		name := fmt.Sprintf("%s-%d", s.Config.NamePrefix, i+1)
		nodes = append(nodes, Node{
			Name:      name,
			Container: "cwe-e2e-" + name,
			AgentPort: s.Config.AgentPort,
		})
	}
	return nodes
}

// checkNetwork verifies the target docker network exists. It is deliberately
// not created here: the default is the kind cluster's own network, and creating
// it ourselves would either race `kind create cluster` or silently produce a
// network the management cluster is not attached to — which shows up much later
// as CWE being unable to reach the nodes.
func (s *Substrate) checkNetwork(ctx context.Context) error {
	if err := exec.CommandContext(ctx, "docker", "network", "inspect", s.Config.Network).Run(); err != nil {
		return fmt.Errorf(
			"docker network %q not found.\n"+
				"  It is the management cluster's network; create the cluster first (`make kind-up`),\n"+
				"  or point CWE_E2E_PREPROV_NETWORK at the network CWE runs on",
			s.Config.Network,
		)
	}
	return nil
}

// startNode runs one host container.
//
// The flags are what kubeadm and containerd need in order to run inside a
// container, and are carried over from the nkectl integration harness:
//
//   - --privileged plus unconfined seccomp/apparmor: kubelet mounts, mknod and
//     the CNI's netlink work
//   - --cgroupns private: kubelet manages its own cgroup subtree
//   - /lib/modules read-only: CNI kernel modules resolve
//   - /dev/null over /proc/swaps: kubelet's IsSwapOn() runs before the
//     failSwapOn knob is read, and docker-in-docker inherits the host's swap
//   - tmpfs on /tmp and /run: systemd expects them writable and non-persistent
//   - named volumes for /var and /var/lib/containerd: overlayfs on overlayfs
//     breaks containerd's snapshotter
func (s *Substrate) startNode(ctx context.Context, node Node) error {
	dnsAddress := hostDNSAddress()
	args := []string{
		"run", "-d",
		"--name", node.Container,
		"--hostname", node.Name,
		"--privileged",
		"--security-opt", "seccomp=unconfined",
		"--security-opt", "apparmor=unconfined",
		"--cgroupns", "private",
		"--tty",
		"--restart", "unless-stopped",
		"--network", s.Config.Network,
		"--volume", "/lib/modules:/lib/modules:ro",
		"--volume", node.Container + "-var:/var",
		"--volume", node.Container + "-containerd:/var/lib/containerd",
		"--volume", "/dev/null:/proc/swaps:ro",
		"--tmpfs", "/tmp",
		"--tmpfs", "/run",
		"--env", "NODE_NAME=" + node.Name,
		"--label", "cwe-e2e-preprovisioned=true",
	}
	if dnsAddress != "" {
		args = append(args, "--dns", dnsAddress)
	}

	// Kind node images use the bridge gateway as their resolver. Docker's
	// container-name records are not always available through that resolver,
	// so make the local image mirrors explicit when they are present.
	for _, mirror := range []string{"registry-k8s-mirror", "registry-mirror"} {
		if address := dockerNetworkAddress(ctx, mirror, s.Config.Network); address != "" {
			args = append(args, "--add-host", mirror+":"+address)
		}
	}
	args = append(args, s.Image)

	if out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("starting %s: %w\n%s", node.Container, err, out)
	}
	if err := s.configureGateway(ctx, node); err != nil {
		return err
	}
	if err := s.markBootstrapComplete(ctx, node); err != nil {
		return err
	}
	if dnsAddress != "" {
		resolver := fmt.Sprintf("nameserver %s\noptions edns0 trust-ad ndots:0\n", dnsAddress)
		command := exec.CommandContext(ctx, "docker", "exec", "-i", node.Container, "tee", "/etc/resolv.conf")
		command.Stdin = strings.NewReader(resolver)
		if out, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf("setting resolver in %s: %w\n%s", node.Container, err, out)
		}
	}
	s.log("substrate: started %s", node.Container)
	return nil
}

// markBootstrapComplete writes the sentinel that says this host finished its
// own preparation before CWE ever heard of it.
//
// CWE's preflight for a container-backed node looks for
// /run/cluster-api/bootstrap-success.complete. On the CAPD path cloud-init
// writes it from capiPreKubeadmCommands; a preprovisioned host has no
// cloud-init to run, which is the whole premise, so the substrate writes it as
// part of presenting a prepared machine.
//
// It cannot be baked into the image: /run is a tmpfs mounted at container
// start, so anything the build put there is gone by the time the agent runs.
func (s *Substrate) markBootstrapComplete(ctx context.Context, node Node) error {
	command := exec.CommandContext(ctx, "docker", "exec", node.Container,
		"bash", "-c", "mkdir -p /run/cluster-api && touch "+bootstrapSuccessPath)
	if out, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("marking %s as prepared: %w\n%s", node.Container, err, out)
	}
	return nil
}

func (s *Substrate) configureGateways(ctx context.Context) error {
	for _, node := range s.nodes {
		if err := s.configureGateway(ctx, node); err != nil {
			return err
		}
	}
	return nil
}

func (s *Substrate) configureGateway(ctx context.Context, node Node) error {
	if s.Config.Gateway == "" {
		return nil
	}
	if net.ParseIP(s.Config.Gateway).To4() == nil {
		return fmt.Errorf("CWE_E2E_PREPROV_GATEWAY %q is not a valid IPv4 address", s.Config.Gateway)
	}
	if out, err := exec.CommandContext(ctx, "docker", "exec", node.Container,
		"ip", "route", "replace", "default", "via", s.Config.Gateway).CombinedOutput(); err != nil {
		return fmt.Errorf("setting default gateway for %s: %w\n%s", node.Container, err, out)
	}
	return nil
}

func hostDNSAddress() string {
	for _, path := range []string{"/run/systemd/resolve/resolv.conf", "/etc/resolv.conf"} {
		contents, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for line := range strings.SplitSeq(string(contents), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 || fields[0] != "nameserver" {
				continue
			}
			address := net.ParseIP(fields[1])
			if address != nil && !address.IsLoopback() {
				return address.String()
			}
		}
	}
	return ""
}

func dockerNetworkAddress(ctx context.Context, containerName, network string) string {
	format := fmt.Sprintf(`{{(index .NetworkSettings.Networks %q).IPAddress}}`, network)
	out, err := exec.CommandContext(ctx, "docker", "inspect", "-f", format, containerName).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// discoverAddresses reads each container's address on the substrate network.
//
// This is the address the test puts in the inventory, so it is deliberately
// read back from docker rather than assigned: the suite should be exercising
// "the provider was told an address and published it", not "the provider and
// the suite happen to agree on a constant".
func (s *Substrate) discoverAddresses(ctx context.Context) error {
	format := fmt.Sprintf(`{{(index .NetworkSettings.Networks %q).IPAddress}}`, s.Config.Network)

	for i := range s.nodes {
		out, err := exec.CommandContext(ctx, "docker", "inspect", "-f", format, s.nodes[i].Container).Output()
		if err != nil {
			return fmt.Errorf("inspecting %s for its %s address: %w", s.nodes[i].Container, s.Config.Network, err)
		}
		ip := strings.TrimSpace(string(out))
		if ip == "" || ip == "<no value>" {
			return fmt.Errorf("container %s has no address on network %q", s.nodes[i].Container, s.Config.Network)
		}
		s.nodes[i].IP = ip
	}
	return s.discoverControlPlaneVIP(ctx)
}

// kubeVIPUsable reports whether kube-vip could actually announce a VIP from
// these hosts.
//
// It probes the thing that breaks rather than the thing that is easy to see:
// the BGP peer port on the hosts' own default gateway. A peer container that is
// running but unreachable looks healthy to any other check, and the failure it
// produces — kube-vip stuck in BGP state ACTIVE while reporting "announcing
// route" — surfaces minutes later as an unreachable API server.
func (s *Substrate) kubeVIPUsable(ctx context.Context) bool {
	switch s.Config.KubeVIP {
	case "false":
		s.log("substrate: kube-vip disabled (CWE_E2E_PREPROV_KUBE_VIP=false)")
		return false
	case "true":
		return true
	}

	if len(s.nodes) == 0 {
		return false
	}
	container := s.nodes[0].Container

	gatewayOut, err := exec.CommandContext(ctx, "docker", "exec", container,
		"sh", "-c", "ip route show default | awk '{print $3; exit}'").Output()
	gateway := strings.TrimSpace(string(gatewayOut))
	if err != nil || gateway == "" {
		s.log("substrate: could not read the hosts' default gateway; continuing without a control-plane VIP")
		return false
	}

	probe := exec.CommandContext(ctx, "docker", "exec", container,
		"bash", "-c", fmt.Sprintf("timeout 3 bash -c '</dev/tcp/%s/%d'", gateway, bgpPort))
	if err := probe.Run(); err != nil {
		s.log("substrate: no BGP peer reachable at %s:%d from the hosts, so kube-vip could not "+
			"announce a VIP; continuing without one. Docker 28+ blocks a container from reaching a "+
			"host service on the bridge gateway, so a host-network FRR is listening but unreachable.",
			gateway, bgpPort)
		return false
	}

	s.log("substrate: BGP peer reachable at %s:%d; offering a control-plane VIP", gateway, bgpPort)
	return true
}

func (s *Substrate) discoverControlPlaneVIP(ctx context.Context) error {
	if !s.kubeVIPUsable(ctx) {
		s.vip = ""
		return nil
	}

	subnetOutput, err := exec.CommandContext(ctx, "docker", "network", "inspect", "-f",
		"{{range .IPAM.Config}}{{println .Subnet}}{{end}}", s.Config.Network).Output()
	if err != nil {
		return fmt.Errorf("reading subnet for docker network %s: %w", s.Config.Network, err)
	}
	var subnet *net.IPNet
	for _, value := range strings.Fields(string(subnetOutput)) {
		_, candidate, parseErr := net.ParseCIDR(value)
		if parseErr == nil && candidate.IP.To4() != nil {
			subnet = candidate
			break
		}
	}
	if subnet == nil {
		return fmt.Errorf("docker network %s does not have a usable IPv4 subnet", s.Config.Network)
	}
	ones, bits := subnet.Mask.Size()
	if bits != 32 || ones > 28 {
		return fmt.Errorf("docker network %s subnet %s is too small for a control-plane VIP", s.Config.Network, subnet)
	}

	allocatedOutput, err := exec.CommandContext(ctx, "docker", "network", "inspect", "-f",
		"{{range .Containers}}{{.IPv4Address}} {{end}}", s.Config.Network).Output()
	if err != nil {
		return fmt.Errorf("reading allocations for docker network %s: %w", s.Config.Network, err)
	}
	allocated := map[string]struct{}{}
	for _, value := range strings.Fields(string(allocatedOutput)) {
		address, _, _ := strings.Cut(value, "/")
		allocated[address] = struct{}{}
	}

	network := binary.BigEndian.Uint32(subnet.IP.To4())
	hostMask := ^binary.BigEndian.Uint32(net.IP(subnet.Mask).To4())
	broadcast := network | hostMask
	for offset := uint32(10); offset < 74; offset++ {
		candidateBytes := make(net.IP, net.IPv4len)
		binary.BigEndian.PutUint32(candidateBytes, broadcast-offset)
		candidate := candidateBytes.String()
		if _, found := allocated[candidate]; found {
			continue
		}
		s.vip = candidate
		return nil
	}
	return fmt.Errorf("docker network %s has no free address for a control-plane VIP", s.Config.Network)
}

// waitReady polls each node until nke-agent is up.
//
// "Container running" is not readiness: systemd still has to start, containerd
// has to settle and nke-agent has to bind its port. Proceeding early makes
// cluster creation fail preflight for reasons unrelated to what is under test.
//
// Readiness is the systemd unit reporting active *and* the port accepting a
// connection. A gRPC-level probe would be stronger, but it would pull the
// node-agent protobufs and grpc into this module for one call; CWE's own
// preflight performs the real gRPC check moments later, and a failure there is
// a product signal worth seeing rather than one to pre-empt here.
func (s *Substrate) waitReady(ctx context.Context) error {
	deadline := time.Now().Add(s.Config.ReadyTimeout)

	for _, node := range s.nodes {
		var lastErr error
		ready := false
		for time.Now().Before(deadline) {
			if err := probeAgent(ctx, node); err == nil {
				ready = true
				break
			} else {
				lastErr = err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
		if !ready {
			return fmt.Errorf("node %s (%s:%s) agent not ready within %s: %w",
				node.Name, node.IP, node.AgentPort, s.Config.ReadyTimeout, lastErr)
		}
		s.log("substrate: %s agent ready on %s:%s", node.Name, node.IP, node.AgentPort)
	}
	return nil
}

func probeAgent(ctx context.Context, node Node) error {
	out, err := exec.CommandContext(ctx, "docker", "exec", node.Container,
		"systemctl", "is-active", "nke-agent").Output()
	state := strings.TrimSpace(string(out))
	if err != nil && state == "" {
		return fmt.Errorf("querying nke-agent unit state: %w", err)
	}
	if state != "active" {
		return fmt.Errorf("nke-agent unit is %q, want active", state)
	}

	// Dial from inside the container: on Linux the host can reach container
	// addresses directly, but when the suite itself runs as a sibling container
	// (see hack/local-preprovisioned-e2e.sh) or on Docker Desktop it cannot.
	// Exec'ing the check keeps readiness meaningful everywhere.
	probe := exec.CommandContext(ctx, "docker", "exec", node.Container,
		"bash", "-c", fmt.Sprintf("timeout 2 bash -c '</dev/tcp/127.0.0.1/%s'", node.AgentPort))
	if out, err := probe.CombinedOutput(); err != nil {
		return fmt.Errorf("nke-agent not accepting connections on %s: %w\n%s", node.AgentPort, err, out)
	}

	return nil
}

func (s *Substrate) allRunning(ctx context.Context) bool {
	for _, node := range s.nodes {
		out, err := exec.CommandContext(ctx, "docker", "inspect",
			"-f", "{{.State.Running}}", node.Container).Output()
		if err != nil || strings.TrimSpace(string(out)) != "true" {
			return false
		}
	}
	return true
}

// teardown removes the containers and their volumes. Every step is best-effort:
// teardown has to make progress even from a half-built substrate, and a leaked
// container holds a name and volumes that break the next run.
//
// The network is deliberately left alone — we did not create it.
func (s *Substrate) teardown(ctx context.Context) {
	for _, node := range s.nodes {
		_ = exec.CommandContext(ctx, "docker", "rm", "-f", "-v", node.Container).Run()
		_ = exec.CommandContext(ctx, "docker", "volume", "rm", "-f",
			node.Container+"-var", node.Container+"-containerd").Run()
	}
}

// DumpDiagnostics logs what a failed bring-up needs in order to be debugged
// after the containers are gone.
func (s *Substrate) DumpDiagnostics(ctx context.Context) {
	for _, node := range s.nodes {
		if out, err := exec.CommandContext(ctx, "docker", "logs", "--tail", "50", node.Container).CombinedOutput(); err == nil {
			s.log("substrate: --- docker logs %s (tail) ---\n%s", node.Container, out)
		}
		if out, err := exec.CommandContext(ctx, "docker", "exec", node.Container,
			"journalctl", "-u", "nke-agent", "--no-pager", "-n", "50").CombinedOutput(); err == nil {
			s.log("substrate: --- journalctl -u nke-agent %s ---\n%s", node.Container, out)
		}
	}
}

// ContainerCount returns how many containers carry the substrate label. The
// pre-provisioned flow must never create infrastructure, so this staying
// constant across a cluster create is a meaningful assertion.
func ContainerCount(ctx context.Context) (int, error) {
	out, err := exec.CommandContext(ctx, "docker", "ps", "-aq",
		"--filter", "label=cwe-e2e-preprovisioned=true").Output()
	if err != nil {
		return 0, fmt.Errorf("listing substrate containers: %w", err)
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return 0, nil
	}
	return len(strings.Split(trimmed, "\n")), nil
}

// Running reports whether the named container is still running, so teardown
// specs can assert CWE did not destroy a host it does not own.
func Running(ctx context.Context, container string) (bool, error) {
	out, err := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.State.Running}}", container).Output()
	if err != nil {
		return false, fmt.Errorf("inspecting %s: %w", container, err)
	}
	return strings.TrimSpace(string(out)) == "true", nil
}

// FileExists reports whether a path exists inside the host container.
//
// Teardown is only really verified by looking at the machine: the API can say a
// node is gone while the host is still carrying a live control plane, which
// would make it unusable for the next cluster.
func FileExists(ctx context.Context, container, path string) (bool, error) {
	cmd := exec.CommandContext(ctx, "docker", "exec", container, "test", "-e", path)
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// `test -e` exits 1 for "absent"; anything else is a real failure
			// and must not be reported as absence.
			if exitErr.ExitCode() == 1 {
				return false, nil
			}
		}
		return false, fmt.Errorf("checking %s in %s: %w", path, container, err)
	}
	return true, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}
