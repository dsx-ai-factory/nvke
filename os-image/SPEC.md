# NVKE reference node image — contract

> **Which path is this?** This spec covers nodes built from the NVKE OS image — SPIRE agent pre-installed, mTLS on by default, TPM attestation supported. If you are bringing your own Ubuntu nodes without a SPIRE agent or TPM (bootstrapping with `--disable-mtls`), see [`docs/site-bootstrap/pre-provisioned-nodes.md`](../docs/site-bootstrap/pre-provisioned-nodes.md) instead.

This is the contract an OS image must satisfy for NVKE to manage a node.
It is **not** a cluster-ready Kubernetes image: kubeadm and GPU drivers are
installed later through `nvke-agent`. The image includes the Calico manifests
package used during bootstrap.

The in-tree Packer recipe (`packer/`) is one implementation of this contract,
not the only allowed one.

## Must have (baked into the image)

> **Known limitation:** the SPIRE agent requirements below are not final and
> can change between preview releases.

| Item | Requirement |
|---|---|
| OS | Ubuntu 26.04 LTS by default, or 24.04 LTS with `UBUNTU_RELEASE=24.04.5`; both are published. `linux/amd64` and `linux/arm64` both build both variants (`make os-image-fde ARCH=arm64`), each on a runner of its own architecture under KVM. See [README.md](README.md#build-the-os-image). |
| Kernel | Ubuntu's NVIDIA-tuned kernel flavor (`linux-nvidia` on amd64, `linux-nvidia-64k` on arm64), not the generic default — set via autoinstall's `kernel.package` (`cidata/autoinstall-lvm.pkrtpl`), so curtin installs it directly at image-build time instead of the generic kernel ever landing on disk. Matches `node-images`' `nvidia-kernel` role. |
| `nvke-agent` | Built as a Debian package from this repository's `agent/` source at bake time. dpkg owns `/usr/local/bin/nvke-agent`, `/usr/local/bin/nvke-agent-updater`, and their systemd units. `nvke-agent.service` is **enabled**, with startup deferred until first boot. Listens on TCP **15005**. mTLS is on (no `--disable-mtls` in `/etc/default/nvke-agent`). An image-owned systemd drop-in waits for the SPIRE workload API before exec and survives package upgrades. |
| SPIRE agent | `spire-agent` at `/usr/local/bin/spire-agent`, unit `spire-agent.service` **enabled**. Workload API socket at `unix:///tmp/spire-agent/public/api.sock` (the `nvke-agent` default). Installed from the `spire-agent` .deb built at bake time by [`packages/spire-agent/`](packages/spire-agent/), which also owns the two units, the first-boot renderer and its template; `dpkg -S` on any of those paths names that package. |
| Unix workload attestor | SPIRE `WorkloadAttestor "unix"` so root `nvke-agent` can fetch an X.509 SVID. |
| Node attestor | Whichever the instance can use, chosen on first boot: join-token (`NodeAttestor "join_token"`) when cloud-init supplied `SPIRE_JOIN_TOKEN`, TPM (`NodeAttestor "tpm"`, via `/usr/local/lib/spire-agent/tpm_attestor_agent`) otherwise. A site cluster attests its nodes by TPM and mints no join token, so an image that could only do join-token could not join one. This is separate from the FDE variant's use of TPM to seal the disk-encryption key. |
| TPM identity tool | `/usr/local/bin/get_tpm_pubhash` for the image architecture. |
| Calico manifests | An `nvke-cni-calico-manifests` Debian package is installed. |
| Open vSwitch | NVIDIA's DOCA build, `doca-openvswitch-switch`, in kernel mode, installed at bake time from debs pinned by [`packages/doca-ovs/`](packages/doca-ovs/). It replaces Ubuntu's `openvswitch-switch` and keeps its units. DOCA's `ovs-exporter.service`, an unauthenticated metrics endpoint on TCP 6103, is **masked**. |
| etcd data directory | `/var/lib/etcd` exists at mode `0700`, owned by a system `etcd` user and group created at bake time. kubeadm reuses the directory rather than creating its own `root:root` one, which is what CIS 1.1.12 checks. etcd itself still runs as root from `registry.k8s.io/etcd` as a static pod, so this changes what the node reports, not what etcd can do. |
| `multipathd` | **Masked** — both `multipathd.service` and its socket. Ubuntu Server's installer leaves multipathd enabled, and with no blacklist it claims Longhorn's iSCSI-exported volumes (they present as `IET,VIRTUAL-DISK`) through device-mapper: `mkfs` then fails with `is apparently in use by the system; will not make a filesystem here` and newly provisioned volumes never format. No NVKE node boots from a multipath-backed disk, so nothing depends on it. Matches `node-images`' `packages` role, which disables the same unit. |
| Background apt | **Masked** — `apt-daily.timer`, `apt-daily-upgrade.timer`, their services and `unattended-upgrades.service`. Ubuntu runs them a few minutes after first boot; the package job restarts journald, networkd and udevd, and a `systemctl daemon-reload` from kubeadm's kubelet-start phase in that window fails, so the kubelet never starts and the node's init or join is lost. Nodes take updates through a new image, never in place. |
| erofs and dm-verity | 26.04 only. `erofs-utils` is installed, `/etc/modules-load.d/nvke-kata.conf` loads the `erofs` and `dm-verity` modules at boot, and the ext4 root filesystem has the `verity` feature. Kata Containers' erofs snapshotter needs all three: containerd converts image layers to erofs with dm-verity metadata, needs `mkfs.erofs` 1.8.2 or later (24.04 ships 1.7), and refuses to load its CRI plugin unless fs-verity works on `/var/lib/containerd`. It also needs containerd 2.2 or later, so Kubernetes 1.35 or later from `nvke-kubeadm-distro`. |
| Unit ordering | `nvke-spire-configure.service` before `spire-agent.service` before `nvke-agent.service`. `nvke-agent` waits for the workload API socket before exec. |
| First-boot renderer | `/usr/lib/nvke/spire-configure.sh`, driven by `nvke-spire-configure.service`. Renders `/etc/spire-agent/agent.conf` from instance identity. Does nothing unless `/etc/nke/spire.env` exists. |
| Plugin checksum | `/etc/spire-agent/spire-agent.env` holds `TPM_PLUGIN_CHECKSUM`, written by the .deb's postinst from the plugin it installed. `spire-agent.service` runs with `-expandEnv`, so an `agent.conf` that cloud-init wrote with the literal `${TPM_PLUGIN_CHECKSUM}` resolves against this image's own plugin. The two config paths are exclusive: a profile supplies `agent.conf` and no `/etc/nke/spire.env`, or it supplies `/etc/nke/spire.env` and the renderer writes `agent.conf`. |
| Conntrack limits | `conntrack-tune.service` **enabled** (`WantedBy=sysinit.target`), ordered after `systemd-modules-load.service` and before `systemd-sysctl.service`. At every boot `/usr/lib/nvke/configure-conntrack.sh` writes `/run/sysctl.d/10-nke-conntrack.conf`, raising `net.netfilter.nf_conntrack_max` and `nf_conntrack_buckets` to `min(max(RAM_MiB × 64, 1024), 600000)` wherever that is above the kernel's value, and `systemd-sysctl` applies it. `/etc/sysctl.d/10-nke-conntrack-timeouts.conf` sets `nf_conntrack_tcp_timeout_time_wait` to 60s. `/etc/modules-load.d/nf_conntrack.conf` loads `nf_conntrack` at boot. See [Conntrack limits](#conntrack-limits). |

### Conntrack limits

The kernel caps its default conntrack table at 262144 entries on hosts with
more than 4 GiB of RAM, and many short-lived connections through Services
exhaust it. The image raises it to 600000 on any node with 9375 MiB of RAM or
more, and to 64 entries per MiB on smaller VMs.

How the values are applied:

- `conntrack-tune.service` runs early in boot, after
  `systemd-modules-load.service` has loaded `nf_conntrack` and before
  `systemd-sysctl.service`. It computes the target from the booting node's
  `MemTotal` and writes `/run/sysctl.d/10-nke-conntrack.conf` with only the
  keys the target raises. When the kernel is already at or above the target,
  it writes no file, so it never lowers the kernel's value.
- `systemd-sysctl.service` applies that file along with every other
  `sysctl.d` file, before `sysinit.target`, so containerd, kubelet and the CNI
  all start with the limits in place.
- The file lives in `/run`, which is emptied at every boot and is never part
  of the image. The value therefore always comes from the RAM of the node that
  is booting, never from the VM that built the image.
- `/etc/sysctl.d/10-nke-conntrack-timeouts.conf` sets TIME_WAIT to 60s, the
  TIME_WAIT Linux keeps for its own closed TCP sockets. The cost is that a FIN
  retransmitted after 60s with no packets no longer matches its connection,
  so it is not NATed; the file's comment explains when that happens. The UDP
  stream timeout stays at the kernel's default of 120s.
- `sysctl.d` files apply in file-name order, and the last value set for a key
  wins. Both files use the `10-` prefix so that they sort before the files
  operators normally add: a value in any later-sorting file, such as
  `60-conntrack.conf` or `99-local.conf`, overrides them, higher or lower. A
  same-named file in `/etc/sysctl.d` replaces the generated one outright.

These files ship only in this image. Packer installs them as loose files, not
from a package, so nodes built from another image, or imaged before this
change, do not get them, and package updates cannot change them.

The image cannot know which CNI a node will run, so the value it sets is the
same for both; what happens next depends on whether the cluster runs
kube-proxy:

| CNI | kube-proxy | Effective `nf_conntrack_max` |
|---|---|---|
| OVN-Kubernetes | Not deployed | 600000, from `conntrack-tune.service` |
| Calico | Deployed | `max(32768 × logical CPUs, 131072)`, from kube-proxy's defaults; from Kubernetes 1.36, 1048576 on nodes with more than 32 logical CPUs |

kube-proxy computes its own limit from the node's CPU count (not RAM) and
writes it every time it starts. Through Kubernetes 1.37 it writes that value
even when it is lower than the current one, so on Calico nodes with 18 or
fewer logical CPUs it replaces the image's 600000 with less (262144 on an
8-CPU node), and nodes with 19 or more get more. The 600000 floor therefore
holds only on OVN-Kubernetes clusters. kube-proxy leaves TIME_WAIT alone.

