# Reference node OS image

Ubuntu image that ships `nke-agent` and `spire-agent` so a site
controller can reach the node over SPIFFE mTLS, built on Ubuntu's
NVIDIA-tuned kernel flavor (`linux-nvidia`/`linux-nvidia-64k`), not the
generic default. The contract is [SPEC.md](SPEC.md).

26.04 LTS is the default. 24.04 LTS is built and published alongside it, for
consumers whose other software pins that release — see `UBUNTU_RELEASE` under
[Build the OS image](#build-the-os-image).

| Path | What |
|---|---|
| `SPEC.md` | Image contract (bake vs first-boot, ports, identity) |
| `packer/` | Packer + QEMU recipe that produces a qcow2 |
| [`nix/`](nix/README.md) | Nix apps for package builds and the NixOS VM |
| `files/` | systemd units, SPIRE template, first-boot script |
| `files/luks/` | LUKS/TPM2 first-boot binding assets for the FDE variant |
| `files/lvm/` | First-boot storage-growth assets for the non-FDE variant |
| `docs/full-disk-encryption.md` | FDE variant: build, verify, failure modes |
| `demo-fde.sh` | Runs the host-FDE acceptance scenarios against a built image |
| `cmd/nke-os-image-check` | Connectivity checker (Go) |

## Build the OS image

Common prerequisites for image builds:

| Tool | Version | Install |
|---|---|---|
| [Packer](https://developer.hashicorp.com/packer) | 1.15.4 | https://developer.hashicorp.com/packer/install |
| Python | >= 3.10, standard library only | via your package manager |
| `curl` | any | via your package manager |
| `qemu-system-x86_64` or `qemu-system-aarch64`, `qemu-img`, `cloud-localds`, `swtpm`, OVMF/AAVMF | any | via your package manager |
| `xorriso`, `p7zip-full` (`7z`) | any | via your package manager |

Package-builder prerequisites depend on the selected mode:

- **Host builds:** Linux matching the image architecture, plus
  [Go](https://go.dev/dl/) at the version specified by `go.work`.
- **Nix VM builds:** Nix with `nix-command` and `flakes` enabled. Preparing the
  VM requires a matching Linux builder or cached build outputs; see the
  [Nix setup instructions](nix/README.md). Go is provided inside the VM.

Both variants boot the Ubuntu Server installer (not a prebuilt cloud image),
so all of the above are needed even for a first build — see
`.github/workflows/os-image-artifact.yml`'s install step for the exact
package list on Ubuntu. `xorriso`/`7z` are for `prepare-installer-iso.sh`'s
ISO patching (see its header comment for why).

From the repository root, bake and smoke-test with native KVM:

```bash
make -C os-image image-native ARCH=amd64 VARIANT=non-fde
make -C os-image boot-smoke REQUIRE_ACCELERATION=1
```

Use `ARCH=arm64` for an arm64 image on a matching Linux KVM host (`image-native`
always needs one, regardless of which host builds the packages), or `VARIANT=fde`
for the encrypted image. `UBUNTU_RELEASE` selects the Ubuntu point release, and
with it the image flavor — it defaults to `26.04.1`, and `UBUNTU_RELEASE=24.04.5`
builds the 24.04 LTS flavor instead:

```bash
make -C os-image/packer os-image-non-fde ARCH=amd64 UBUNTU_RELEASE=24.04.5
```

The point release lands in the output filename, so the two flavors never
collide on disk (`nke-node-non-fde-ubuntu-24.04.5-amd64.qcow2`). Pass the same
`UBUNTU_RELEASE` to `collect-qcow2-*`, which re-derives that expected name.
A package builder newer than the target release cannot build the image: the
`nke-agent` .deb declares a bare `Depends: libc6`, so an agent compiled against a
newer glibc installs cleanly and then fails to exec. The builder enforces this —
`auto` selects the Nix VM when the host's Ubuntu is newer than the target, and
an explicit `NKE_PACKAGE_BUILDER=host` is rejected before packaging. An older
host is fine, since older glibc runs on newer Ubuntu. `boot-smoke` also runs
`nke-agent -version` inside the guest as a backstop.

`SERIAL_CONSOLE` selects the UART the **installed** system sends kernel output
to. Empty (the default) keeps the architecture default — `ttyS0` on amd64,
`ttyAMA0` on arm64 — so Forge and tenant nodes are unaffected. CSC
control-plane hardware uses the second UART:

```bash
make -C os-image/packer os-image-non-fde SERIAL_CONSOLE=ttyS1
```

The image writes `console=tty0 console=<value>,115200` to
`/etc/default/grub.d/99-console.cfg`. The **last** `console=` owns
`/dev/console`, so the serial one is the owner and the getty follows it.

Only the installed system moves; the installer keeps the first UART, where the
build VM captures its log. A locally built `ttyS1` image therefore shows nothing
in `run-vm`/`boot-smoke`'s serial view — QEMU gives the guest one UART — but
those tools check over SSH and still pass.

Packer validates against `^tty[A-Za-z0-9]+$`. That is a boundary, not a typo
check: the value reaches a shell command in the autoinstall late-commands.

Image baking uses the existing package Make targets.
`NKE_PACKAGE_BUILDER=auto|host|nix` picks the package builder: `auto` builds on
a matching Linux host and otherwise uses the
[NixOS package-builder VM](nix/README.md), which returns the packages to the
host before Packer uploads them to its separate Ubuntu guest.

`image-native` requires usable KVM. The generic `image` target and the existing
`packer/` targets allow `ACCELERATOR=tcg` for development. Emulated builds are
much slower. Image builds accept `kvm` or `tcg` and nothing else —
`check-acceleration` rejects `ACCELERATOR=hvf`, which exists only for `run-vm`
and `boot-smoke`. Make stages artifacts in its owned `packer/artifacts/`
directory; Packer uploads them to `/var/tmp/nke-image`, which the provisioner removes before
finishing the image. Installer ISOs remain in their separate cache.

Boot tools share architecture, firmware, and acceleration selection. They
infer the architecture from the `-amd64.qcow2` or `-arm64.qcow2` filename and
choose KVM on Linux, HVF on matching macOS hosts, or TCG. Firmware discovery
checks OVMF/AAVMF paths, then QEMU's own firmware directory; override with
`OVMF_CODE` and `OVMF_VARS_TEMPLATE` together as a matching pair. Setting only
one is an error. For example, from `os-image/`:

```bash
OVMF_CODE=/usr/share/OVMF/OVMF_CODE_4M.fd \
OVMF_VARS_TEMPLATE=/usr/share/OVMF/OVMF_VARS_4M.fd \
make run-vm QCOW=packer/output-non-fde/nke-node-non-fde-ubuntu-26.04.1-amd64.qcow2
```

`REQUIRE_ACCELERATION=1` rejects software
emulation, and an explicit unavailable `ACCELERATOR=kvm` or `hvf` fails early.

On Apple Silicon, `run-vm` and `boot-smoke` can use HVF with an already-built
compatible arm64 image. The production arm64 kernel is `linux-nvidia-64k`;
compatibility with a particular Mac/QEMU combination still needs live
validation. Apple Silicon macOS package builds use the NixOS VM; image baking still needs the
host Packer/ISO tools. The boot scripts also need `cloud-localds`, SSH and, for
smoke tests, `timeout`.
Run `make -C os-image test-qemu` for the selection and build-entrypoint tests
without booting a VM.

The build checks package-builder prerequisites before starting Packer — Go for
host builds, Nix for VM builds; `make -C os-image/packer check-deps ARCH=arm64`
runs that check on its own. The agent package uses nfpm, pinned in
`make/tool-versions.mk`.

`nke-agent` is built as a Debian package from this checkout's `agent/` source
using `make -C agent deb-<arch>` and installed with dpkg during provisioning.
Host package builds require Linux matching the target architecture with cgo
enabled, as required by the agent package's libc linkage check. Nix mode supplies
that Linux environment and toolchain inside the VM. The package owns the agent, updater,
and both systemd units. Startup is deferred during the bake with
`NKE_AGENT_SKIP_START=1`; a systemd drop-in preserves the image's SPIRE ordering
and socket wait across package upgrades.
The baked agent is whatever your checkout says — no prebuilt artifact is
pulled. To bake a released agent, check out its `agent/vX.Y.Z` tag first.
The SPIRE agent, TPM tools, and DSX IMDS node attestor come from
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

Both variants build for arm64. Host package-builder mode requires an arm64
Linux host and cgo toolchain. On Apple Silicon macOS and mismatched Linux hosts,
`auto` selects the NixOS VM, which provides that toolchain. QEMU can emulate
the image guest with `tcg`; package compilation happens separately. `ARCH` picks the right ISO URL,
QEMU binary (`qemu-system-aarch64`), machine type (`virt`), and UEFI firmware
(AAVMF instead of OVMF) automatically; nothing else needs to change per-arch.
`demo-fde.sh` takes the same `ARCH`; `boot-smoke.sh` reads the image filename.

The build-time cloud-init seed is only for Packer's SSH. The provisioner runs
`cloud-init clean` and drops the build key, so each qcow2 is a golden image.

Each Packer build applies the publication QCOW2 conversion once through its
existing `compress-qcow2.sh` post-processor. Before replacing Packer's output,
that path verifies the converted temporary file is a self-contained,
unencrypted QCOW2 with a positive virtual size and no backing file. The
source-owned Make targets check that the checkout is clean before Packer starts
and again before collection. Collection requires exactly one finalized QCOW2,
preserves its basename, and verifies that the copied publication bytes are
identical. By default, the Makefile exports `PACKER_CACHE_DIR` under the ignored
`packer/iso-cache/` tree so Packer's build/download cache does not dirty the
checkout; callers may override that directory without changing the recipe.

### Direct Packer interface

`packer/` is one directory project. External build systems select exactly one
build and supply already prepared local inputs. Packer does not download image
materials or build packages:

| Build | Exact selector | Output below `output_directory` |
|---|---|---|
| Non-FDE | `nke-node-non-fde.qemu.nke-non-fde` | `non-fde/nke-node.qcow2` |
| FDE | `nke-node-fde.qemu.nke-fde` | `fde/nke-node.qcow2` |

For example, for a local invocation after installing the pinned plugin with
`packer init`:

```bash
cd packer
packer build \
  -only=nke-node-non-fde.qemu.nke-non-fde \
  -var architecture=amd64 \
  -var material_manifest=/run/nke-image/inputs/material-manifest.json \
  -var output_directory=/run/nke-image/output \
  -var ssh_private_key_file=/run/nke-image/state/id_ed25519 \
  -var ssh_public_key_file=/run/nke-image/state/id_ed25519.pub \
  -var efi_firmware_code=/usr/share/OVMF/OVMF_CODE_4M.fd \
  -var efi_firmware_vars=/usr/share/OVMF/OVMF_VARS_4M.fd \
  .
```

The existing Make targets run `packer init` before validation or build, so the
developer and NKE CI workflow retain their current plugin-installation path.
Hermetic external runners instead provide the exact Packer executable and the
QEMU plugin version pinned by `packer/versions.pkr.hcl`, expose the authenticated
plugin tree through `PACKER_PLUGIN_PATH`, and run `packer validate` followed by
`packer build`. They do not run `packer init` during the build. A missing or
incompatible plugin therefore fails validation before QEMU starts rather than
falling back to plugin discovery or download.

The `-only` selector is required. Omitting it is unsupported because Packer
runs both directory-project builds, including the FDE build, by default.

Both builds require the `architecture`, `material_manifest`, `output_directory`,
`ssh_private_key_file`, `ssh_public_key_file`, `efi_firmware_code`, and
`efi_firmware_vars` variables. Supply a matching firmware pair for the
architecture (OVMF for amd64, AAVMF for arm64). The manifest must be an
absolute file path. The requested architecture must be `amd64` or `arm64`, and
both manifest records must match it before Packer selects QEMU or guest
packages. Its sibling `materials/` directory must contain:

- `installer-iso`, the prepared installer ISO;
- `image-inputs`, a tar archive whose root contains the same prepared files that Make
  stages in `packer/artifacts/`.

The explicit `architecture` is the caller's requested build intent, while the
two manifest records are the independently authenticated material facts. Packer
requires all three values to agree before architecture-specific configuration
is selected. This is a breaking interface change for direct Packer callers:
invocations that previously relied on architecture being derived from the
manifest must now pass `-var architecture=<amd64|arm64>`. The existing Make
targets already supply it. Packer 1.14 does not support cross-variable custom
validation, so a mismatch is reported as an `Invalid index` diagnostic for
`local.material_bound_architectures`; that diagnostic means the requested
architecture does not match both material records.

The Make targets accept `SSH_PRIVATE_KEY_FILE` and `SSH_PUBLIC_KEY_FILE` as an
optional pair. When both are set, they must be absolute paths. Make verifies
that they are regular files and that the public key matches the private key,
then passes the paths to Packer. When neither is set, Make generates or reuses
`packer/.ssh/id_ed25519` and `packer/.ssh/id_ed25519.pub`. Setting only one path
is rejected before artifact preparation or Packer execution.

The manifest contract is:

```json
{
  "schemaVersion": "nke.dev/os-image-materials/v1",
  "materials": [
    {
      "binding": "installer-iso",
      "component": "ubuntu-installer",
      "kind": "disk-image",
      "architecture": "amd64",
      "digest": "sha256:<digest>",
      "path": "materials/installer-iso"
    },
    {
      "binding": "image-inputs",
      "component": "nke-image-inputs",
      "kind": "archive",
      "architecture": "amd64",
      "digest": "sha256:<digest>",
      "path": "materials/image-inputs"
    }
  ]
}
```

Both records must declare the same `amd64` or `arm64` architecture. The caller
must keep the authenticated manifest and material bytes immutable for the
entire Packer process. The image-input archive may contain only regular files
and directories; links and special entries are rejected before host-side
extraction.

The direct defaults assume a same-architecture, KVM-capable host:
`accelerator=kvm` and `cpu_model=host`. A caller without usable KVM must set
`accelerator=tcg`, select the architecture-appropriate CPU model (`max` for
amd64 or `max,pauth-impdef=on` for arm64), and provide longer `ssh_timeout` and
`boot_wait` values. The Make targets detect KVM and supply those four settings
automatically.

The manifest's `installer-iso` entry carries the ISO's `sha256:` digest. Packer
checks the staged ISO against that authenticated value before booting it. The
project also checks the `image-inputs` archive against its manifest digest
before extracting it into transient host-side state and uploading the expanded
contents. The archive itself is not copied into the guest disk.

The SSH files are caller-owned attempt state. The direct caller generates the
pair before invoking Packer and supplies both absolute paths. Packer uses the
private key while autoinstall injects the matching public key. Packer's generated
`SSHPublicKey` value is available only to its boot-command template and cannot be
interpolated into `http_content`, so omitting these inputs would not produce a
bootable SSH handoff. `provision.sh` removes the authorized build key before the
image is finalized.

The Make targets remain the normal developer and CI entry point. They prepare
the same material layout, use the supplied key pair when both key variables are
set or generate the Make-owned pair otherwise, run one exact selector, and move
the byte-identical finalized QCOW2 to the existing versioned output filename.

After an interrupted build, `make clean-packer-scratch` removes only transient
Packer inputs and output state while preserving the downloaded ISO and prepared
packages. `make clean-fde-output` and `make clean-non-fde-output` additionally
remove the selected variant's completed output.

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
`boot-smoke.sh` (`QCOW` defaults to the single qcow2 under `output-non-fde/`;
firmware is discovered automatically):

```bash
./scripts/boot-smoke.sh
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
list (`OVMF_CODE` defaults automatically from the image filename since every qcow2 this
repo builds is UEFI-only; `TPM=1` attaches a TPM for the FDE image).
Examples:

```bash
make run-vm QCOW=packer/output-non-fde/nke-node-non-fde-ubuntu-26.04.1-amd64.qcow2
make run-vm QCOW=packer/output-fde/nke-node-fde-ubuntu-26.04.1-amd64.qcow2 TPM=1
make run-vm QCOW=packer/output-non-fde/nke-node-non-fde-ubuntu-26.04.1-arm64.qcow2
```

## Release

An `os-image/vX.Y.Z` tag (or `os-image/vX.Y.Z-rc.N`) runs
`.github/workflows/os-image-artifact.yml`, which builds and validates the FDE
and non-FDE images for both architectures and, only if that succeeds, pushes
the qcow2s to GHCR as OCI artifacts under arch-suffixed tags — not one
multi-platform manifest shared across arches, see the workflow's header
comment for why — then cuts a GitHub Release attaching `nke-os-image-check`
and a `checksums.txt` covering all four qcow2s and the checker binary.

The workflow selects the source-owned Make collection target for each matrix
variant. Make copies the final QCOW2 under its existing basename and verifies
that the publication copy is byte-identical. Existing `oras pull` output
remains one qcow2.

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

The workflow builds each architecture natively on the `arc-amd64-kvm` and
`arc-arm64-kvm` runners, chosen for real `/dev/kvm`: under `tcg` software
emulation a full FDE bake took 1h39m on amd64, and arm64 never finished. The
job's "Enable KVM access" step fails immediately if `/dev/kvm` is absent or
unusable, rather than silently falling back to the ~10x-slower emulated path.
The Makefile still retains its portable same-architecture KVM probe and TCG
fallback for local builds.

If the FDE/non-FDE build or boot-smoke steps fail in CI, download the
`install-console-logs-<arch>[-ubuntu24.04]-<variant>` artifact from the run — it's the guest's own
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

## Conntrack limits

At every boot, `conntrack-tune.service` writes
`/run/sysctl.d/10-nke-conntrack.conf`, sizing `nf_conntrack_max` from the
node's RAM: 600000 on nodes with 9375 MiB of RAM or more, less on smaller VMs.
It only ever raises the kernel's values, and `systemd-sysctl` applies the file
early in boot. `/etc/sysctl.d/10-nke-conntrack-timeouts.conf` sets TIME_WAIT
to 60s. On Calico clusters kube-proxy re-sets the limit from the node's CPU
count when it starts, which is lower than 600000 on nodes with 18 or fewer
logical CPUs; see [SPEC.md](SPEC.md#conntrack-limits).

To check a node:

```bash
cat /run/sysctl.d/10-nke-conntrack.conf   # what boot generated, and from how much RAM
sysctl net.netfilter.nf_conntrack_max net.netfilter.nf_conntrack_buckets net.netfilter.nf_conntrack_count
journalctl -u conntrack-tune
```

To override, add a file to `/etc/sysctl.d`: a same-named
`10-nke-conntrack.conf` replaces the generated one, and any later-sorting
file, such as `60-conntrack.conf` or `99-local.conf`, overrides individual
keys, higher or lower. `boot-smoke.sh` and
`demo-fde.sh` run `packer/scripts/check-conntrack.sh` against each built
image.

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

If the node runs `nke-agent --grpc-auth-mode=tls-jwt`, pass `--tls-jwt`:

```bash
nke-os-image-check --endpoint 192.0.2.10:15005 --tls-jwt
```

That agent writes an `NKEAuth` record after the TLS handshake and before the
HTTP/2 preface, so a probe that does not expect it reads the record as a
corrupt preface and reports an opaque transport failure. With the flag, the
checker consumes the record and logs the token's `sub` claim as
`unverified_auth_subject` — useful when a node's identity does not match the
UUID the controller dialled. The checker does not verify the signature; only
the controller validates the token.
