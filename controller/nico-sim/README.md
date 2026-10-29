# nico-sim

nico-sim is a standalone NICo-compatible instance simulator for local
development. One server and one API support two lifecycle backends:

- **nico-vms** creates virtual machines through a fast in-memory runtime, QEMU
  on macOS via vmnet, or KVM/libvirt on Linux.
- **nico-containers** creates systemd-based node containers by invoking Docker
  directly.

Both backends can run at the same time. They share the catalog, API, and common
instance records while keeping backend-specific runtime behavior separate.

This service was called `forge-vms` until [#279](https://github.com/dsx-ai-factory/nvke/pull/279). The rename is a hard cut with
no compatibility shims, so a host that ran the old build needs a one-time migration.

## Selecting a backend

Each seeded site is assigned one backend. An instance create request identifies
a VPC, the VPC identifies its site, and the site selects nico-vms or
nico-containers. The API caller does not pass a separate backend flag.

CAPNICo makes the same choice through `NicoCluster.spec.siteID`. Two clusters
can therefore use different backends concurrently while sharing one CAPNICo
controller and one nico-sim process: point one cluster at `NICo VMs Site` and
another at `NICo Containers Site`.

## Quick Start

```bash
make build                # build server (./bin/nico-sim) + CLI (./bin/nico-simctl)
make test                 # unit tests
make lint                 # go vet
make fmt                  # go fmt
```

Run the server with mock backend (no VMs, instant transitions):

```bash
make run
# or equivalently:
NICO_VMS_LIBVIRT_URI=mock://local make run
```

Run with real VMs (backend auto-detected by OS):

```bash
NICO_VMS_BASE_IMAGE=/path/to/ubuntu-cloud.qcow2 make run-vm
```

The server starts on `:18080` by default. Hit the API with the CLI:

```bash
./bin/nico-simctl site list
./bin/nico-simctl instance create --name my-vm
./bin/nico-simctl instance list
./bin/nico-simctl instance delete --instance-id <id>
```

The CLI defaults (`--base-url http://localhost:18080 --org novxwn552ssc --token nico-simctl-token`) match the server defaults, so you can usually omit them during local dev.

A web dashboard is available at `http://localhost:18080/ui/`. It shows VMs and
containers in separate tables; VM diagnostics and console logs remain available
from the VM table.

## Architecture Overview

The API selects the backend from the requested site. VM sites follow a
**desired-state reconciliation** pattern:

```
                                    ┌─────────────┐
    API request ──► HTTP handlers ──►  SQLite DB  │
                                    └──────┬──────┘
                                           │ poll for desired ≠ observed
                                    ┌──────▼──────┐
                                    │  Reconcile  │  (4 workers, per-instance lock)
                                    │   Engine    │
                                    └──────┬──────┘
                          ┌────────────────┼────────────────┐
                          ▼                ▼                 ▼
                      vm.Mock        vm.DarwinQEMU      vm.Libvirt
                    (in-memory)     (macOS vmnet)      (Linux KVM)
```

1. **API handlers** receive create/delete/reboot requests and write the desired state to SQLite.
2. **Reconciliation workers** poll for instances where `desired ≠ observed` and drive transitions.
3. **VM backends** are pluggable implementations of the `vm.Runtime` interface.

Container sites use the same API handlers but create and delete Docker
containers synchronously. SQLite stores their common instance identity and
request data; Docker supplies their runtime state.

Instance lifecycle:

```
create → new → provisioning → running → ready
delete → deleting → deleted
reboot → rebooting → running → ready
```

Readiness is determined by either an SSH port-22 probe (default) or a cloud-init `phone_home` POST (when enabled by the API caller).

## nico-vms

The VM backend persists desired and observed state in SQLite. Reconciliation
workers create, reboot, and delete the corresponding VM through the configured
runtime. The sections below describe the VM-only networking, boot, diagnostics,
and TPM behavior.

### Migrating from `forge-vms`

This service was called `forge-vms` until
[#279](https://github.com/dsx-ai-factory/nvke/pull/279). The rename is a hard cut with no
compatibility shims, so a host that ran the old build needs a one-time migration.

First destroy any VMs the old build left behind. They are named `forge-vm-*`,
and the new orphan reaper only matches `nico-vm-*`, so they are invisible to
cleanup.

`--nvram` is required: these domains boot UEFI, and libvirt refuses to undefine
a domain that has an NVRAM varstore without it. `--tpm` clears the persistent
swtpm state and is only supported on newer libvirt, hence the fallback.

```bash
virsh list --all --name | grep '^forge-vm-' | xargs -r -n1 virsh destroy
virsh list --all --name | grep '^forge-vm-' | while read -r d; do
  virsh undefine "$d" --nvram --tpm --remove-all-storage ||
    virsh undefine "$d" --nvram --remove-all-storage   # older libvirt: no --tpm
done
```

Then run the old checkout's `scripts/teardown-linux.sh` before this checkout's
`scripts/setup-linux.sh`, because the host state moved:

| Old | New |
|---|---|
| `/var/lib/forge-vms/dhcp-hosts` | `/var/lib/nico-vms/dhcp-hosts` |
| `/var/run/forge-vms-dnsmasq.{pid,leases}` | `/var/run/nico-vms-dnsmasq.{pid,leases}` |
| `/etc/sudoers.d/forge-vms-setcap` | `/etc/sudoers.d/nico-sim-setcap` |

`teardown-linux.sh` does not remove the sudoers file; delete the stale one by
hand.

Finally, re-export the environment: simulator-wide settings use `NICO_SIM_*`,
VM settings use `NICO_VMS_*`, and container settings use `NICO_CONTAINERS_*`.
The old environment names are not read. Use the `nico` provider key in the
gitignored `controller/tilt_config.json`; it starts nico-sim by default.

Already-provisioned VMs also lose diagnostic SSH access because the built-in
account changed from `forge-diag` to `nico-diag`. Recreate them before using
`scripts/get-kubeconfig.sh`.

### Networking Modes

Instances use one of two networking modes, derived from whether an iPXE script is present:

- **Static** (no iPXE): IPAM allocates an IP at create time. Baked into the seed ISO.
- **DHCP** (iPXE present): No IPAM allocation. VM gets IP from dnsmasq. Engine confirms IP from the DHCP lease file after boot.

### BGP and VIP Networking

nico-sim's VM backend runs a BGP speaker that enables kube-vip high availability for Kubernetes control plane VIPs.

**How it works:**

1. Each control plane node runs kube-vip in BGP mode (no leader election). All CP nodes bind the VIP to their loopback interface and announce it as a `/32` route to the nico-vms BGP speaker.
2. kube-vip peers with the bridge gateway (`172.30.0.1:179`). An iptables PREROUTING rule redirects port 179 to the speaker's actual listen port (default 10179).
3. The speaker validates incoming route announcements, then installs ECMP multipath routes on the host via netlink — one nexthop per CP node. The Linux setup configures Layer 4 multipath hashing, so separate connections from the same client can be distributed across control plane nodes while each connection remains on one nexthop. Installing routes requires `CAP_NET_ADMIN` — use `make build-privileged` (or `make run-vm`) which builds the binary and applies the capability via `sudo setcap`. The setup script (`scripts/setup-linux.sh`) installs a sudoers rule that allows your user to run `setcap` on the nico-sim binary without a password, so rebuilds don't prompt for sudo. The Tiltfile uses `make build-privileged` automatically.
4. When a CP node goes down, kube-vip withdraws its announcement. The speaker removes that nexthop from the ECMP route. No ARP cache expiry or leader election needed.

**VIP range (off-link, routed via BGP):**

The control-plane VIP range is a *separate* subnet, not carved from the bridge. Every client (VMs, worker kubelets, and nvke-controller in Kind) reaches the VIP via the BGP route the speaker installs, exercising the same L3/ECMP path as a routed production fabric instead of on-link ARP. The whole bridge subnet is therefore free for DHCP.

```
bridge 172.30.0.0/24:  .1 gateway, .10-.254 DHCP pool (dnsmasq)
VIP    172.31.0.0/24:  nvke-controller allocates cluster VIPs; reached via BGP /32 routes
```

On startup the speaker installs a covering `blackhole` route for the VIP range (so traffic for a VIP that no CP is currently announcing is dropped locally rather than leaking to the host default route), plus a more-specific `/32` ECMP route per announced VIP. The Tiltfile sets `NICO_VMS_VIP_CIDR`, registers the site with nvke-controller, and passes it as the speaker's allowlist; the layout is printed at `tilt up` time.

### Libvirt-Network Mode

For shared hosts where nico-vms must not create host networking of its own — most notably a hostNetwork Kubernetes pod.

By default nico-sim's VM backend **owns its networking**: `scripts/setup-linux.sh` creates the `controllerbr0` bridge, runs a dnsmasq for DHCP, installs iptables MASQUERADE, and nico-sim runs a BGP speaker. That is ideal on a dedicated host (Colossus, a laptop). It is unsafe where nico-sim shares a host's network namespace with other workloads — most notably a **hostNetwork Kubernetes pod** (e.g. a shared GitLab libvirt runner), where a created bridge / iptables rule / dnsmasq leaks onto the node and can break neighbouring jobs or require a reboot.

**Libvirt-network mode** flips this: nico-vms attaches each VM to a **pre-existing libvirt network** (e.g. the stock `default` / `virbr0` NAT network) instead of building its own. nico-vms adds zero host networking, so nothing can leak. Enable it by setting `NICO_VMS_LIBVIRT_NETWORK=<network-name>` (empty — the default — keeps the `controllerbr0` behavior).

| | Default (`controllerbr0`) mode | Libvirt-network mode |
|---|---|---|
| Bridge + NAT | nico-vms creates `controllerbr0` + iptables MASQUERADE (`setup-linux.sh`) | uses the libvirt network's own bridge + NAT; no bridge/iptables created |
| DHCP | nico-vms's own dnsmasq + lease file | libvirt's dnsmasq; leases read via `virsh net-dhcp-leases` |
| Domain XML | `<interface>` on `controllerbr0` | `<interface type='network'><source network='…'/>` |
| Metadata server | NoCloud/EC2 server serves user-data + `phone_home` | **none on the VM's network** → drive readiness by SSH, not `phone_home` |
| BGP / VIPs | speaker installs host routes for kube-vip VIPs | **auto-skipped** when `NICO_VMS_LIBVIRT_NETWORK` is set (`app.go`) |
| libvirt URI | `qemu:///session` (user session) | **`qemu:///system`** — the pre-existing network is system-owned |

**Requirements when enabling it:**

- `NICO_VMS_LIBVIRT_URI=qemu:///system` — the stock `default`/`virbr0` network is owned by the **system** libvirtd. In a runner pod, the system socket is already bind-mounted (`/run/libvirt/libvirt-sock`).
- VM disk images + seed ISOs must live on a path the **system** libvirtd can read (e.g. `/var/lib/libvirt/images`) — a pod-local path (`/builds`, `/tmp`) is invisible to a daemon in another mount namespace.
- The base image must boot with **DHCP** (the libvirt network runs the DHCP server); network mode forces DHCP regardless of iPXE presence.
- `NICO_VMS_BGP_ENABLED=false` is **optional** — BGP is auto-skipped in this mode — but setting it makes intent explicit.

**Readiness (no `phone_home`).** Because there is no nico-vms metadata server on the VM's network, cloud-init's `phone_home` can't report back. Drive readiness by **SSH-probing port 22** (fires early in boot) and then polling whatever agent the VM runs. The NVKE cluster-bringup driver does exactly this: SSH-probe, then a `WaitForAgent` gRPC poll on the nvke-agent port.

**Host prep — `scripts/setup-libvirt-network.sh`.** The network-mode counterpart to `setup-linux.sh`. It (1) ensures the target libvirt network is active, and (2) exempts intra-subnet (VM↔VM) traffic from MASQUERADE by inserting a `RETURN` at the top of the nat `POSTROUTING` chain. Some hosts SNAT inter-VM traffic to the node's own IP — e.g. **AWS EKS via the VPC CNI `AWS-SNAT-CHAIN`**, which masquerades anything not bound for the VPC CIDR. That rewritten source IP breaks IP-bound peer TLS: etcd control-plane joins fail with `"<node-ip>" does not match any of DNSNames [...]`. The `RETURN` preserves the real VM source IPs for VM↔VM traffic while leaving VM→internet NAT intact. The subnet is derived from the network's own XML (not hardcoded), the rule is idempotent, and `--cleanup` removes it (call that from a CI `after_script` so it always runs).

```bash
# set up (CI before_script): ensure the network is up + add the no-masquerade rule
scripts/setup-libvirt-network.sh --network default

# run nico-vms attached to that network
NICO_VMS_LIBVIRT_NETWORK=default \
NICO_VMS_LIBVIRT_URI=qemu:///system \
NICO_VMS_BGP_ENABLED=false \
NICO_VMS_BASE_IMAGE=/var/lib/libvirt/images/noble-server-cloudimg-amd64.img \
  ./bin/nico-sim

# tear down (CI after_script): remove the rule
scripts/setup-libvirt-network.sh --network default --cleanup
```

> This is how the NVKE kubeadm `cluster-bringup` test runs its 4-VM HA cluster on the shared GitLab libvirt runner. The kubeadm repo wraps the above in `tests/cluster-bringup/scripts/setup-gitlab-runner.sh`.

### CIDR ranges

Ranges used by a nico-vms deployment and the tenant clusters it creates — keep them mutually non-overlapping.

| Range | Default | Class | Scope | Set by |
|---|---|---|---|---|
| Bridge / node subnet | `172.30.0.0/24` | B | nico-vms VM IPs (DHCP) + gateway `.1` | `NICO_VMS_BRIDGE_CIDR` |
| Control-plane VIP range | `172.31.0.0/24` | B | tenant API VIPs (off-link, via BGP) | `NICO_VMS_VIP_CIDR` (Tiltfile) |
| Tenant pod CIDR | `192.168.0.0/16` | C | Calico pods in tenant clusters | defaulted in `internal/server/cluster_network.go` |
| Tenant service CIDR | `10.96.0.0/12` | A | tenant cluster Services | kubeadm default |
| kind mgmt network | assigned by Docker; see the note below | B (C on the `192.168` fallback) | Docker network hosting the nvke-controller/Kind management cluster | Kind / Docker |

> **Why `172.30`/`172.31`?** The bridge and VIP ranges are deliberately kept clear of every other consumer:
> - tenant **pod CIDR** `192.168.0.0/16` (class C, Calico default) — a node or VIP inside it gets shadowed by Calico's per-node `/26` `blackhole` routes;
> - tenant **service CIDR** `10.96.0.0/12` and the commonly used corporate/VPN `10.0.0.0/8` (class A) — a `10.x` bridge could shadow a real corporate route on the host;
> - Docker's default address pools (class B, then C). Docker gives each user-defined network the next free `/16` from `172.17.0.0/16` through `172.31.0.0/16`. The `kind` subnet therefore depends on the networks that already exist on the host: `172.18.0.0/16` when `kind` is the first, `172.19.0.0/16` when another network holds `172.18`, and so on. Read the actual value with `docker network inspect kind -f '{{range .IPAM.Config}}{{println .Subnet}}{{end}}' | grep -v :`. The `grep` drops the IPv6 subnet, which can be listed first. Use the inspected value for any address-based rule derived from this row. Docker reaches `172.30`/`172.31` only after it has used `172.17` through `172.29`. After `172.31`, it hands out `/20`s from `192.168.0.0/16`, which overlaps the tenant pod CIDR. Revisit these ranges if a host has that many networks or its `default-address-pools` are reconfigured.
>
> The bridge previously lived at `192.168.200.0/24`, *inside* the pod CIDR, which intermittently blackholed nodes whose IP landed in a Calico `/26` (`ip route get <nodeIP>` → `Invalid argument`; apiserver→kubelet dials fail `connect: invalid argument`). The bridge and VIP ranges were moved out of the pod CIDR to fix this, and pod/service CIDRs were made configurable as the complementary root-cause fix.

**Key config:**

| Variable | Default | Description |
|---|---|---|
| `NICO_VMS_BGP_ENABLED` | `true` | Enable/disable the BGP speaker |
| `NICO_VMS_BGP_LISTEN_PORT` | `10179` | Speaker listen port (179 is redirected here) |
| `NICO_VMS_BGP_LOCAL_ASN` | `4294967000` | Host-side ASN |
| `NICO_VMS_BGP_PEER_ASN` | `65100` | kube-vip ASN |
| `NICO_VMS_BGP_VIP_CIDRS` | `""` (Tilt sets it to the VIP range) | VIP CIDRs the speaker accepts and installs a covering blackhole for |
| `NICO_VMS_BGP_HOLD_TIME` | `9` | BGP hold timer (seconds) |
| `NICO_VMS_BGP_KEEPALIVE` | `3` | BGP keepalive interval (seconds) |
| `NICO_VMS_DHCP_RANGE_END` | last usable bridge host | Last DHCP IP (Tilt derives it from the bridge CIDR) |

**Debugging:**

- `ip route show <VIP>` — should be an ECMP `/32` with one nexthop per healthy CP (`ip route get <VIP>` picks one). If it's absent (falls through to the blackhole/default), no CP is currently announcing it.
- The VIP is off-link, so it is *not* ARPable on `controllerbr0`; reachability is via the BGP route, not ARP.
- nico-sim logs: look for "BGP route announced" (working) or "BGP ignoring disallowed route" (misconfigured)

### Cloud-Init and User Data

User data provided via the create API is served to VMs through the metadata server over HTTP (NoCloud-net). The server exposes per-instance `user-data`, `meta-data`, and `network-config` endpoints.

Production FDE images ship with cloud-init configured to use `http://carbide-pxe.forge/api/v0/cloud-init/` as their NoCloud seed URL, with `http://169.254.169.254:7777` (EC2) as fallback. nico-vms serves both: DNS resolves `carbide-pxe.forge` to the bridge gateway, and iptables redirects port 80→7777 where the metadata server listens. This means user data set via the API is automatically available to the VM at boot — no seed ISO needed for production images.

For cloud images (no iPXE), user data is baked into the seed ISO instead, and the metadata server acts as a supplementary source.

### Metadata Server

The DB-backed metadata server listens on two addresses:

- `169.254.169.254:7777` — EC2-style (link-local)
- `{bridge_gateway}:7777` — production NoCloud (`carbide-pxe.forge` resolves here)

It serves three endpoint families:

- **Production NoCloud** (`/api/v0/cloud-init/{user-data,meta-data,vendor-data,phone_home}`) — what production images use
- **EC2** (`/{version}/meta-data/...`, `/{version}/user-data`) — link-local fallback
- **NoCloud-net per-instance** (`/nocloud/{instanceID}/{user-data,meta-data,network-config}`) — direct access by ID

Instance identity is resolved from the request source IP via DB lookup, with an ARP→MAC fallback for DHCP IP mismatches.

### Diagnostic SSH User

nico-vms injects a diagnostic user into every VM's cloud-init user data, enabling SSH access for debugging and the diagnostics API. Enabled by default.

| Variable | Default | Description |
|---|---|---|
| `NICO_VMS_DIAG_SSH_ENABLED` | `true` | Inject diagnostic user into VM cloud-init |
| `NICO_VMS_DIAG_SSH_USER` | `nico-diag` | Username |
| `NICO_VMS_DIAG_SSH_PASSWORD` | `nico-diag` | Password |

This user is used by `scripts/get-kubeconfig.sh` and the diagnostics API. Disable with `NICO_VMS_DIAG_SSH_ENABLED=false` for production-like testing.

### Diagnostics API

The diagnostics endpoints execute commands on a running VM over SSH (using the diagnostic user) and return the output:

| ID | Description | Streamable |
|---|---|---|
| `agent-logs` | Node Agent (`nvke-agent`) journal logs | Yes |
| `kubelet-logs` | Kubelet journal logs | Yes |
| `containerd-logs` | Containerd journal logs | Yes |
| `cloud-init-logs` | Cloud-init journal logs | No |
| `agent-state` | `/var/lib/nvke/state.json` | No |
| `kubelet-config` | `/var/lib/kubelet/config.yaml` | No |

```bash
# List available diagnostics for an instance
curl http://localhost:18080/v2/org/novxwn552ssc/carbide/instance/<id>/diagnostics

# Get kubelet logs
curl http://localhost:18080/v2/org/novxwn552ssc/carbide/instance/<id>/diagnostics/kubelet-logs

# Stream agent logs (like journalctl -f)
curl http://localhost:18080/v2/org/novxwn552ssc/carbide/instance/<id>/diagnostics/agent-logs?follow=true
```

### TPM Support

The libvirt backend automatically provisions a software TPM 2.0 (swtpm) for each VM when `swtpm_setup` is available on the host. The EK certificate is captured during provisioning and stored in the database, accessible via the instance API.

Requirements:
- `swtpm` and `swtpm-tools` installed (the setup script handles this)
- AppArmor configured to allow swtpm access to the working directory (also handled by the setup script)

If `swtpm_setup` is not found in `$PATH`, VMs are created without a TPM — no error, no configuration needed.

## nico-containers

nico-containers implements the same instance create, get, update, and delete
operations with Docker containers. The service invokes the configured Docker
CLI directly rather than using a Docker client library. SQLite remains the
source for common request data and instance identity, while `docker inspect`
supplies live runtime state and IP addresses.

Create starts a privileged systemd node container on the configured Docker
network. Delete stops and removes it. The default image is the local
`cwe-capd-kindest-node:local` image used by the container E2E flow.

### Cloud-init simulation

The container image does not run cloud-init. nico-containers simulates the
parts required by node bootstrap:

1. It writes `running` to
   `/var/lib/nico-containers/cloud-init/status`.
2. It parses `write_files` entries, creates their parent directories, writes
   their contents, and applies optional permissions and ownership.
3. It runs each `runcmd` string inside the container with `sh -lc`.
4. It writes `done`, or `error` when parsing or execution fails.

The node image replaces `cloud-init` with a small status shim. Calling
`cloud-init status` reads that file and prints the normal `status: running`,
`status: done`, or `status: error` contract, so the NVKE agent follows the same
wait path it uses on a real node.

### Reboot simulation

Containers share the host kernel boot ID, so a plain Docker restart would not
look like a reboot to Kubernetes. Each managed node instead keeps a synthetic
boot ID in `/var/lib/nico-containers/boot-id` and bind-mounts it over
`/proc/sys/kernel/random/boot_id` before systemd starts.

For a reboot request, nico-containers stages a new UUID in
`/var/lib/nico-containers/next-boot-id` and runs `docker restart`. The entrypoint
moves the staged UUID into place before kubelet starts, making the restarted
container report a new boot ID like a real machine.

### Configuration

| Variable | Default | Description |
|---|---|---|
| `NICO_CONTAINERS_ENABLED` | `true` | Enable Docker containers; set to `false` for VM-only custom seeds |
| `NICO_CONTAINERS_REQUIRE_HEALTHY` | `false` | Fail startup when Docker is unavailable |
| `NICO_CONTAINERS_NODE_NETWORK` | `kind` | Docker network for container nodes |
| `NICO_CONTAINERS_NODE_IMAGE` | `cwe-capd-kindest-node:local` | Container node image |
| `NICO_CONTAINERS_DOCKER_BINARY` | `docker` | Docker CLI path |

The default seed includes one container site. A custom seed without a container
site must set `NICO_CONTAINERS_ENABLED=false`.

The container implementation was previously named `nico-mock`. Docker labels
and in-container state paths now use `nico-containers`, so rebuild the node
image and recreate existing local container nodes after updating.

## The CLI: nico-simctl

`nico-simctl` is a thin CLI client for the nico-sim API. Build it with `make build`.

### Instance Commands

```bash
# List instances
./bin/nico-simctl instance list [--site-id ID] [--tenant-id ID]

# Create (defaults to seeded IDs — just --name is required for mock)
./bin/nico-simctl instance create --name my-vm \
  [--tenant-id ID] \
  [--instance-type-id ID] \
  [--vpc-id ID] \
  [--subnet-id ID] \
  [--ssh-key-group-ids ID,ID,...] \
  [--ipxe-script SCRIPT] \
  [--user-data DATA] \
  [--phone-home-enabled]

# Create on nico-containers by selecting its seeded catalog resources
./bin/nico-simctl instance create --name my-container \
  --vpc-id 00000000-0000-0000-0000-000000000002 \
  --subnet-id 00000000-0000-0000-0000-000000000003 \
  --instance-type-id 00000000-0000-0000-0000-000000000005 \
  --ssh-key-group-ids 00000000-0000-0000-0000-000000000004

# Update (patch labels or trigger reboot)
./bin/nico-simctl instance update --instance-id ID \
  [--label env=dev,tier=compute] \
  [--trigger-reboot true]

# Delete
./bin/nico-simctl instance delete --instance-id ID
```

### Console Log Commands

View serial console output from a VM (essential for debugging boot issues):

```bash
# Stream console log via SSH (remote, uses the API to find the VM)
./bin/nico-simctl instance console-log --name my-vm
./bin/nico-simctl instance console-log --name my-vm --dump  # print all and exit

# Stream console log from local disk (no API needed)
./bin/nico-simctl instance console-log-local --name my-vm
./bin/nico-simctl instance console-log-local --name my-vm --dump --lines 100
```

The local variant searches `./var/instances/`, `/tmp/nico-vms-integration/*/instances/`, and `/tmp/nico-sim-test-*/instances/` for log files.

### Other Commands

```bash
./bin/nico-simctl site list
./bin/nico-simctl instance-type list
```

### Global Flags


| Flag         | Default                  | Description     |
| ------------ | ------------------------ | --------------- |
| `--base-url` | `http://localhost:18080` | Server URL      |
| `--org`      | `novxwn552ssc`           | Organization ID |
| `--token`    | `nico-simctl-token`     | Bearer token    |


## Test Server Mode

For quick one-off testing, the server has a `test-server` subcommand that auto-downloads a base image and creates an ephemeral database:

```bash
./bin/nico-sim test-server [--listen-addr :0] [--seed-path PATH]
```

It prints `NICO_SIM_URL=http://localhost:<port>` to stdout (machine-readable) with all other logs on stderr. Auto-detects the best available backend.

## Configuration

All configuration is via environment variables. The server fails fast on invalid config.

### Core


| Variable                | Default                             | Description                                  |
| ----------------------- | ----------------------------------- | -------------------------------------------- |
| `NICO_SIM_LISTEN_ADDR` | `:18080`                            | HTTP listen address                          |
| `NICO_SIM_DB_PATH`     | `./var/nico-sim.db`                | SQLite database path (converted to absolute) |
| `NICO_SIM_SEED_PATH`   | `./internal/seed/default-seed.yaml` | Seed data YAML                               |
| `NICO_VMS_VM_BACKEND`  | `auto`                              | `auto`, `mock`, `libvirt`, or `darwin-qemu`  |


### VM Resources


| Variable                  | Default                   | Description              |
| ------------------------- | ------------------------- | ------------------------ |
| `NICO_VMS_BASE_IMAGE`    | `images/ubuntu-cloud.qcow2` | Path to base qcow2 image (relative to working dir) |
| `NICO_VMS_VM_VCPUS`      | `2`                       | vCPUs per VM             |
| `NICO_VMS_VM_MEMORY_MIB` | `2048`                    | Memory per VM (MiB)      |
| `NICO_VMS_VM_DISK_GIB`   | `20`                      | Disk size per VM (GiB)   |


### Networking


| Variable                    | Default                             | Description                                      |
| --------------------------- | ----------------------------------- | ------------------------------------------------ |
| `NICO_VMS_LIBVIRT_NETWORK` | *(unset)*                           | If set, attach VMs to this pre-existing libvirt network instead of creating `controllerbr0`. Requires `NICO_VMS_LIBVIRT_URI=qemu:///system`. See [Libvirt-Network Mode](#libvirt-network-mode). |
| `NICO_VMS_BRIDGE_NAME`     | `controllerbr0`                     | Network bridge name                              |
| `NICO_VMS_BRIDGE_CIDR`     | `172.30.0.0/24`                     | Bridge subnet (auto-detected on macOS via vmnet) |
| `NICO_VMS_DHCP_HOSTS_DIR`  | `/var/lib/nico-vms/dhcp-hosts`     | dnsmasq hosts directory                          |
| `NICO_VMS_DHCP_LEASE_FILE` | `/var/run/nico-vms-dnsmasq.leases` | dnsmasq lease file                               |


### Backend-Specific


| Variable                       | Default                     | Description                                                     |
| ------------------------------ | --------------------------- | --------------------------------------------------------------- |
| `NICO_VMS_LIBVIRT_URI`        | `qemu:///session`           | libvirt connection URI. Set to `mock://local` for mock backend. |
| `NICO_VMS_QEMU_SYSTEM_PATH`   | *(auto-detected by arch)*   | Path to qemu-system binary                                      |
| `NICO_VMS_QEMU_IMG_PATH`      | `qemu-img`                  | Path to qemu-img                                                |
| `NICO_VMS_CLOUD_LOCALDS_PATH` | `cloud-localds`             | Path to cloud-localds                                           |
| `NICO_VMS_IMAGE_CACHE_DIR`    | `~/.cache/nico-sim/images` | Cache for downloaded kernel/image files                         |


### Engine Tuning


| Variable                             | Default | Description                               |
| ------------------------------------ | ------- | ----------------------------------------- |
| `NICO_VMS_RECONCILE_WORKERS`        | `4`     | Concurrent reconciliation workers         |
| `NICO_VMS_RECONCILE_INTERVAL`       | `5s`    | How often workers poll for work           |
| `NICO_VMS_READINESS_PROBE_INTERVAL` | `3s`    | How often to probe VM readiness           |
| `NICO_VMS_READINESS_TIMEOUT`        | `15m`   | Max time to wait for a VM to become ready |


### Logging


| Variable                        | Default   | Description                                                                                  |
| ------------------------------- | --------- | -------------------------------------------------------------------------------------------- |
| `NICO_SIM_LOG_FORMAT`          | `json`    | `json` or `text` (text has color, use for local dev)                                         |
| `NICO_SIM_LOG_REQUEST_METHODS` | `write`   | HTTP methods to log: `write` (POST/PUT/PATCH/DELETE), `all`, `none`, or comma-separated list |
| `NO_COLOR`                      | *(unset)* | Disable colored output in text mode                                                          |


### Observability


| Variable                             | Default   | Description                                                    |
| ------------------------------------ | --------- | -------------------------------------------------------------- |
| `NICO_VMS_PROMTAIL_TARGETS_FILE`    | *(unset)* | Path to write Promtail file_sd_configs JSON. Empty = disabled. |
| `NICO_VMS_PROMTAIL_LOG_PATH_PREFIX` | *(unset)* | `__path__` prefix for Promtail (may differ from host path in containers) |


### Other


| Variable                         | Default    | Description                                                                 |
| -------------------------------- | ---------- | --------------------------------------------------------------------------- |
| `NICO_VMS_CONSOLE_SSH_PORT`     | `2222`     | SSH port for remote console log access. `0` = auto-assign, `-1` = disabled. |
| `NICO_VMS_CLEANUP_ORPHANED_VMS` | `false`    | Auto-delete VMs that exist in the hypervisor but not in the DB              |
| `NICO_SIM_FILES_DIR`            | *(auto)*   | Directory served via `/files/`. Default: `files/` next to DB.               |

### Local File Server

nico-vms exposes an unauthenticated file server at `GET /files/{path}`. It
serves files from `NICO_SIM_FILES_DIR`, which defaults to `./var/files` when
`NICO_SIM_DB_PATH` is left at its default `./var/nico-sim.db`.

Use typed subdirectories under the file root:

```text
var/files/
  images/
    custom-image.qcow2
  agent/
    nvke-agent
```

Example URLs:

```text
http://localhost:18080/files/images/custom-image.qcow2
http://localhost:18080/files/agent/nvke-agent
```

When the VM backend is enabled, the `nico-sim` Tilt resource provides a
**Stage OS image** button. Enable **Pull NVKR release** to pull an immutable OCI
release reference and disk output. Leave it disabled to download an `https://`
URL or copy an absolute `file://` path. NVKR pulling fails clearly when
`NVKR_BIN` is unset and `nvkr` is unavailable on `PATH`. The destination is
relative to the nico-sim `/files` root and defaults to `images/nvke-node.qcow2`.

The nico-vms dev release uses `carbide-pxe.forge` inside the VM, so the
same agent URL becomes:

```text
http://carbide-pxe.forge:18080/files/agent/nvke-agent
```

To try a local `nvke-agent` build without rebuilding the full node image, build
the agent and copy it into the file root. From the repository root:

```bash
cd agent
make build-amd64

mkdir -p ../controller/nico-sim/var/files/agent
cp bin/linux_amd64/nvke-agent ../controller/nico-sim/var/files/agent/nvke-agent
```

The cloud-init pickup command used by `nvke-nico-vms-24.04-amd64-fde` is:

```bash
sh -c 'u=http://carbide-pxe.forge:18080/files/agent/nvke-agent; if curl -fsSL "$u" -o /tmp/nvke-agent; then install -m 0755 /tmp/nvke-agent /usr/local/bin/nvke-agent && echo "downloaded local nvke-agent" && /usr/local/bin/nvke-agent -version; else echo "local nvke-agent not available at $u; using image agent"; fi'
```

Remove or omit `files/agent/nvke-agent` to fall back to the
image-provided agent.


## Testing

### Unit Tests

```bash
make test       # go test ./...
```

### Integration Tests

Integration tests require the `integration` build tag. They're organized by backend:

```bash
# Mock backend — fast, no real VMs, always works
make test-integration-mock

# Real VMs — backend auto-detected by OS
make test-integration-qemu    # macOS
make test-integration-libvirt  # Linux

# All backends at once
make test-integration-all
```

Filter tests by name:

```bash
RUN_TESTS=TestCreatePollReady make test-integration-mock
```

### Test Control Variables


| Variable                       | Default                     | Description                                                             |
| ------------------------------ | --------------------------- | ----------------------------------------------------------------------- |
| `NICO_VMS_E2E_TESTS_KEEP_VMS` | `false`                     | Keep VMs running after tests for manual SSH inspection                  |
| `NICO_VMS_PRESERVE_LOGS`      | `false`                     | Save console logs + cloud-init artifacts to `/tmp/nico-vms-preserved/` |
| `NICO_VMS_FLOW_TIMEOUT`       | `3s` (mock) / `2m` (real)   | Max wait for instance state transitions                                 |
| `NICO_VMS_FLOW_POLL_INTERVAL` | `50ms` (mock) / `2s` (real) | State polling interval                                                  |
| `REAL_VM_TEST_TIMEOUT`         | `5m`                        | Go test `-timeout` for real VM suites                                   |


### Test Coverage

Mock tests cover:

- Full create → ready → delete lifecycle
- Label patching and reboot flow
- Duplicate create conflict handling
- IP/subnet exhaustion error messages
- Delete of nonexistent instance (404)
- iPXE boot flow (script parsing, image download, invalid scripts)

Real VM tests add:

- SSH connectivity verification (`testuser`/`testpass`)
- VM-to-VM bidirectional TCP connectivity
- Disk persistence across reboot
- Cloud-init user-data execution
- Production FDE image boot (iPXE + LUKS, opt-in via `NICO_VMS_TEST_PRODUCTION_IMAGE=true`)

### Shared Test Flows

Test logic lives in `test/integration/testutil/flows.go` and is called from each backend's test file. This means the same test scenarios run against mock, libvirt, and darwin-qemu.

## Debugging

### Console Logs

Console logs are the primary debugging tool for VM boot issues:

```bash
# Via the CLI (recommended)
./bin/nico-simctl instance console-log-local --name my-vm --dump

# Via shell script (legacy)
./scripts/nico-vms-console-log.sh        # print log
./scripts/nico-vms-console-log.sh -f     # follow
./scripts/nico-vms-console-log.sh -n 50  # last 50 lines
```

Log file locations per instance (under `{db_dir}/instances/{instance_id}/`):

- `console.log` — darwin-qemu backend
- `disk.qcow2.ttyS0.log` — libvirt, cloud images (serial port 0)
- `disk.qcow2.ttyS1.log` — libvirt, production FDE images (serial port 1)

### Health Check

```bash
curl http://localhost:18080/healthz
# {"status": "ok"}
```

No auth required. Use for readiness probes.

### Structured Logging

Run with `NICO_SIM_LOG_FORMAT=text` (the default for `make run`) for human-readable colored logs. Key log sources:

- `controller` — reconciliation engine state transitions
- `http` — API requests (controlled by `NICO_SIM_LOG_REQUEST_METHODS`)
- `cloud-init-metadata` — metadata server requests
- `consolessh` — console SSH server events
- `imagecache` — image download progress
- `bootpatcher` — kernel/grub patching for production images
- `bgp` — BGP speaker route announcements and peer events
- `promtail-targets` — Promtail targets file writer

### Cleaning Up Stuck VMs

```bash
# macOS: list and kill orphaned QEMU processes
make darwin-qemu-list
make darwin-qemu-clean

# Linux (Lima): list and destroy orphaned libvirt domains
make lima-libvirt-list
make lima-libvirt-clean   # LIMA_VM_NAME=nico-vms by default
```

The server also detects orphaned VMs on startup. Set `NICO_VMS_CLEANUP_ORPHANED_VMS=true` to auto-delete them (default: warn only).

## Platform Setup

### macOS

One command installs all dependencies (QEMU, vmnet-helper, cloud-localds):

```bash
make install-deps-homebrew
```

The `darwin-qemu` backend runs QEMU via `vmnet-client --unprivileged` — no sudo needed on macOS 26+. On macOS 15 and earlier, vmnet-client uses sudo only to create the vmnet interface, then drops privileges.

### Linux

```bash
sudo ./scripts/setup-linux.sh
```

This installs QEMU, libvirt, swtpm, and configures:

- `/dev/vtpmx` for container nodes, through `controller/hack/ensure-vtpm-proxy.sh`
- Bridge `controllerbr0` with `172.30.0.0/24`, after removing the `cwebr0` bridge an older nico-vms created
- Metadata IP `169.254.169.254` on the bridge
- dnsmasq DHCP on the bridge
- iptables NAT + port 80→7777 redirect for the metadata server
- Layer 4 ECMP hashing (`net.ipv4.fib_multipath_hash_policy=1`)
- OVMF firmware for UEFI boot
- swtpm for TPM 2.0 emulation (+ AppArmor rules)
- Sudoers rule for passwordless `setcap` on the nico-sim binary (for `CAP_NET_ADMIN`)

#### Cursor Sandbox AppArmor (optional)

If you are using [Cursor](https://cursor.com) on Linux, install the sandbox AppArmor profile:

```bash
curl -fsSL https://downloads.cursor.com/lab/enterprise/cursor-sandbox-apparmor_0.6.0_all.deb -o cursor-sandbox-apparmor.deb
sudo dpkg -i cursor-sandbox-apparmor.deb
```

If you only need NAT for VM internet access:

```bash
sudo sysctl -w net.ipv4.ip_forward=1
sudo iptables -t nat -A POSTROUTING -s 172.30.0.0/24 ! -d 172.30.0.0/24 -j MASQUERADE
```

### Teardown / reset

```bash
make teardown   # or: bash ./scripts/teardown-linux.sh
```

This stops the nico-vms dnsmasq, removes the bridge's iptables rules, deletes
`controllerbr0` and any leftover `cwebr0` (dropping their addresses and the BGP blackhole route), and wipes the
nico-vms DB (`.dev/nico-sim.db`) so it re-seeds from the current bridge CIDR
on next start. It leaves packages, groups, and the sudoers/qemu/AppArmor config
in place (reused by the next setup).

### Remote Testing

For one-shot nico-vms integration tests, sync just this tree and run on the remote:

```bash
./scripts/sync-to-remote.sh [--ssh|--tsh] hostname
# syncs to ~/dev/nvke/controller/nico-sim/ on the remote (override: NICO_SIM_REMOTE_DIR)
```

## Helper Scripts


| Script                             | Description                                                       |
| ---------------------------------- | ----------------------------------------------------------------- |
| `scripts/run-vm-server.sh`         | Start the server with auto-detected backend and base image        |
| `scripts/run-dnsmasq.sh`           | Start dnsmasq for VM DHCP/DNS on the bridge (used by Tilt)        |
| `scripts/get-kubeconfig.sh`        | Extract kubeconfig from a running NVKE cluster via the nvke-controller API + SSH   |
| `scripts/inspect-vm-ips.sh`        | Compare DB IPs vs actual ARP IPs to detect IP drift               |
| `scripts/create-prod-vm.sh`        | Create a production FDE VM with iPXE + LUKS + kubeadm             |
| `scripts/nico-vms-console-log.sh` | View VM console output from disk                                  |
| `scripts/setup-linux.sh`           | Full Linux environment setup (bridge, dnsmasq, iptables, swtpm)   |
| `scripts/teardown-linux.sh`        | Tear down bridge + iptables + dnsmasq and wipe the nico-vms DB    |
| `scripts/install-deps-homebrew.sh` | Install macOS dependencies via Homebrew                           |
| `scripts/darwin-qemu-machines.sh`  | `list` / `clean` QEMU processes on macOS                          |
| `scripts/lima-libvirt-machines.sh` | `list` / `clean` libvirt domains inside a Lima VM                 |
| `scripts/sync-to-remote.sh`        | Rsync codebase to a remote test machine                           |


## Package Guide


| Package                      | What it does                                                                                                            |
| ---------------------------- | ----------------------------------------------------------------------------------------------------------------------- |
| `cmd/nico-sim/`             | Server entrypoint. Wires up config, DB, runtime, engine, HTTP. Also has `test-server` subcommand.                       |
| `cmd/nico-simctl/`          | CLI client. Instance CRUD, console log streaming (local + SSH).                                                         |
| `internal/config/`           | Env var parsing and validation. All configuration lives here.                                                           |
| `internal/http/`             | NICo-compatible API handlers, routes (`router.go`), SDK type conversion (`convert.go`), auth + logging middleware.      |
| `internal/lifecycle/`        | Reconciliation engine. Drives `new → provisioning → running → ready` transitions. DHCP lease reading. Orphan detection. |
| `internal/state/`            | SQLite persistence via `bun` ORM. Models, queries, IPAM allocation. Single `Store` interface for all DB access.         |
| `internal/vm/`               | `Runtime` interface + mock implementation.                                                                              |
| `internal/vm/darwinqemu/`    | macOS QEMU backend. Process management, QMP control, vmnet-client networking.                                           |
| `internal/vm/libvirt/`       | Linux libvirt backend. Domain XML generation, virsh CLI wrapper.                                                        |
| `internal/vm/artifacts/`     | Builds disk overlays (`qemu-img`) and cloud-init seed ISOs (`cloud-localds`). Static vs DHCP network-config.            |
| `internal/cloudinit/`        | Metadata server: EC2, NoCloud-net, and production NoCloud endpoints. Phone-home handler.                                |
| `internal/ipxe/`             | iPXE script parser, image cache, and boot patcher (kernel swap + grub.cfg patching for production images).              |
| `internal/bgp/`              | BGP speaker for kube-vip ECMP load balancing. Validates route announcements and installs multipath routes.              |
| `internal/lokilog/`          | Promtail targets file writer — discovers VM console logs and writes `file_sd_configs` JSON for Loki ingestion.          |
| `internal/sshexec/`          | SSH command execution backend used by the diagnostics API.                                                              |
| `internal/seed/`             | YAML seed data loading. Auto-hydrates subnet CIDRs and VPC prefixes from bridge config.                                 |
| `test/integration/`          | Integration test suites by backend (`mock/`, `libvirt/`, `qemu/`).                                                      |
| `test/integration/testutil/` | Shared test flows, helpers, SSH utilities, base image download.                                                         |


## API Endpoints

All routes require `Authorization: Bearer <token>` except `/healthz`,
`/files/{path}`, and `/ui/`.

### V2 (primary)


| Method | Path                                                     | Description                                   |
| ------ | -------------------------------------------------------- | --------------------------------------------- |
| GET    | `/v2/org/{org}/carbide/instance`                           | List instances                                |
| POST   | `/v2/org/{org}/carbide/instance`                           | Create instance                               |
| PATCH  | `/v2/org/{org}/carbide/instance/{id}`                      | Update (labels, trigger reboot)               |
| DELETE | `/v2/org/{org}/carbide/instance/{id}`                      | Delete instance                               |
| GET    | `/v2/org/{org}/carbide/instance/{id}/console-log`          | Console log (`?follow=true`, `?order=newest`) |
| GET    | `/v2/org/{org}/carbide/instance/{id}/diagnostics`          | List diagnostic items                         |
| GET    | `/v2/org/{org}/carbide/instance/{id}/diagnostics/{itemId}` | Run diagnostic (`?follow=true`)               |
| GET    | `/v2/org/{org}/carbide/site`                               | List sites                                    |
| GET    | `/v2/org/{org}/carbide/instance/type`                      | List instance types                           |
| GET    | `/healthz`                                               | Health check (no auth)                        |
| GET    | `/files/{path}`                                          | Serve files (no auth)                         |
| GET    | `/ui/`                                                   | Web dashboard (no auth)                       |

All NICo SDK list endpoints are implemented (VPCs, subnets, VPC prefixes, SSH key groups, IP blocks, InfiniBand partitions). See `internal/http/router.go` for the full route list.

V1 equivalents exist at `/v1/orgs/{orgId}/instances` etc.

Pagination: `?pageNumber=1&pageSize=100`. Filtering by `siteId` and `tenantId` where applicable.

## Seeded Test Data

The default seed (`internal/seed/default-seed.yaml`) provides IDs used throughout tests and the CLI defaults:


| Backend | Resource | ID | Name |
|---|---|---|---|
| shared | Org | `novxwn552ssc` | |
| shared | Tenant | `d282929f-49d5-4dec-8d27-21ceb30ade19` | Forge-Tenant-NVKE |
| nico-vms | Site | `a1b2c3d4-0001-4000-8000-000000000001` | NICo VMs Site |
| nico-vms | VPC | `a1b2c3d4-0002-4000-8000-000000000001` | mock-vpc |
| nico-vms | Subnet | `a1b2c3d4-0003-4000-8000-000000000001` | mock-subnet |
| nico-vms | VPC Prefix | `a1b2c3d4-0004-4000-8000-000000000001` | mock-vpc-prefix |
| nico-vms | Instance Types | `...-0005-...-000000000001` / `...0002` | cpu2-2gb, cpu2-4gb |
| nico-vms | SSH Key Group | `a1b2c3d4-0006-4000-8000-000000000001` | mock-ssh-keys |
| nico-vms | IP Block | `a1b2c3d4-0007-4000-8000-000000000001` | mock-ip-block |
| nico-vms | IB Partition | `a1b2c3d4-0008-4000-8000-000000000001` | mock-ib-partition |
| nico-containers | Site | `00000000-0000-0000-0000-000000000001` | NICo Containers Site |
| nico-containers | VPC | `00000000-0000-0000-0000-000000000002` | nico-containers-vpc |
| nico-containers | Subnet | `00000000-0000-0000-0000-000000000003` | nico-containers-subnet |
| nico-containers | SSH Key Group | `00000000-0000-0000-0000-000000000004` | nico-containers-ssh-keys |
| nico-containers | Instance Type | `00000000-0000-0000-0000-000000000005` | nico-container-node |


Subnet CIDRs and VPC prefixes are auto-hydrated from `NICO_VMS_BRIDGE_CIDR` at startup if not explicitly set in the seed file.

## nvke-controller Integration

### Manual

Configure CAPNICo's credentials Secret to use the nico-sim endpoint. Clusters
pointing at the VM site use nico-vms; clusters pointing at the container site
use nico-containers.

### With Tilt (recommended)

The Tilt workflow runs nvke-controller and nico-sim locally with hot-reload. The parent
`nico` provider loads the nico-sim component when no external endpoint is set.

Prerequisites: [Tilt](https://tilt.dev), a Kind cluster, Docker.

```bash
# From the controller/ directory:
tilt up
```

This starts:

- **nico-sim** — one API process serving both nico-vms and nico-containers,
  built with `make build-privileged` and live-reloaded on code changes
- **CAPNICo** — one controller configured to use the nico-sim endpoint
- **nico-containers node image** — the systemd image used for container-backed nodes
- **dnsmasq** — DHCP/DNS for VMs on the bridge (if installed on host)
- **Loki + Promtail + Grafana** — observability stack deployed into the Kind cluster
- **nico-sim-log-shipper** — host Promtail container that ships nico-sim and VM console logs to Loki

At startup, Tilt prints the network layout (DHCP pool, VIP pool) derived from `NICO_VMS_BRIDGE_CIDR`.

Grafana is available at `http://localhost:3000` (admin/admin). VM console logs appear under the `vm_console` job with per-instance `instance_name` and `instance_id` labels.