Anything that re-applies `sysctl.d` after kube-proxy starts, such as the
`sysctl --system` in `nvke-kubeadm-distro`'s postinst on a Kubernetes version
change, sets `nf_conntrack_max` back to the image's value until kube-proxy
next restarts. On Calico nodes with 18 or fewer CPUs that is a raise; on
nodes with 19 or more it is a reduction.

## Must not bake (instance identity)

These are **site- or node-specific**. They arrive via cloud-init (or equivalent
user-data) on first boot, never at image-build time:

- SPIRE server address and port
- Trust domain
- Trust bundle (PEM)
- Join token (single-use, minted per node)

The Packer build **enables** the units but does not start them against a live
SPIRE server, and it runs `cloud-init clean` so the next boot is a real
instance.

### Cloud-init input

Write these two files. Permissions: `spire.env` and the join token inside it
are secrets (`0600`).

`/etc/nke/spire.env`:

```sh
SPIRE_SERVER_ADDRESS=spire.example.com
SPIRE_SERVER_PORT=8081
SPIRE_TRUST_DOMAIN=example.org
SPIRE_JOIN_TOKEN=REDACTED
```

`SPIRE_JOIN_TOKEN` is optional and selects the attestor. Omit it and the agent
attests by TPM, which is what a site cluster expects: it enrols nodes by TPM EK
thumbprint and mints no tokens. Supply it and the agent uses the token, which is
single-use — after a successful start the image records that and later boots
drop it.

