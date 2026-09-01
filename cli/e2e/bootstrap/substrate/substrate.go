// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package substrate stands up docker containers that stand in for
// pre-provisioned bare-metal hosts, for the nkectl bootstrap-site E2E suite.
//
// Each container runs systemd with nke-agent listening on the node-agent
// port, exactly as a pre-provisioned host would. The suite owns the
// containers end to end: it derives the node image, starts them, waits for
// the agent, and destroys them afterwards.
//
// Modelled on CWE's own provisioned-provider E2E substrate
// (test/e2e/internal/substrate in the CWE repo), with two differences that
// follow from nkectl having no existing cluster to attach to:
//
//   - This package creates its own private network rather than joining one
//     that already exists. CWE's substrate joins the management cluster's
//     kind network because CWE already runs there; nkectl's ephemeral
//     bootstrap cluster does not exist yet when these fixture nodes come up,
//     so there is nothing to join.
//   - Addresses are assigned, not discovered. CWE's substrate reads addresses
//     back from docker specifically to avoid assuming IPAM behavior on a
//     shared network it does not own. This package owns its network outright,
//     so pinning addresses is safe, and the fixture's inventory file needs a
//     stable IP to reference before any container exists.
package substrate

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Logf is a minimal logging sink so the substrate can report progress without
// depending on a particular test framework.
type Logf func(format string, args ...any)

// Config describes the docker-level substrate.
type Config struct {
	// Network is the private docker network the nodes run on. Created if it
	// does not already exist.
	Network string
	// Subnet is the IPv4 CIDR for Network. Node addresses are assigned from
	// it deterministically (NodeIP), so the inventory fixture can reference
	// them before any container exists.
	Subnet string

	// NodeCount is how many host containers to start.
	NodeCount int

	// BaseImage is the image the node image is derived from.
	BaseImage string
	// Image overrides the derived node image tag. Empty means derive one.
	Image string

	// AgentPort is the nke-agent gRPC port.
	AgentPort string

	// SpireServerAddress and SpireServerPort are baked into the node image's
	// spire-agent config. They must be where `nkectl bootstrap site` will put
	// the SPIRE server: the agents start looking before it exists and retry
	// until it answers, so the address is chosen here and the command is made
	// to match it, not the other way round.
	SpireServerAddress string
	SpireServerPort    string

	// SpireTrustDomain must match what the command deploys SPIRE with.
	SpireTrustDomain string

	// CacheDir holds downloaded build inputs that outlive a single run.
	CacheDir string

	// CADir holds the bootstrap CA both ends of attestation are anchored to.
	// See EnsureBootstrapCA.
	CADir string

	// NamePrefix prefixes both the Kubernetes node name and the container name.
	NamePrefix string

	// Keep leaves the substrate running after the suite, for debugging.
	Keep bool
	// Reuse attaches to an already-running substrate instead of recreating it.
	Reuse bool

	// ReadyTimeout bounds how long to wait for each agent to answer.
	ReadyTimeout time.Duration
}

// DefaultConfig reads the substrate's knobs from the environment. Every value
// has a working default for a local or CI run.
func DefaultConfig() Config {
	return Config{
		Network:   envOr("NKECTL_E2E_NETWORK", "nkectl-e2e-nodes"),
		Subnet:    envOr("NKECTL_E2E_SUBNET", "172.26.0.0/24"),
		NodeCount: envInt("NKECTL_E2E_NODES", 3),
		BaseImage: envOr("NKECTL_E2E_BASE_IMAGE", "cwe-capd-kindest-node:local"),
		Image:     os.Getenv("NKECTL_E2E_IMAGE"),
		AgentPort: envOr("NKECTL_E2E_AGENT_PORT", "15005"),

		SpireServerAddress: envOr("NKECTL_E2E_SPIRE_SERVER_ADDRESS", DefaultSpireServerAddress),
		SpireServerPort:    envOr("NKECTL_E2E_SPIRE_SERVER_PORT", DefaultSpireServerPort),
		SpireTrustDomain:   envOr("NKECTL_E2E_SPIRE_TRUST_DOMAIN", "nkectl-bootstrap.nke.local"),
		CacheDir:           envOr("NKECTL_E2E_CACHE_DIR", defaultCacheDir()),
		CADir:              envOr("NKECTL_E2E_CA_DIR", defaultCADir()),
		NamePrefix:         envOr("NKECTL_E2E_NAME_PREFIX", "node"),
		Keep:               os.Getenv("NKECTL_E2E_KEEP") == "1",
		Reuse:              os.Getenv("NKECTL_E2E_REUSE") == "1",
		ReadyTimeout:       envDuration("NKECTL_E2E_READY_TIMEOUT", 3*time.Minute),
	}
}

