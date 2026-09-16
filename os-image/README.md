# Reference node OS image

Ubuntu 26.04 image that ships `nke-agent` and `spire-agent` so a site
controller can reach the node over SPIFFE mTLS, built on Ubuntu's
NVIDIA-tuned kernel flavor (`linux-nvidia`/`linux-nvidia-64k`), not the
generic default. The contract is [SPEC.md](SPEC.md).

| Path | What |
|---|---|
| `SPEC.md` | Image contract (bake vs first-boot, ports, identity) |
| `packer/` | Packer + QEMU recipe that produces a qcow2 |
| `files/` | systemd units, SPIRE template, first-boot script |
| `files/luks/` | LUKS/TPM2 first-boot binding assets for the FDE variant |
| `files/lvm/` | First-boot storage-growth assets for the non-FDE variant |
| `docs/full-disk-encryption.md` | FDE variant: build, verify, failure modes |
| `demo-fde.sh` | Runs the host-FDE acceptance scenarios against a built image |
| `cmd/nke-os-image-check` | Connectivity checker (Go) |

## Build the OS image

Prereqs, on a Linux host:

| Tool | Version | Install |
|---|---|---|
| [Packer](https://developer.hashicorp.com/packer) | >= 1.10.0 | https://developer.hashicorp.com/packer/install |
| [Go](https://go.dev/) | per `go.work` | https://go.dev/dl/ |
| `curl` | any | via your package manager |
| `qemu-system-x86_64`, `cloud-localds`, `swtpm`, OVMF | any | via your package manager |
| `xorriso`, `p7zip-full` (`7z`) | any | via your package manager |

Both variants boot the Ubuntu Server installer (not a prebuilt cloud image),
so all of the above are needed even for a first build — see
`.github/workflows/os-image-artifact.yml`'s install step for the exact
package list on Ubuntu. `xorriso`/`7z` are for `prepare-installer-iso.sh`'s
ISO patching (see its header comment for why).

`/dev/kvm` is preferred, not required. `packer/Makefile` probes for it and picks
`accelerator = kvm` when it is usable, `tcg` (software emulation) otherwise —
correct but roughly an order of magnitude slower. Force either mode with
`make os-image-fde ACCELERATOR=tcg`. `boot-smoke.sh` does the same probe.

`make os-image-fde`/`make os-image-non-fde` check for `go` on
`PATH` before they boot the build VM and fail fast if it is missing. The agent package uses nfpm, pinned in `make/tool-versions.mk`.

`nke-agent` is built as a Debian package from this checkout's `agent/` source
using `make -C agent deb-<arch>` and installed with dpkg during provisioning.
Build on Linux for the target architecture with cgo enabled, as required by
the agent package's libc linkage check. The package owns the agent, updater,
and both systemd units. Startup is deferred during the bake with
`NKE_AGENT_SKIP_START=1`; a systemd drop-in preserves the image's SPIRE ordering
and socket wait across package upgrades.
The baked agent is whatever your checkout says — no prebuilt artifact is
pulled. To bake a released agent, check out its `agent/vX.Y.Z` tag first.
The SPIRE agent and TPM tools come from
the `spire-agent` .deb built at bake time out of
[`packages/spire-agent/`](packages/spire-agent/), pinned by version and
SHA-256 in its lock; the build installs that package into the image rather than
dropping the upstream binaries in by hand, so a node ends up with the same
artifact whichever path installed it. The build also installs the Calico
manifests package that the agent uses during offline bootstrap.

Both variants install Ubuntu's NVIDIA-tuned kernel (`kernel.package` in
`cidata/autoinstall-lvm.pkrtpl`), so the generic kernel never lands on disk in
the first place — no post-install kernel swap or reboot.

`ARCH=arm64` builds the arm64 image instead (`ARCH=amd64` is the default):

```bash
make os-image-non-fde ARCH=arm64
make os-image-fde ARCH=arm64
```

Both variants build for arm64. Use an arm64 Linux build host for the arm64
agent package. QEMU can emulate a guest of another architecture with `tcg`,
but that does not supply the cgo cross-toolchain needed to build its agent
package. `ARCH` picks the right ISO URL,
QEMU binary (`qemu-system-aarch64`), machine type (`virt`), and UEFI firmware
(AAVMF instead of OVMF) automatically; nothing else needs to change per-arch.
`demo-fde.sh` and `boot-smoke.sh` take the same `ARCH`.

The build-time cloud-init seed is only for Packer's SSH. The provisioner runs
`cloud-init clean` and drops the build key, so each qcow2 is a golden image.

## Full-disk-encryption (FDE) variant

An opt-in build that lays down an LVM-on-LUKS2 root volume via the Ubuntu
Server installer + autoinstall, then defers TPM 2.0 binding to first boot on
the target hardware. See [docs/full-disk-encryption.md](docs/full-disk-encryption.md)
for the full design, verification steps, and (unrecoverable-by-design)
failure modes.

```bash
cd packer
make os-image-fde
```

Output: `packer/output-fde/nke-node-fde-ubuntu-<version>-<arch>.qcow2` (e.g.
`nke-node-fde-ubuntu-26.04.1-amd64.qcow2`). `LUKS_PASSPHRASE` (default
`changeit` — **set this to a real value for any build that will run on real
hardware**; see [docs/full-disk-encryption.md](docs/full-disk-encryption.md))
sets the build-time-only passphrase, which the shipped image never retains
once a node's first boot completes a verified TPM bind.

## Non-FDE variant (no encryption)

This image uses the same LVM-backed ext4 layout as the FDE variant. It has no
LUKS, Clevis, or TPM binding. `lvm-growth-firstboot.service` expands the
partition, PV, LV, and filesystem after the image boots on a larger disk.

Both variants apply `ROOT_FS_LABEL` to the root filesystem. Its default is
`cloudimg-rootfs`. An image importer can use this label instead of a device
path. Set a different label when you build the image:

```bash
make os-image-non-fde ROOT_FS_LABEL=nke-rootfs
```

```bash
cd packer
make os-image-non-fde
```

Output: `packer/output-non-fde/nke-node-non-fde-ubuntu-<version>-<arch>.qcow2`
(e.g. `nke-node-non-fde-ubuntu-26.04.1-amd64.qcow2`). Boot it with
`boot-smoke.sh` (it needs `OVMF_CODE` set, since this variant boots UEFI —
see the comment at the top of that script; `QCOW` defaults to the single
qcow2 under `output-non-fde/`, so it usually doesn't need to be passed):

```bash
OVMF_CODE=/usr/share/OVMF/OVMF_CODE_4M.fd ./scripts/boot-smoke.sh
```

The smoke test copies the qcow2 to a larger virtual disk. It then boots that
disk and verifies that the first-boot service expands the root filesystem.

## Interactive VM

`make run-vm QCOW=<path>` boots any qcow2 this repo produces and leaves it
running with SSH forwarded, for poking around manually — unlike
`boot-smoke`/`demo-fde.sh`, it runs no checks and doesn't tear itself down.
It prints the exact `ssh` command to use once the VM is up (key path and
port included); by default that's:

```bash
ssh -i packer/.ssh/id_ed25519 -p 2222 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null ubuntu@127.0.0.1
```

**To quit, use `Ctrl-A` then `X`, not plain `Ctrl-C`**: QEMU runs with
`-nographic`, so your terminal is the guest's serial console, and plain
`Ctrl-C` is sent to the guest as literal input (garbling a login prompt)
instead of stopping QEMU. See the comment at the top of
[`packer/scripts/run-vm.sh`](packer/scripts/run-vm.sh) for the full env var
list (`OVMF_CODE` defaults automatically per `ARCH` since every qcow2 this
repo builds is UEFI-only; `TPM=1` attaches a TPM for the FDE image).
Examples:

```bash
make run-vm QCOW=packer/output-non-fde/nke-node-non-fde-ubuntu-26.04.1-amd64.qcow2
make run-vm QCOW=packer/output-fde/nke-node-fde-ubuntu-26.04.1-amd64.qcow2 TPM=1
make run-vm QCOW=packer/output-non-fde/nke-node-non-fde-ubuntu-26.04.1-arm64.qcow2 ARCH=arm64
```

## Release

An `os-image/vX.Y.Z` tag (or `os-image/vX.Y.Z-rc.N`) runs
`.github/workflows/os-image-artifact.yml`, which builds and validates the FDE
and non-FDE images for both architectures and, only if that succeeds, pushes
the qcow2s to GHCR as OCI artifacts under arch-suffixed tags — not one
multi-platform manifest shared across arches, see the workflow's header
comment for why — then cuts a GitHub Release attaching `nke-os-image-check`
and a `checksums.txt` covering all four qcow2s and the checker binary.

The pulled file keeps the name Packer gave it, so it carries the Ubuntu point
release and the architecture and two arches can share a directory:

```bash
oras pull ghcr.io/dsx-ai-factory/nke/os-image-fde:vX.Y.Z-amd64 --platform linux/amd64
oras pull ghcr.io/dsx-ai-factory/nke/os-image-fde:vX.Y.Z-arm64 --platform linux/arm64
oras pull ghcr.io/dsx-ai-factory/nke/os-image-non-fde:vX.Y.Z-amd64 --platform linux/amd64
oras pull ghcr.io/dsx-ai-factory/nke/os-image-non-fde:vX.Y.Z-arm64 --platform linux/arm64
# -> nke-node-{fde,non-fde}-ubuntu-26.04.1-{amd64,arm64}.qcow2
```

**FDE security note**: the published image's `LUKS_PASSPHRASE` is the
shared, documented build-time placeholder (`changeit`), baked into both a
LUKS passphrase slot and GRUB's `luks.key=` kernel parameter until a node's
first real boot completes a verified TPM bind (see
[docs/full-disk-encryption.md](docs/full-disk-encryption.md)). Anyone with
console/physical access to an unbound node can read it and unlock the
volume with it — this is a known, accepted tradeoff for publishing a
build-and-go FDE image without a per-node secret-provisioning story.
Verify a node closed that window with `luks-tpm-verify` before trusting
TPM-only unlock in production.

The workflow's amd64 leg runs on `linux-amd64-gpu-l4-latest-1`, chosen
specifically for real `/dev/kvm` — a container-based pool tried earlier had
none, forcing every bake onto `tcg` software emulation (confirmed directly: a
full FDE bake took 1h39m under `tcg` vs a few minutes locally under real
KVM). The job's "Enable KVM access" step fails immediately if that
assumption turns out wrong, rather than silently falling back to the
~10x-slower emulated path. The arm64 leg runs on that same amd64 runner —
there is no arm64-labeled runner in this org's pool yet — always
cross-building under `tcg`, since KVM can't accelerate a guest arch that
doesn't match the host.

If the FDE/non-FDE build or boot-smoke steps fail in CI, download the
`fde-install-console-logs` artifact from the run — it's the guest's own
serial console output (boot + autoinstall), uploaded regardless of outcome
since there's no VNC/monitor access on the runner to inspect a stuck build
interactively.

The baked `nke-agent` is built from the tagged commit's `agent/` source, so
the agent version is fixed by the ref the workflow runs on.

## First boot (cloud-init)

Do **not** bake SPIRE server, trust domain, bundle, or join token. Pass them
as instance user-data, for example:

```yaml
#cloud-config
write_files:
  - path: /etc/nke/spire.env
    permissions: "0600"
    content: |
      SPIRE_SERVER_ADDRESS=spire.example.com
      SPIRE_SERVER_PORT=8081
      SPIRE_TRUST_DOMAIN=example.org
      SPIRE_JOIN_TOKEN=REDACTED
  - path: /etc/nke/trust-bundle.pem
    permissions: "0644"
    content: |
      -----BEGIN CERTIFICATE-----
      ...
      -----END CERTIFICATE-----
```

`nke-spire-configure.service` reads the files after cloud-init writes them, renders
`/etc/spire-agent/agent.conf`, and only then does `spire-agent` start.
`nke-agent` starts after the workload API socket exists.

The site SPIRE server must also register a workload entry for root
`nke-agent` (unix attestor, uid 0) and a `/nke-service/...` identity for
whatever will dial the node (controller or `nke-os-image-check`).

## Checker

From the repository root (`make build` writes `bin/nke-os-image-check`):

```bash
nke-os-image-check --endpoint 192.0.2.10:15005
```

The process needs a SPIFFE workload API with an SVID whose path starts with
`/nke-service/`, in the same trust domain as the node. Default socket:

`unix:///tmp/spire-agent/public/api.sock`

Override with `--spiffe-socket`. This is the same shape as the controller
dialer; it is not a SPIRE server. For a local SPIRE, see
`controller/scripts/dev-env-spire.sh`.
