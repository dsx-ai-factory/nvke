# AGENTS.md — `controller/nico-sim/`

Guidance for AI coding agents working on nico-vms, the local VM-backed Forge
emulator. Read the root [AGENTS.md](../../AGENTS.md) first.

## Quick Reference

```bash
make build    # Build nico-vms server + nico-vmsctl CLI to ./bin/
make test     # Unit tests (go test ./...)
make lint     # go vet ./...
make fmt      # go fmt ./...
```

Integration tests require the `integration` build tag:
```bash
go test -tags=integration -count=1 -timeout 10m -v ./test/integration/mock/
```

All configuration is via environment variables — see `internal/config/config.go` for the full list.

## Architecture

nico-sim is a standalone NICo-compatible instance simulator with VM and
container backends.

### Reconciliation Engine

The system follows a **desired-state reconciliation** pattern (`internal/lifecycle/engine.go`):

1. **API handler** receives create/delete/reboot requests, writes desired state to SQLite
2. **Reconciliation workers** (default 4) poll for instances where desired != observed state
3. **Per-instance lock** prevents concurrent reconciliation of the same instance
4. Workers drive transitions: `new → provisioning → running → ready` (or `→ error`)

```
create API → new → provisioning (build artifacts, create VM) → running (probe readiness) → ready
delete API → deleting (cleanup VM + DB) → deleted
reboot API → rebooting → running → ready
```

### Two Networking Models

Instances use one of two networking modes, determined by whether an iPXE script is present:

- **Static** (no iPXE): IPAM allocates IP at create time. Baked into seed ISO network-config. VM boots with that exact IP.
- **DHCP** (iPXE present): No IPAM allocation. Seed ISO has DHCP network-config. VM gets IP from dnsmasq. Engine confirms IP from DHCP lease file.

`InstanceRow.UsesDHCP()` derives the mode from `IpxeScript != ""`.

Readiness is determined independently: SSH port 22 probe (default) or cloud-init `phone_home` POST (when `PhoneHomeEnabled` is set by the API caller).

### Metadata Server

Production FDE images have cloud-init configured to reach `http://carbide-pxe.forge/api/v0/cloud-init/` (NoCloud), with `http://169.254.169.254:7777` (EC2) as fallback.

The metadata server is DB-backed (no in-memory maps), listening on:
- `169.254.169.254:7777` — EC2-style (link-local)
- `{bridge_gateway}:7777` — Forge NoCloud (DNS resolves `carbide-pxe.forge` to gateway; iptables redirects port 80→7777)

Instance identity resolved from request source IP via DB lookup, with ARP→MAC fallback for DHCP IP mismatches.

### Key Packages

| Package | Purpose |
|---|---|
| `internal/lifecycle/engine.go` | Reconciliation engine — start here. Provisioning, readiness probing, deletion. |
| `internal/state/` | SQLite persistence via `bun` ORM. Models, queries, IP allocation. `Store` is the single DB access layer. |
| `internal/http/` | Forge API handlers. Routes in `router.go`, SDK conversion in `convert.go`. |
| `internal/vm/` | Pluggable `Runtime` interface. Implementations: `mock.go`, `libvirt/`, `darwinqemu/`. |
| `internal/vm/artifacts/` | Builds disk overlays (`qemu-img`) and seed ISOs (`cloud-localds`). Static vs DHCP network-config. |
| `internal/cloudinit/` | Metadata server: EC2, NoCloud-net, Forge NoCloud endpoints. Phone-home handler writes to DB. |
| `internal/ipxe/` | iPXE script parser, image cache (CloudFront downloads), GRUB bootcmd injection. |
| `internal/bgp/` | BGP speaker for kube-vip ECMP. Validates route announcements, installs multipath routes via netlink. |
| `internal/lokilog/` | Promtail targets file writer. Discovers VM console logs, writes `file_sd_configs` JSON for Loki. |
| `internal/sshexec/` | SSH command execution backend. Used by the diagnostics API (`internal/http/handler_diagnostics.go`). |
| `internal/config/` | Env var config. Backend auto-detection. Bridge CIDR normalization. |
| `internal/seed/` | YAML seed data (tenants, sites, VPCs, subnets). CIDRs hydrated from bridge config at startup. |

### VM Backends

Three implementations of the `vm.Runtime` interface:

| Backend | OS | What it does |
|---|---|---|
| `mock` | any | No real VMs. Instant state transitions. For API/logic testing. |
| `libvirt` | Linux | KVM/QEMU via `virsh`. Bridge networking. UEFI support. |
| `darwinqemu` | macOS | QEMU via `vmnet-client`. Unprivileged vmnet networking. |

Auto-detection: `mock://` URI → mock; macOS → darwin-qemu; otherwise → libvirt.