`/etc/nke/trust-bundle.pem`: the SPIRE server's trust bundle, PEM-encoded.

A worked `#cloud-config` is in [README.md](README.md).

After a successful `nvke-agent` start, the image touches
`/var/lib/spire-agent/.nvke-attested` and subsequent boots omit `-joinToken`
(the token is single-use).

## Must have (FDE variant)

`nvke-node-fde.pkr.hcl` is an **opt-in** build variant, not the default image.
It satisfies everything above, plus:

| Item | Requirement |
|---|---|
| Root volume | LVM-on-LUKS2 (`ubuntu-vg`/`ubuntu-lv` on a LUKS2 container), created by `autoinstall-lvm.pkrtpl` (`luks_enabled = true`) at install time. |
| TPM binding | `luks-tpm-firstboot.service` binds the LUKS key to the node's own TPM 2.0 (Clevis `tpm2`) on first real boot — TPM binding is machine-specific and cannot happen at image-build time. TPM-only policy by default (no PCR/measured-boot binding); see [docs/full-disk-encryption.md](docs/full-disk-encryption.md) for the tradeoff. |
| Passphrase removal | The build-time LUKS passphrase slot is removed once the TPM bind is verified unsealable. After that, the volume unlocks **only** via that node's TPM — moving the disk to different hardware, or losing the TPM state, makes it unrecoverable by design. |
| No key material off-image | The only secret on the cidata/autoinstall medium is the shared build-time passphrase (documented default, not a per-node secret); no per-node key, join token, or TPM-derived material is ever written to cloud-init, user-data, or the genesis medium. |
| Unattended first unlock | `GRUB_CMDLINE_LINUX_DEFAULT` carries `luks.key=<passphrase>` (build-time, same shared default) until `luks-tpm-firstboot.sh` strips it post-bind; `files/luks/luks-cmdline-script` reads it. Matches how production feeds an iPXE `luks.key=` kernel parameter. |
| Serial console | `GRUB_CMDLINE_LINUX` carries `console=tty0 console=ttyS0,115200`, so boot/kernel messages (including any LUKS prompt) reach a serial console, not only the VGA framebuffer. |
| Node replacement | Each new node image boot re-runs first-boot binding against its own local TPM — no old-TPM/new-TPM migration step exists or is needed. |