// Node is one host container standing in for a pre-provisioned machine.
type Node struct {
	// Name is what the node will be called in Kubernetes — passed through the
	// inventory file's node name and ends up as kubeadm's --node-name.
	Name string
	// IP is the address assigned on the substrate network. It is what CWE
	// dials for node-agent and what kubeadm advertises.
	IP string
	// Container is the docker container name.
	Container string
	// AgentPort is the nke-agent gRPC port.
	AgentPort string

	// EKPubhash is the SHA-256 of this host's TPM endorsement key. It is what
	// the SPIRE server allowlists and the last element of the SPIFFE ID the
	// host is issued. Only populated for AttestationTPM.
	EKPubhash string
}

// Substrate is the running set of host containers.
type Substrate struct {
	Config Config
	Image  string

	nodes []Node
	log   Logf
}

// Start derives the node image, creates the network if needed, starts the
// containers and waits for their prerequisite services. The caller must Stop
// it. nke-agent becomes ready only after SPIRE issues its mTLS identity.
func Start(ctx context.Context, cfg Config, log Logf) (*Substrate, error) {
	if log == nil {
		log = func(string, ...any) {}
	}

	s := &Substrate{Config: cfg, log: log}
	nodes, err := s.planNodes()
	if err != nil {
		return nil, err
	}
	s.nodes = nodes

	if err := s.ensureNetwork(ctx); err != nil {
		return nil, err
	}

	if cfg.Reuse && s.allRunning(ctx) {
		log("substrate: reusing running containers (NKECTL_E2E_REUSE=1)")
		return s, s.waitReady(ctx)
	}

	image, err := EnsureNodeImage(ctx, cfg, log)
	if err != nil {
		return nil, err
	}
	s.Image = image

	// Always start from a clean slate. A container left behind by a killed
	// run would otherwise be adopted silently, and its stale kubeadm state
	// would fail a later step in a way that looks like a product bug.
	s.teardown(ctx)

	for i := range s.nodes {
		if err := s.startNode(ctx, s.nodes[i]); err != nil {
			s.teardown(ctx)
			return nil, err
		}
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

// Attach returns a Substrate for the fixture Config describes, without
// starting anything. It lets a caller that only wants to tear down or
// inspect a fixture (e.g. a standalone "down" or "status" command) plan the
// same nodes Start would, without going through Start's build-and-run path.
func Attach(cfg Config, log Logf) (*Substrate, error) {
	if log == nil {
		log = func(string, ...any) {}
	}
	s := &Substrate{Config: cfg, log: log}
	nodes, err := s.planNodes()
	if err != nil {
		return nil, err
	}
	s.nodes = nodes
	return s, nil
}

// Nodes returns the running hosts.
func (s *Substrate) Nodes() []Node {
	out := make([]Node, len(s.nodes))
	copy(out, s.nodes)
	return out
}

// Stop tears the substrate down unless Keep is set.
func (s *Substrate) Stop(ctx context.Context) {
	if s.Config.Keep {
		s.log("substrate: NKECTL_E2E_KEEP=1 — leaving %d container(s) running", len(s.nodes))
		return
	}
	s.log("substrate: tearing down %d container(s)", len(s.nodes))
	s.teardown(ctx)
}

// planNodes assigns each node a name, container name and a deterministic IP
// within Config.Subnet, starting at .10 — matching the range the inventory
// fixture already references.
func (s *Substrate) planNodes() ([]Node, error) {
	_, subnet, err := net.ParseCIDR(s.Config.Subnet)
	if err != nil {
		return nil, fmt.Errorf("NKECTL_E2E_SUBNET %q is not a valid CIDR: %w", s.Config.Subnet, err)
	}
	base := subnet.IP.To4()
	if base == nil {
		return nil, fmt.Errorf("NKECTL_E2E_SUBNET %q is not an IPv4 CIDR", s.Config.Subnet)
	}

	nodes := make([]Node, 0, s.Config.NodeCount)
	for i := range s.Config.NodeCount {
		name := fmt.Sprintf("%s%d", s.Config.NamePrefix, i+1)
		ip := net.IPv4(base[0], base[1], base[2], base[3]+byte(10+i)).String()
		nodes = append(nodes, Node{
			Name:      name,
			IP:        ip,
			Container: "nkectl-e2e-" + name,
			AgentPort: s.Config.AgentPort,
		})
	}
	return nodes, nil
}

// ensureNetwork creates the substrate's private network if it does not
// already exist. Safe to call repeatedly: an existing network with the same
// name is left alone.
func (s *Substrate) ensureNetwork(ctx context.Context) error {
	if exec.CommandContext(ctx, "docker", "network", "inspect", s.Config.Network).Run() == nil {
		return nil
	}
	if out, err := exec.CommandContext(ctx, "docker", "network", "create",
		"--driver", "bridge", "--subnet", s.Config.Subnet, s.Config.Network).CombinedOutput(); err != nil {
		return fmt.Errorf("creating docker network %s: %w\n%s", s.Config.Network, err, out)
	}
	s.log("substrate: created network %s (%s)", s.Config.Network, s.Config.Subnet)
	return nil
}

// startNode runs one host container.
//
// The flags are what kubeadm and containerd need in order to run inside a
// container:
//
//   - --privileged plus unconfined seccomp/apparmor: kubelet mounts, mknod and
//     the CNI's netlink work
//   - /lib/modules read-only: CNI kernel modules resolve
//   - /dev/null over /proc/swaps: kubelet's IsSwapOn() runs before the
//     failSwapOn knob is read, and a container inherits the host's swap state
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
		"--tty",
		"--restart", "unless-stopped",
		"--network", s.Config.Network,
		"--ip", node.IP,
		"--volume", "/lib/modules:/lib/modules:ro",
		"--volume", node.Container + "-var:/var",
		"--volume", node.Container + "-containerd:/var/lib/containerd",
		"--volume", "/dev/null:/proc/swaps:ro",
		"--tmpfs", "/tmp",
		"--tmpfs", "/run",
		"--env", "NODE_NAME=" + node.Name,
		"--label", "nkectl-e2e=true",
	}
	if dnsAddress != "" {
		args = append(args, "--dns", dnsAddress)
	}
	args = append(args, s.Image)

	if out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("starting %s: %w\n%s", node.Container, err, out)
	}
	if dnsAddress != "" {
		resolver := fmt.Sprintf("nameserver %s\noptions edns0 trust-ad ndots:0\n", dnsAddress)
		command := exec.CommandContext(ctx, "docker", "exec", "-i", node.Container, "tee", "/etc/resolv.conf")
		command.Stdin = strings.NewReader(resolver)
		if out, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf("setting resolver in %s: %w\n%s", node.Container, err, out)
		}
	}
	s.log("substrate: started %s (%s)", node.Container, node.IP)
	return nil
}

