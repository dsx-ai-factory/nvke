# NKE reference node image — contract

This is the contract an OS image must satisfy for NKE to manage a node.
It is **not** a cluster-ready Kubernetes image: kubeadm, a CNI, and GPU
drivers are installed later through `nke-agent`. The bar for v1 is that the
controller can reach `nke-agent` over SPIFFE mTLS.

The in-tree Packer recipe (`packer/`) is one implementation of this contract,
not the only allowed one.

## Must have (baked into the image)

TODO:  Need to refine the requirements around spire-agent below

| Item | Requirement |
|---|---|
| OS | Ubuntu 26.04 LTS, `linux/amd64` for the first reference image. `linux/arm64` also builds (`make os-image-non-fde ARCH=arm64`), but is a first pass, not full parity — see [README.md](README.md#build-the-os-image) and the `os-image-artifact.yml` header comment for what's not (yet) covered: no FDE variant, CI runs it cross-emulated under `tcg` rather than on real hardware, and it isn't attached to the GitHub Release yet, only GHCR. |
| Kernel | Ubuntu's NVIDIA-tuned kernel flavor (`linux-nvidia` on amd64, `linux-nvidia-64k` on arm64), not the generic default — set via autoinstall's `kernel.package` (`cidata/autoinstall-lvm.pkrtpl`), so curtin installs it directly at image-build time instead of the generic kernel ever landing on disk. Matches `node-images`' `nvidia-kernel` role. |
| `nke-agent` | Built from this repository's `agent/` source at bake time and installed at `/usr/local/bin/nke-agent`. systemd unit `nke-agent.service` **enabled**. Listens on TCP **15005**. mTLS is on (no `--disable-mtls` in `/etc/default/nke-agent`). The unit waits for the SPIRE workload API before exec. |
| SPIRE agent | `spire-agent` at `/usr/local/bin/spire-agent`, unit `spire-agent.service` **enabled**. Workload API socket at `unix:///tmp/spire-agent/public/api.sock` (the `nke-agent` default). |
| Unix workload attestor | SPIRE `WorkloadAttestor "unix"` so root `nke-agent` can fetch an X.509 SVID. |
| Node attestor | Whichever the instance can use, chosen on first boot: join-token (`NodeAttestor "join_token"`) when cloud-init supplied `SPIRE_JOIN_TOKEN`, TPM (`NodeAttestor "tpm"`, via `/usr/local/lib/spire-agent/tpm_attestor_agent`) otherwise. A site cluster attests its nodes by TPM and mints no join token, so an image that could only do join-token could not join one. This is separate from the FDE variant's use of TPM to seal the disk-encryption key. |
| Unit ordering | `nke-spire-configure.service` before `spire-agent.service` before `nke-agent.service`. `nke-agent` waits for the workload API socket before exec. |
| First-boot renderer | `/usr/lib/nke/spire-configure.sh`, driven by `nke-spire-configure.service`. Renders `/etc/spire-agent/agent.conf` from instance identity. Does nothing unless `/etc/nke/spire.env` exists. |

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

After a successful `nke-agent` start, the image touches
`/var/lib/spire-agent/.nke-attested` and subsequent boots omit `-joinToken`
(the token is single-use).

## Must have (FDE variant)

`nke-node-fde.pkr.hcl` is an **opt-in** build variant, not the default image.
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

`nke-node-non-fde.pkr.hcl` is also **opt-in**, not the default image. It follows
the same installer + autoinstall + LVM flow as the FDE variant —
`autoinstall-lvm.pkrtpl` with `luks_enabled = false` — but configures **no
encryption at all**: no LUKS container, no Clevis, no TPM binding. It exists
to isolate FDE-specific issues from installer-flow issues (same partitioning,
same identity/SSH setup, same first-boot storage growth pattern, minus
encryption), and as a base for anyone who wants the installer-based LVM
layout without opting into encryption.

| Item | Requirement |
|---|---|
| Root volume | Plain LVM (`ubuntu-vg`/`ubuntu-lv`), no LUKS container. |
| Storage growth | `lvm-growth-firstboot.service` grows the partition/PV/LV/filesystem to the target disk's full size on first boot — the same growth logic the FDE variant's `luks-tpm-firstboot.sh` runs, minus everything LUKS/TPM. |
| Serial console | Same `console=tty0 console=ttyS0,115200` GRUB parameter as the FDE variant (installer-based images otherwise carry no `console=` at all). |

## Identity rules (runtime)

These are enforced by `nke-agent`, not by the image:

- Client and agent SVIDs share a trust domain.
- The **client** SPIFFE path must start with `/nke-service/` or the agent
  rejects the handshake.
- The **agent** SVID is whatever this node's SPIRE registration issues. The
  site SPIRE server must have a workload entry for unix uid 0 (root
  `nke-agent`) parented to the node's agent SPIFFE ID.

Join-token attestation and those workload entries are the operator's SPIRE
server, not this image.

## Ports and paths

| What | Value |
|---|---|
| `nke-agent` gRPC | TCP `15005` |
| SPIFFE workload API | `unix:///tmp/spire-agent/public/api.sock` |
| Agent state | `/var/lib/nke/state.json` |
| Agent env | `/etc/default/nke-agent` (`AGENT_ARGS`) |
| SPIRE config | `/etc/spire-agent/agent.conf` (rendered, not a baked secret) |
| SPIRE data | `/var/lib/spire-agent` |

## Out of scope for this image

- kubeadm / kubelet / container runtime as a cluster-ready stack
- CNI, GPU drivers, NVIDIA fabric
- TPM as a SPIRE *node attestor* plugin, and swtpm for that purpose (the FDE
  variant's use of TPM to seal the disk-encryption key is unrelated and in
  scope — see "Must have (FDE variant)" above)
- `--disable-mtls` as a supported production mode
- Kubernetes Secrets / etcd encryption-at-rest — a controller/agent concern,
  not an image-build concern; out of scope for `os-image/`

## How to prove it

Boot a VM from the image, feed it the cloud-init files above against a SPIRE
server that has issued a join token and a `/nke-service/...` identity for the
checker, then:

```bash
nke-os-image-check --endpoint <node-ip>:15005
```

The checker is a **client**. It uses the local workload API (same pattern as
the controller). It does not embed a SPIRE server.
