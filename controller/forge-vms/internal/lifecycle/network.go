// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package lifecycle

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os/exec"
	"strings"

	"github.com/NVIDIA/nke/controller/forge-vms/internal/config"
	"github.com/NVIDIA/nke/controller/forge-vms/internal/netutil"
	"github.com/NVIDIA/nke/controller/forge-vms/internal/state"
	vmruntime "github.com/NVIDIA/nke/controller/forge-vms/internal/vm"
)

// metadataHostname is the DNS name baked into NKE FDE qcow2 images as the
// primary cloud-init NoCloud seed source (97-forge-datasource.cfg).
// forge-vms adds this name to the libvirt network's DNS when running in
// libvirt-network mode so the VM can resolve it to the metadata server.
const metadataHostname = "carbide-pxe.forge"

// NetworkManager abstracts the two networking modes forge-vms supports:
//   - forge/bridge mode (Colossus): creates cwebr0 + dnsmasq + iptables + BGP,
//     runs a metadata server at 169.254.169.254:7777 and {bridge_gw}:7777.
//   - libvirt-network mode (shared GitLab runner): attaches VMs to a pre-existing
//     libvirt network (virbr0); adds a DNS host entry so the image's baked
//     carbide-pxe.forge hostname resolves to the libvirt gateway where
//     forge-vms's metadata server binds on port 80.
type NetworkManager interface {
	// EnsureSetup runs once at engine start. Bridge mode creates cwebr0;
	// libvirt-network mode adds the carbide-pxe.forge DNS host entry to the
	// libvirt network and records the gateway for MetadataAddr.
	EnsureSetup(ctx context.Context) error

	// NeedsDHCP reports whether this instance should obtain its IP via DHCP.
	// Libvirt-network mode always returns true (libvirt's dnsmasq owns the
	// address space). Bridge mode delegates to instance.UsesDHCP().
	NeedsDHCP(instance state.InstanceRow) bool

	// WriteReservation persists a dnsmasq dhcp-host entry for the instance.
	// Libvirt-network mode is a no-op (libvirt's dnsmasq manages its own leases).
	// ip may be empty for a bare MAC reservation (no pinned address yet).
	WriteReservation(instanceID, mac, hostname, ip string, log *slog.Logger)

	// LookupLease returns the confirmed DHCP IP for a MAC address.
	// Libvirt-network mode queries the hypervisor API; bridge mode reads
	// the forge-vms dnsmasq lease file.
	LookupLease(ctx context.Context, mac string) (ip string, ok bool, err error)

	// MetadataAddrs returns all addresses (host:port) the metadata server
	// should bind in this networking mode.
	// Bridge mode: ["169.254.169.254:7777", "{bridge_gw}:7777"].
	// Libvirt-network mode: ["{libvirt_gw}:80"] — the image's baked
	// carbide-pxe.forge seedfrom URL uses port 80; 169.254 is unbindable.
	MetadataAddrs() []string

	// Teardown removes network-mode-specific resources created by EnsureSetup.
	// Libvirt-network mode removes the carbide-pxe.forge DNS host entry.
	// Bridge mode is a no-op.
	Teardown(ctx context.Context) error
}

// newNetworkManager resolves the mode once from cfg and returns the appropriate
// implementation. Called from Engine.New() so no subsequent code re-checks cfg.
func newNetworkManager(cfg config.Config, rt vmruntime.Runtime) NetworkManager {
	if cfg.UseLibvirtNetwork() {
		return &libvirtNetwork{
			runtime:     rt,
			networkName: cfg.LibvirtNetwork,
			libvirtURI:  cfg.LibvirtURI,
		}
	}
	return forgeNetwork{
		runtime:    rt,
		bridgeName: cfg.BridgeName,
		bridgeCIDR: cfg.BridgeCIDR,
		hostsDir:   cfg.DHCPHostsDir,
		leaseFile:  cfg.DHCPLeaseFile,
	}
}

// ---------------------------------------------------------------------------
// forgeNetwork — bridge mode (Colossus / bare-metal)
// ---------------------------------------------------------------------------

type forgeNetwork struct {
	runtime    vmruntime.Runtime
	bridgeName string
	bridgeCIDR string
	hostsDir   string
	leaseFile  string
}

func (n forgeNetwork) EnsureSetup(ctx context.Context) error {
	return n.runtime.EnsureBridge(ctx, n.bridgeName, n.bridgeCIDR)
}

func (n forgeNetwork) NeedsDHCP(instance state.InstanceRow) bool {
	return instance.UsesDHCP()
}

func (n forgeNetwork) WriteReservation(instanceID, mac, hostname, ip string, log *slog.Logger) {
	if err := writeDHCPHostEntry(n.hostsDir, instanceID, mac, hostname, ip); err != nil {
		log.Warn("Failed to write DHCP host entry", "error", err)
	} else {
		log.Info("Wrote DHCP host entry", "mac", mac)
	}
}

func (n forgeNetwork) LookupLease(_ context.Context, mac string) (string, bool, error) {
	if n.leaseFile == "" {
		return "", false, fmt.Errorf("dhcp lease file is not configured and VM has no IP assigned by IPAM")
	}
	ip, ok := readLeaseByMAC(n.leaseFile, mac)
	return ip, ok, nil
}

