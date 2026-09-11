# qcow-imager.efi

Builds `qcow-imager.efi`, the EFI a node chainloads from iPXE to write an NKE
node qcow2 onto its disk.

## What it does

The EFI is a Unified Kernel Image whose payload is `disk_imaging.sh`. On the
target node it picks a serial console, downloads and checksums the qcow2 named
in the kernel command line, chooses a disk, writes the image to it, repairs the
GPT backup header, creates a UEFI boot entry, rewrites GRUB for the detected
serial port, points cloud-init at the metadata server, grows the root
filesystem, and reboots. On failure it halts, so a node does not spin in a PXE
retry loop.

The `nico-vms` integration tests reference a prebuilt copy. Until now nothing
here could produce one.

## Where the build lives

Upstream, in [NVIDIA/infra-controller][ic], which is public and Apache-2.0. It
holds `pxe/common_files/disk_imaging.sh`, the `qcow-imager` and
`qcow-imager-aarch64` mkosi profiles, and the cargo-make tasks that assemble
them. Older notes call it `ncx-infra-controller-core`; that name redirects
there.

This directory checks out the pinned source and applies the patches under
`patches/`. The disk-state patch deactivates old LVM mappings and clears the
old partition signatures before it writes a new image. This prevents stale
device state from hiding the root filesystem in a replacement image.

`make build-efi-x86` then runs the upstream `build-qcow-imager-x86` task.

[ic]: https://github.com/NVIDIA/infra-controller

## What this does not build

The EFI the fleet actually boots is not this one. It is built by the internal
GitLab repo `nke/node-images`, which uses the same clone-and-run shape as this
Makefile but first copies **its own** `disk_imaging.sh` over upstream's, then
cosign-signs the result and uploads it to S3 behind CloudFront. The two payload
copies have diverged a long way: 874 lines in `node-images` against 1141
upstream, differing in about a thousand lines. `node-images` also pins a
different upstream commit.

So this target proves the artifact can be built from source in the open. It
does not reproduce the shipped binary, and the two are not interchangeable
today.

The concrete difference, if anyone is tempted to swap them: every production
iPXE script passes `disk_strategy=` on the kernel command line. The
`node-images` payload parses it and selects a disk by it. Upstream's payload
has no `disk_strategy` at all — it reads a differently named `image_disk`. An
EFI built here would ignore the parameter without complaining and choose a disk
by its own rules. Reconciling the two payloads is a prerequisite for using this
output anywhere real.

## Building

Linux only: mkosi builds inside user namespaces. Both architectures need about
10 GB free, and leave a few hundred MB of package cache under
`.infra-controller/`.

```sh
make -C os-image/efi setup-host      # once per box; needs root or sudo
make -C os-image/efi check-deps      # says what is still missing
make -C os-image/efi build-efi-x86   # -> output/x86_64/qcow-imager.efi
make -C os-image/efi build-efi-arm64 # -> output/aarch64/qcow-imager.efi
```

`setup-host` also relaxes the two AppArmor unprivileged-namespace restrictions
Ubuntu 24.04 applies. Without them mkosi fails inside its sandbox with a
message that does not name a fix.

A cold x86_64 build takes about 20 minutes, most of it mkosi's tools image and
the Ubuntu package set; both are cached, so a rebuild is minutes.
`build-efi-arm64` on an x86_64 host runs the package install under qemu-user
emulation. That works — it has produced a valid `PE32+ EFI application
Aarch64` — but roughly doubles the build. Prefer an arm64 host.

`make clean` removes the checkout, its caches, and `output/`.

## Bumping the pin

`IC_COMMIT` in the `Makefile`. No published matrix says which
`qcow-imager.efi` can image which qcow2, so bump it only after imaging a node
and booting it.

## In CI

`.github/workflows/os-image-efi.yml` builds the x86_64 EFI and uploads it as a
workflow artifact, calling the same `make` targets. There is no aarch64 lane:
the only arm64 label this repo may use is a container-based pool, and mkosi
needs a host where `sysctl -w` works. Emulating aarch64 on the amd64 runner
would double the job for an artifact nothing consumes yet.