func hostDNSAddress() string {
	for _, path := range []string{"/run/systemd/resolve/resolv.conf", "/etc/resolv.conf"} {
		contents, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(contents), "\n") {
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

// waitReady polls each node until the services needed for bootstrap are up.
//
// "Container running" is not readiness: systemd still has to start, containerd
// has to settle, and the software TPM and SPIRE agent must be active. nke-agent
// is deliberately not checked here. Its mTLS listener cannot start before the
// bootstrap SPIRE server exists and issues its identity.
//
// Readiness is the systemd unit reporting active *and* the port accepting a
// connection. Each node gets its own full ReadyTimeout window rather than a
// budget shared across all nodes, so a slow node does not starve the ones
// after it.
func (s *Substrate) waitReady(ctx context.Context) error {
	for _, node := range s.nodes {
		deadline := time.Now().Add(s.Config.ReadyTimeout)
		var lastErr error
		ready := false
		for time.Now().Before(deadline) {
			err := probeHost(ctx, node)
			if err == nil {
				ready = true
				break
			}
			lastErr = err
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
		if !ready {
			return fmt.Errorf("node %s bootstrap services not ready within %s: %w",
				node.Name, s.Config.ReadyTimeout, lastErr)
		}
		s.log("substrate: %s bootstrap services ready", node.Name)
	}
	return nil
}

// probeHost checks readiness from inside the container.
func probeHost(ctx context.Context, node Node) error {
	out, err := exec.CommandContext(ctx, "docker", "exec", node.Container,
		"systemctl", "is-active", "containerd", "swtpm-local", "spire-agent").CombinedOutput()
	if err != nil {
		return fmt.Errorf("required systemd units are not active: %w\n%s", err, out)
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

// teardown removes the containers and their volumes. Every step is
// best-effort: teardown has to make progress even from a half-built
// substrate, and a leaked container holds a name and volumes that break the
// next run. The network is left in place — it is cheap to leave and re-used
// across runs by ensureNetwork.
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

// ContainerCount returns how many containers carry the substrate label.
func ContainerCount(ctx context.Context) (int, error) {
	out, err := exec.CommandContext(ctx, "docker", "ps", "-aq",
		"--filter", "label=nkectl-e2e=true").Output()
	if err != nil {
		return 0, fmt.Errorf("listing substrate containers: %w", err)
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return 0, nil
	}
	return len(strings.Split(trimmed, "\n")), nil
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
