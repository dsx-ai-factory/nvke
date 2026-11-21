<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Build Linux packages with Nix

Nix prepares a Linux VM with the tools needed to build NVKE's Debian packages.
Use this on Apple Silicon macOS or when your Linux host differs from the target
architecture. Matching Linux hosts use the existing direct package build by default.

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

### macOS: trust your user once

The package VM is a Linux system, so a Mac needs a Linux builder to prepare it.
The build starts a temporary one for you (below), but Nix accepts a builder
chosen on the command line only from a **trusted user**. Trust your account
once, then restart the Nix daemon:

```bash
echo "trusted-users = root $USER" | sudo tee -a /etc/nix/nix.conf
sudo launchctl kickstart -k system/org.nixos.nix-daemon
```

`nix store info --json` then reports `"trusted":true`. On nix-darwin, set
`nix.settings.trusted-users` instead. The build checks this before it starts
anything and never changes it for you.

### macOS: the temporary Linux builder

`make -C os-image/packer check-deps` and `build-packages-<arch>` first check
the local store and the binary caches. If the package VM still needs Linux
builds, they start nixpkgs' stock `darwin.linux-builder` from the pinned
nixpkgs, prepare the package VM through it, and shut it down before any
package is built. Later runs skip it until the VM's definition changes. Nothing
is installed as a service and no system configuration changes.

- **First run** downloads the builder's guest from the public Nix cache, about
  720 MiB for arm64. It cannot be built locally, since no Linux builder exists
  yet, so a nixpkgs pin whose `darwin.linux-builder` is uncached cannot
  bootstrap.
- **Network:** its SSH listens on `127.0.0.1` only, port 31024 for arm64 and
  31025 for amd64; set `NKE_BOOTSTRAP_PORT` to change it. The amd64 builder
  runs under emulation on Apple silicon and is much slower.
- **Host key:** the guest's host key is nixpkgs' public test key. It identifies
  the guest type but does not authenticate the VM against a local process
  squatting its port, the same trade-off as upstream `darwin.linux-builder`.
- **State:** the client key, `known_hosts` and the console log live in
  `bootstrap/` under the builder state directory, by default
  `~/.cache/nke/builders/<arch>/bootstrap`. The builder's disk is created for
  each run and deleted when it exits. One run at a time can use that state;
  a second fails with "already in use".

Linux hosts keep their existing behavior. A Linux host whose CPU differs from
the target still needs a matching Linux builder, see Nix's
[remote builder setup](https://nix.dev/manual/nix/stable/advanced-topics/distributed-builds),
or cached guest outputs. QEMU's software emulation applies when running the
prepared VM.

## Build packages

Run from the repository root:

```bash
NKE_TARGET_UBUNTU_SERIES=26.04 nix run ./os-image#build-packages-arm64 -- --output "$PWD/artifacts/arm64"
```

`NKE_TARGET_UBUNTU_SERIES` is the image's Ubuntu release; it selects the DOCA
Open vSwitch build. Use `build-packages-amd64` for x86 packages. The command starts the VM, copies
in your checkout, builds the packages, copies the results to your output
directory, and shuts down the VM. The first build takes longer; later builds
reuse a persistent disk containing the Nix store and compiler/download caches.
The builder copies top-level artifacts, replacing same-named files such as
`nvke-agent.deb`, and synchronizes `debs/` with deletion of stale packages.
Unrelated files outside `debs/` are preserved. See the [module README](../README.md)
for how this fits into an OS image build and for `check-deps`.

`os-image/flake.nix` is the Nix configuration; `flake.lock` pins its dependencies.

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
