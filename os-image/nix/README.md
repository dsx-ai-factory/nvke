<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Build Linux packages with Nix

Nix prepares a Linux VM with the tools needed to build NVKE's Debian packages.
Use this on Apple Silicon macOS or when your Linux host differs from the target
architecture. Matching Linux hosts use the existing direct package build by default.

On macOS the VM is nixpkgs' stock Linux builder, fetched from the binary cache;
see [macOS: nixpkgs' Linux builder](#macos-nixpkgs-linux-builder). On a Linux
host it is the package VM this directory defines.

## Install Nix

Follow the [official Nix installation instructions](https://nixos.org/download/)
for your OS. For macOS or Linux with systemd, the multi-user installer is:

```bash
curl --proto '=https' --tlsv1.2 -L https://nixos.org/nix/install | sh -s -- --daemon
```

Open a new terminal and check `nix --version`. If your system already manages Nix
through NixOS or nix-darwin, use that installation.

On macOS the installer hooks Nix into `/etc/zshrc` and `/etc/bashrc`, but bash
reads `/etc/bashrc` only as a login shell. If `nix` is missing in bash, add the
hook to `~/.bashrc`:

```bash
if [ -e /nix/var/nix/profiles/default/etc/profile.d/nix-daemon.sh ]; then
  . /nix/var/nix/profiles/default/etc/profile.d/nix-daemon.sh
fi
```

Add this line to `~/.config/nix/nix.conf` (create the directory/file if needed),
or enable the equivalent setting in your existing NixOS/nix-darwin configuration:

```ini
experimental-features = nix-command flakes
```

That is all a Mac needs. **A Linux host whose CPU differs from the target needs
one more step:** Nix needs a Linux builder matching the target architecture to
prepare the package VM, unless all its build outputs are in a binary cache. A
fresh Nix installation does not configure one. See Nix's
[remote builder setup](https://nix.dev/manual/nix/stable/advanced-topics/distributed-builds).
QEMU's software emulation applies when running the prepared VM.

## Build packages

Run from the repository root:

```bash
NKE_TARGET_UBUNTU_SERIES=26.04 nix run ./os-image#build-packages-arm64 -- --output "$PWD/artifacts/arm64"
```

`NKE_TARGET_UBUNTU_SERIES` is the image's Ubuntu release; it selects the DOCA
Open vSwitch build. Use `build-packages-amd64` for x86 packages. The command starts the VM, copies
in your checkout's tracked and untracked files (not ignored ones), builds the
packages, copies the results to your output directory, and shuts down the VM.
The first build takes longer; later builds reuse a persistent disk containing
the Nix store and compiler/download caches.
The builder copies top-level artifacts, replacing same-named files such as
`nvke-agent.deb`, and synchronizes `debs/` with deletion of stale packages.
Unrelated files outside `debs/` are preserved. See the [module README](../README.md)
for how this fits into an OS image build and for `check-deps`.

`os-image/flake.nix` is the Nix configuration; `flake.lock` pins its dependencies.

### Linux: the package VM

The persistent disk grows as needed up to 32 GiB and is reused per architecture.
Set `NKE_BUILDER_STATE_DIR` to choose where the disk, keys, and logs live;
the default is `~/.cache/nke/builders/<arch>` (or under `XDG_CACHE_HOME`). Remove
that directory to reset a builder back to a fresh disk. The guest's SSH port is
forwarded on `127.0.0.1:5422` for arm64 and `5423` for amd64, clear of the ports
`run-vm.sh`, `boot-smoke.sh` and Packer use. Set `NKE_BUILDER_SSH_PORT` to run
another instance.

The runner generates a private SSH host key in that state directory and pins
its public key before connecting. QEMU supplies the private key to the guest
through a systemd credential; sshd reads it from runtime memory, not the Nix
store. All SSH and rsync connections require that pinned key, including the
first boot and boots with a fresh disk. Keep the state directory private.

### macOS: nixpkgs' Linux builder

A Mac cannot build the package VM without a Linux builder, so on macOS the
build runs in nixpkgs' stock `darwin.linux-builder` from the pinned nixpkgs.
Its guest comes from the binary cache. The flake changes only the host side:
no network port, and the package VM's 32 GiB disk, 4 GiB of memory and 4
cores. Nothing is installed as a service, no Nix setting changes, and your
user does not need to be trusted.

- **First run** downloads the builder's guest, about 720 MiB for arm64, and
  then, inside the guest, the build tools, about 2.4 GB, onto its persistent
  disk. `make -C os-image/packer check-deps` fetches the guest. A nixpkgs pin
  whose builder guest is not cached cannot be used on a Mac; the build says so.
- **How it runs.** The VM has no SSH session and no network port. Its serial
  console logs in its unprivileged `builder` user, and the build types one
  command there. Files go through the VM's shared directory: your checkout as
  a tarball, an environment file and the guest script go in; a log, the
  artifacts and a status come back. In the guest, `builder` runs the build
  inside `nix develop ./os-image#package-build`, with rootless podman, then
  powers the VM off. The guest's log streams to your terminal.
- **State** lives in `stock/` under the builder state directory, by default
  `~/.cache/nke/builders/<arch>/stock`: the persistent disk `root.qcow2`, which
  holds the guest's Nix store and build caches, plus `console.log` and the
  last `build.log`. The disk does not shrink when the guest deletes files, so
  expect it to grow to most of its 32 GiB over a few builds. Delete `stock/` to
  start again from a fresh disk. If this Mac ran the package VM before,
  everything else in the builder state directory is that VM's disk and keys,
  which are no longer used and can be deleted.
- **One run at a time** can use that state; a second fails with "already in
  use". A builder VM left running by a killed build holds it until that VM
  exits. If no build is running, stop its `qemu-system-aarch64` process
  (`qemu-system-x86_64` for amd64).
- **Credentials** such as `NGC_API_KEY` reach the guest only in the
  environment file, which the guest deletes once read. Nothing secret is typed
  on the console, which is logged. The shared directory that carries the file
  is created for each run and deleted when the build exits, or by the next run
  after a hard stop.
- **Interrupting** a build with Ctrl-C powers the guest off from its console
  once the guest has logged in; before that, it stops QEMU.
- **A damaged disk.** If the VM does not reach its console login, or the
  guest's Nix database is corrupt, on a disk reused from an earlier run, the
  build deletes the disk and retries once.
- **amd64** packages build in nixpkgs' x86_64 builder under emulation, which is
  much slower. That path has not been tested.

### Enter the build tools

On a Linux host, `nix develop ./os-image#package-build` opens a shell with the
build tools the VM installs, for the host's own architecture, and sets `CC` to
`nke-ubuntu-cc` (described below), so cgo builds such as the agent's do not
link against the host's glibc. Both lists come from `nix/build-tools.nix`; the
shell adds podman, which the VM gets from its podman module. The VM's base
system is not part of the shell. On macOS this is the shell the stock builder's
guest runs the build in.

## How the builder produces Ubuntu-compatible binaries

The guest sets `CC` to `nke-ubuntu-cc`, a wrapper around the pinned nixpkgs
`zig_0_15` C compiler. It targets the guest architecture with the glibc 2.39
ABI (Ubuntu 24.04's baseline, and still compatible with the newer Ubuntu 26.04
used by the Packer recipe), using Zig's bundled libc headers and link stubs.
The result uses Ubuntu's standard dynamic loader and system libc, with no Nix
store RPATH — the Zig dependency supplies the target ABI independently of the
NixOS host's own libc.

The existing agent Makefile honors `CC`, so native `make -C agent deb-arm64`
(or `deb-amd64`) uses this compiler without changing the agent's build rules.
Cgo stays enabled for the agent's dynamic libc dependency; the updater
currently uses no cgo and remains statically linked.
