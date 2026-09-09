# Reference node OS image

Ubuntu 26.04 image that ships `nke-agent` and `spire-agent` so a site
controller can reach the node over SPIFFE mTLS. The contract is [SPEC.md](SPEC.md).

| Path | What |
|---|---|
| `SPEC.md` | Image contract (bake vs first-boot, ports, identity) |
| `packer/` | Packer + QEMU recipe that produces a qcow2 |
| `files/` | systemd units, SPIRE template, first-boot script |
| `files/luks/` | LUKS/TPM2 first-boot binding assets for the FDE variant |
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
| `qemu-system-x86_64` or `qemu-system-aarch64`, `cloud-localds`, `swtpm`, OVMF or AAVMF | any | via your package manager |
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

`make os-image-fde`/`make os-image-non-fde` check for `go` on `PATH` before
they boot the build VM and fail fast with the install link above if it's
missing.

`nke-agent` is built from this checkout's `agent/` source (cross-compiled to
the image architecture when the host differs), so the baked agent is whatever your
checkout says — no prebuilt artifact is pulled. To bake a released agent,
check out its `agent/vX.Y.Z` tag first. The SPIRE agent and TPM plugin are
pinned by version and SHA-256 from GitHub Releases. The build also creates and
installs the pinned Calico manifests Debian package used by `nke-agent`.

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
`nke-node-fde-ubuntu-26.04-amd64.qcow2`). `LUKS_PASSPHRASE` (default
`changeit` — **set this to a real value for any build that will run on real
hardware**; see [docs/full-disk-encryption.md](docs/full-disk-encryption.md))
sets the build-time-only passphrase, which the shipped image never retains
once a node's first boot completes a verified TPM bind.

## Non-FDE variant (no encryption)

This image uses the Ubuntu installer, a separate `/boot` partition, and a
direct ext4 root partition. It has no LUKS, Clevis, TPM binding, or LVM layer.
Its partition boundaries match the FDE/LVM image. This lets the Forge qcow
imager find the new root even if the old disk used Ubuntu LVM.

```bash
cd packer
make os-image-non-fde
# On an arm64 host, or under QEMU TCG on another architecture:
make os-image-non-fde-arm64
```

Output: `packer/output-non-fde/nke-node-non-fde-ubuntu-<version>-<arch>.qcow2`
(e.g. `nke-node-non-fde-ubuntu-26.04-amd64.qcow2`). Boot it with
`boot-smoke.sh` (it needs `OVMF_CODE` set, since this variant boots UEFI —
see the comment at the top of that script; `QCOW` defaults to the single
qcow2 under `output-non-fde/`, so it usually doesn't need to be passed):

```bash
OVMF_CODE=/usr/share/OVMF/OVMF_CODE_4M.fd ./scripts/boot-smoke.sh
# arm64
ARCH=arm64 OVMF_CODE=/usr/share/AAVMF/AAVMF_CODE.no-secboot.fd \
  OVMF_VARS_TEMPLATE=/usr/share/AAVMF/AAVMF_VARS.fd ./scripts/boot-smoke.sh
```

## Interactive VM

`make run-vm QCOW=<path>` boots any qcow2 this repo produces and leaves it
running with SSH forwarded, for poking around manually — unlike
`boot-smoke`/`demo-fde.sh`, it runs no checks and doesn't tear itself down.
**To quit, use `Ctrl-A` then `X`, not plain `Ctrl-C`**: QEMU runs with
`-nographic`, so your terminal is the guest's serial console, and plain
`Ctrl-C` is sent to the guest as literal input (garbling a login prompt)
instead of stopping QEMU. See the comment at the top of
[`packer/scripts/run-vm.sh`](packer/scripts/run-vm.sh) for the full env var
list (`OVMF_CODE` for the UEFI variants, `TPM=1` to attach a TPM for the FDE
image). Examples:

```bash
make run-vm QCOW=packer/output-non-fde/nke-node-non-fde-ubuntu-26.04-amd64.qcow2 OVMF_CODE=/usr/share/OVMF/OVMF_CODE_4M.fd
make run-vm QCOW=packer/output-non-fde/nke-node-non-fde-ubuntu-26.04-arm64.qcow2 OVMF_CODE=/usr/share/AAVMF/AAVMF_CODE.no-secboot.fd
make run-vm QCOW=packer/output-fde/nke-node-fde-ubuntu-26.04-amd64.qcow2 OVMF_CODE=/usr/share/OVMF/OVMF_CODE_4M.fd TPM=1
```

## Release

An `os-image/vX.Y.Z` tag (or `os-image/vX.Y.Z-rc.N`) runs
`.github/workflows/os-image-artifact.yml`, which builds and validates both
the FDE and non-FDE images and, only if that succeeds, pushes both qcow2s to
GHCR as OCI artifacts and cuts a GitHub Release for the tag attaching
`nke-os-image-check` and `checksums.txt` (covering both qcow2s and the
checker binary).

```bash
oras pull ghcr.io/nvidia/nke/os-image-fde:vX.Y.Z --platform linux/amd64
oras pull ghcr.io/nvidia/nke/os-image-non-fde:vX.Y.Z --platform linux/amd64
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

The workflow runs on `linux-amd64-gpu-l4-latest-1`, chosen specifically for
real `/dev/kvm` — a container-based pool tried earlier had none, forcing
every bake onto `tcg` software emulation (confirmed directly: a full FDE
bake took 1h39m under `tcg` vs a few minutes locally under real KVM). The
job's "Enable KVM access" step fails immediately if that assumption turns
out wrong, rather than silently falling back to the ~10x-slower emulated
path.

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

Cloud-init writes the identity files and restarts
`nke-spire-configure.service`. The unit renders
`/etc/spire-agent/agent.conf`, and only then does `spire-agent` start. The
renderer must not order itself after `cloud-final.service`: it is enabled for
`multi-user.target`, and that dependency creates a systemd ordering cycle on
Ubuntu.
`nke-agent` starts after the workload API socket exists.

The site SPIRE server must also register a workload entry for root
`nke-agent` (unix attestor, uid 0) and a `/nke-service/...` identity for
whatever will dial the node (controller or `nke-os-image-check`).

For `nkectl bootstrap site`, including the TPM mTLS and single-node plaintext
cloud-init variants, see the identity-mode addendum in [SPEC.md](SPEC.md).

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

Use `--plaintext` only to verify the single-node bootstrap mode. The image
installs this checker at `/usr/local/bin/nke-os-image-check` so cloud-init can
avoid restarting an agent that already responds over plaintext.