## Must have (non-FDE variant, no encryption)

`nvke-node-non-fde.pkr.hcl` is also **opt-in**, not the default image. It uses
the same LVM layout as the FDE image but configures no encryption. This layout
parity is intentional.

| Item | Requirement |
|---|---|
| Root volume | Plain LVM (`ubuntu-vg`/`ubuntu-lv`), no LUKS container. The root filesystem has a configurable cloud-image label. Its default is `cloudimg-rootfs`. |
| Storage growth | `lvm-growth-firstboot.service` grows the partition, PV, LV, and filesystem to the target disk's full size on first boot. |
| Serial console | Same `console=tty0 console=ttyS0,115200` GRUB parameter as the FDE variant (installer-based images otherwise carry no `console=` at all). |

## Identity rules (runtime)

These are enforced by `nvke-agent`, not by the image:

- Client and agent SVIDs share a trust domain.
- The **client** SPIFFE path must start with `/tenant/nvke/service/`, or
  one of the transitional prefixes `/nvke-service/`, `/nke-service/` and
  `/tenant/nke/service/`, or the agent rejects the handshake.
- The **agent** SVID is whatever this node's SPIRE registration issues. The
  site SPIRE server must have a workload entry for unix uid 0 (root
  `nvke-agent`) parented to the node's agent SPIFFE ID.

Join-token attestation and those workload entries are the operator's SPIRE
server, not this image.

## Ports and paths

| What | Value |
|---|---|
| `nvke-agent` gRPC | TCP `15005` |
| SPIFFE workload API | `unix:///tmp/spire-agent/public/api.sock` |
| Agent state | `/var/lib/nvke/state.json` |
| Agent env | `/etc/default/nvke-agent` (`AGENT_ARGS`) |
| SPIRE config | `/etc/spire-agent/agent.conf` (rendered, not a baked secret) |
| SPIRE data | `/var/lib/spire-agent` |
| Conntrack tuning | `/usr/lib/nvke/configure-conntrack.sh` writes `/run/sysctl.d/10-nke-conntrack.conf` (logs to `journalctl -u conntrack-tune`); TIME_WAIT in `/etc/sysctl.d/10-nke-conntrack-timeouts.conf` |

## Out of scope for this image

- kubeadm / kubelet / container runtime as a cluster-ready stack
- GPU drivers and NVIDIA fabric
- `--disable-mtls` as a supported production mode
- Kubernetes Secrets / etcd encryption-at-rest — a controller/agent concern,
  not an image-build concern; out of scope for `os-image/`

## How to prove it

Boot a VM from the image, feed it the cloud-init files above against a SPIRE
server that has issued a join token and an identity under an accepted client
path for the checker, then:

```bash
nvke-os-image-check --endpoint <node-ip>:15005
```

The checker is a **client**. It uses the local workload API (same pattern as
the controller). It does not embed a SPIRE server.