The libvirt backend auto-provisions a software TPM 2.0 (swtpm) when `swtpm_setup` is available; the EK cert is stored in `InstanceRow.TpmEkCertificate`. Skipped silently if swtpm is not installed.

nico-vms injects a diagnostic SSH user (`nico-diag`/`nico-diag`) into every VM's cloud-init, controlled by `DiagSSHEnabled` in config. This user powers the diagnostics API and `scripts/get-kubeconfig.sh`.

### BGP and VIP Networking

nico-vms runs a BGP speaker (`internal/bgp/speaker.go`) that enables kube-vip high availability
for Kubernetes control plane VIPs.

**How it works:**

1. Each control plane node runs kube-vip in BGP mode (no leader election). All CP nodes bind the
   VIP to their loopback interface and announce it as a `/32` route to the nico-vms BGP speaker.
2. kube-vip peers with the bridge gateway (`172.30.0.1:179`). An iptables PREROUTING rule
   redirects port 179 to the speaker's actual listen port (default 10179).
3. The speaker validates incoming route announcements, then installs ECMP multipath routes on the
   host via netlink — one nexthop per CP node. This gives kernel-level load balancing across all
   control plane nodes. Installing routes requires `CAP_NET_ADMIN` — use `make build-privileged`
   (or `make run-vm`) which applies the capability via `sudo setcap`. The setup script installs a
   sudoers rule allowing passwordless `setcap` on the nico-vms binary, so rebuilds don't prompt.
   The Tiltfile uses `make build-privileged` automatically.
4. When a CP node goes down, kube-vip withdraws its announcement. The speaker removes that nexthop
   from the ECMP route. No ARP cache expiry or leader election needed.

**VIP range (off-link, routed via BGP):**

The control-plane VIP range is a *separate* subnet, not carved from the bridge — clients reach the
VIP via the BGP route the speaker installs (like a routed production fabric), not on-link ARP. The
whole bridge subnet is free for DHCP.

```
bridge 172.30.0.0/24 (NICO_VMS_BRIDGE_CIDR):  .1 gateway, .10-.254 DHCP pool
VIP    172.31.0.0/24 (NICO_VMS_VIP_CIDR):     CWE-allocated cluster VIPs, via BGP /32 routes
```

On startup the speaker installs a covering `blackhole` route for the VIP range (unannounced-VIP
traffic is dropped locally, not leaked to the host default) plus a `/32` ECMP route per announced
VIP. These ranges are deliberately in class B (172.x) to avoid the tenant pod CIDR
(`192.168.0.0/16`), service CIDR (`10.96.0.0/12`), and the commonly used corporate/VPN
`10.0.0.0/8` — see the nico-vms README CIDR table for the full rationale.

**Key config (env vars):**

| Variable | Default | Purpose |
|---|---|---|
| `NICO_VMS_BGP_ENABLED` | `true` | Enable/disable the BGP speaker |
| `NICO_VMS_BGP_LISTEN_PORT` | `10179` | Speaker listen port (179 is redirected here) |
| `NICO_VMS_BGP_LOCAL_ASN` | `4294967000` | Host-side ASN |
| `NICO_VMS_BGP_PEER_ASN` | `65100` | kube-vip ASN |
| `NICO_VMS_BGP_VIP_CIDRS` | auto from bridge | Allowed VIP CIDRs (auto-derived if empty) |
| `NICO_VMS_DHCP_RANGE_END` | auto from bridge | Last DHCP IP (auto-derived if empty) |

**Debugging:**
- `ip route get <VIP>` — should show multipath route with CP node nexthops, not just `dev controllerbr0`
- `sudo arping -I controllerbr0 <VIP>` — all CP nodes respond (VIP on loopback, `arp_ignore=0`)
- nico-vms logs: look for "BGP route announced" (working) or "BGP ignoring disallowed route" (broken)

### Seeded Test Data

The default seed (`internal/seed/default-seed.yaml`) provides IDs used throughout test code:
- Org: `novxwn552ssc`, Tenant: `d282929f-49d5-4dec-8d27-21ceb30ade19`
- Site: `a1b2c3d4-0001-4000-8000-000000000001`, VPC: `a1b2c3d4-0002-4000-8000-000000000001`, Subnet: `a1b2c3d4-0003-4000-8000-000000000001`
- Instance types: `a1b2c3d4-0005-4000-8000-000000000001` (cpu2-2gb), `a1b2c3d4-0005-4000-8000-000000000002` (cpu2-4gb)

### Integration Tests

Tests in `test/integration/` organized by backend (`mock/`, `libvirt/`, `qemu/`). Shared flows in `testutil/flows.go` are called from each backend's test file. Test SSH credentials: `testuser` / `testpass`.

Console log files: `disk.qcow2.ttyS0.log` (cloud images) and `disk.qcow2.ttyS1.log` (production images).