func (n forgeNetwork) MetadataAddrs() []string {
	addrs := []string{"169.254.169.254:7777"}
	if gw, err := netutil.GatewayIP(n.bridgeCIDR); err == nil {
		addrs = append(addrs, net.JoinHostPort(gw, "7777"))
	}
	return addrs
}

func (n forgeNetwork) Teardown(_ context.Context) error { return nil }

// ---------------------------------------------------------------------------
// libvirtNetwork — libvirt-network mode (shared GitLab runner)
// ---------------------------------------------------------------------------

type libvirtNetwork struct {
	runtime     vmruntime.Runtime
	networkName string
	libvirtURI  string
	gateway     string // populated by EnsureSetup via virsh net-dumpxml
}

// EnsureSetup queries the libvirt network gateway, then adds carbide-pxe.forge
// to the network's DNS so VMs can resolve the metadata hostname to the gateway.
func (n *libvirtNetwork) EnsureSetup(ctx context.Context) error {
	gw, err := n.queryNetworkGateway(ctx)
	if err != nil {
		return fmt.Errorf("libvirt network gateway query failed: %w", err)
	}
	n.gateway = gw
	if err := n.addDNSHost(ctx, gw); err != nil {
		return fmt.Errorf("add dns-host %s → %s: %w", metadataHostname, gw, err)
	}
	return nil
}

func (n *libvirtNetwork) NeedsDHCP(_ state.InstanceRow) bool {
	return true // libvirt's dnsmasq owns the address space for all VMs
}

func (n *libvirtNetwork) WriteReservation(_, _, _, _ string, _ *slog.Logger) {
	// libvirt's dnsmasq manages its own lease database; forge-vms writes nothing
}

func (n *libvirtNetwork) LookupLease(ctx context.Context, mac string) (string, bool, error) {
	return n.runtime.LookupDHCPLease(ctx, n.networkName, mac)
}

// MetadataAddrs returns the single address the metadata server should bind
// in libvirt-network mode: the libvirt gateway on port 80, matching the
// carbide-pxe.forge seedfrom URL baked into the NKE FDE image.
func (n *libvirtNetwork) MetadataAddrs() []string {
	if n.gateway == "" {
		return nil
	}
	return []string{net.JoinHostPort(n.gateway, "80")}
}

// Teardown removes the carbide-pxe.forge DNS host entry from the libvirt
// network so it doesn't persist across unrelated job runs.
func (n *libvirtNetwork) Teardown(ctx context.Context) error {
	if n.gateway == "" {
		return nil
	}
	_ = n.removeDNSHost(ctx, n.gateway) // best-effort; don't block shutdown
	return nil
}

// queryNetworkGateway runs virsh net-dumpxml and extracts the <ip address='...'>
// attribute, which is the gateway IP of the libvirt bridge network.
func (n *libvirtNetwork) queryNetworkGateway(ctx context.Context) (string, error) {
	out, err := n.runVirsh(ctx, "net-dumpxml", n.networkName)
	if err != nil {
		return "", err
	}
	// Parse the first <ip address='...'> element in the network XML.
	// Example: <ip address='192.168.122.1' netmask='255.255.255.0'>
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "<ip ") {
			continue
		}
		for _, attr := range []string{`address="`, `address='`} {
			idx := strings.Index(trimmed, attr)
			if idx < 0 {
				continue
			}
			rest := trimmed[idx+len(attr):]
			quote := string(attr[len(attr)-1])
			end := strings.Index(rest, quote)
			if end < 0 {
				continue
			}
			ip := rest[:end]
			if net.ParseIP(ip) != nil {
				return ip, nil
			}
		}
	}
	return "", fmt.Errorf("no <ip address> found in net-dumpxml output for network %q", n.networkName)
}

// addDNSHost adds carbide-pxe.forge → gw to the libvirt network's DNS.
// Idempotent: deletes any existing entry first so re-runs don't fail.
func (n *libvirtNetwork) addDNSHost(ctx context.Context, gw string) error {
	hostXML := fmt.Sprintf(`<host ip='%s'><hostname>%s</hostname></host>`, gw, metadataHostname)
	// Remove first (best-effort) to make the add idempotent.
	_, _ = n.runVirsh(ctx, "net-update", n.networkName, "delete", "dns-host", hostXML, "--live", "--config")
	_, err := n.runVirsh(ctx, "net-update", n.networkName, "add-first", "dns-host", hostXML, "--live", "--config")
	return err
}

// removeDNSHost removes the carbide-pxe.forge DNS entry from the libvirt network.
func (n *libvirtNetwork) removeDNSHost(ctx context.Context, gw string) error {
	hostXML := fmt.Sprintf(`<host ip='%s'><hostname>%s</hostname></host>`, gw, metadataHostname)
	_, err := n.runVirsh(ctx, "net-update", n.networkName, "delete", "dns-host", hostXML, "--live", "--config")
	return err
}

func (n *libvirtNetwork) runVirsh(ctx context.Context, args ...string) (string, error) {
	finalArgs := append([]string{"-c", n.libvirtURI}, args...)
	cmd := exec.CommandContext(ctx, "virsh", finalArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("virsh %s: %w: %s", strings.Join(args, " "), err, string(out))
	}
	return string(out), nil
}
