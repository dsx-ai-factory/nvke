<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# The Ubuntu snapshot the image is built from

Every Ubuntu package in the node image comes from one snapshot of the Ubuntu
archive on [snapshot.ubuntu.com](https://snapshot.ubuntu.com), at one fixed
snapshot ID. Two builds of the same commit therefore install the same package
versions, wherever and whenever they run. This is
[#2206](https://github.com/dsx-ai-factory/nvke/issues/2206), and it is
transitional: once the release lock (#2204) exists, the ID is read from it,
and once the signed suite carries Ubuntu (#2213), the snapshot sources go away.

This page records what the build does, and the proof the issue asked for: how
the Ubuntu installer combines its ISO's package pool with an overridden
mirror, and whether the installed system ends up on snapshot versions.

## What the build does

- **One variable.** `ubuntu_snapshot` in `packer/variables.pkr.hcl` holds the
  snapshot ID (`YYYYMMDDTHHMMSSZ`). It is the only place the ID is pinned. Make
  passes `UBUNTU_SNAPSHOT` through only when it is set, to try another ID for
  one build. `ubuntu_snapshot_base_url` (`UBUNTU_SNAPSHOT_BASE_URL`) defaults to
  `https://snapshot.ubuntu.com/ubuntu`, for a build network that reaches the
  service through a mirror.
- **The installer.** The autoinstall `apt` section sets the primary and the
  security mirror to `<base>/<ID>` for every architecture, turns off
  `geoip`, and sets `fallback: abort`. It also gives curtin an explicit
  deb822 `sources_list` template whose URIs are `$PRIMARY` and `$SECURITY`;
  see "24.04: the live archive survived" for why. Subiquity applies the
  section to the installer and to the installed system.
- **`updates: all`.** The installer upgrades the whole installed system from
  the snapshot before the late-commands run. `updates: security` is not
  enough; see below.
- **Two checks in `provision.sh`**, by `scripts/check-ubuntu-snapshot.py`:
  - After `apt-get update`, before anything is installed: every apt source is
    the snapshot or the image's local NVKE repository (`/usr/lib/nvke/debs`).
  - After cleanup, while the indexes still exist: every installed package's
    version is downloadable from the snapshot or the local repository
    (`nvke-agent`, installed from its own `.deb`, is exempt). Any other
    version fails the build. The check prints a digest of the package set:
    `package set sha256: <hex> (<n> packages)`.
- **`boot-smoke`.** After cloud-init's first boot, no apt source names
  `archive.ubuntu.com`, `ports.ubuntu.com` or `security.ubuntu.com`, and the
  Ubuntu sources name a snapshot ID.

## One archive for both architectures

`snapshot.ubuntu.com/ubuntu/<ID>/` serves the arm64 indexes and arm64 `.deb`s,
so both architectures use it. `snapshot.ubuntu.com/ubuntu-ports/<ID>/`
answers HTTP 401 for every ID tried, including 2025 dates (checked
2026-11-19). The live `archive.ubuntu.com` and `ports.ubuntu.com` now serve
byte-identical `InRelease` files. The issue's original wording (`ubuntu-ports`
on arm64) does not work.

## How the installer combines the ISO pool with the snapshot

Observed on a 26.04.1 arm64 build: the 26.04.1 server ISO built 2026-08-26,
subiquity 26.04.1 (snap revision 7405). Sources are the installer's own logs
in the image's `/var/log/installer/`.

1. **The base system is the ISO's.** Curtin lays down the
   `ubuntu-server-minimal` squashfs layers from the ISO, so every package in
   them starts at the version the point release shipped.
2. **The ISO pool and the snapshot are offered together.** Subiquity builds an
   apt overlay holding the ISO's pool (`file:/cdrom resolute main restricted`)
   and the configured mirror, and runs its mirror check against both. It
   fetched the ISO's `Release` and the snapshot's `resolute`,
   `resolute-updates` and `resolute-backports` `InRelease`, and logged
   "Mirror checking successful". With the legacy `primary` form, the snapshot
   is the only candidate it checks.
3. **Curtin installs from whichever carries the higher version.** No pin
   favours either. The bootloader packages came from the ISO pool
   (`grub-efi-arm64*`, `grub2-common`, `shim-signed`, `efibootmgr`,
   `libefiboot1t64`, `os-prober`). The kernel came from the snapshot
   (`linux-generic`, `7.0.0-43`, newer than the ISO's).
4. **`updates` runs last.** Subiquity runs unattended-upgrades in the target
   ("downloading and installing all updates" with `all`), from the snapshot.
5. **The target keeps only the snapshot.** The ISO source is removed when the
   install finishes. The installed system's `ubuntu.sources` is the build's
   template, rendered: `https://snapshot.ubuntu.com/ubuntu/<ID>` for the
   release, `-updates` and `-backports` suites, and again for `-security`.
   Beside it is the local repository's `nke-local.list`.
   Curtin also leaves `ubuntu.sources.curtin.orig`, its backup of the
   ISO's default sources, which names the live archive. apt ignores it, since
   only `.list` and `.sources` files are sources.

## Does the installed system end up on snapshot versions?

**Not with `updates: security`.** The base packages from step 1 stay at the
point release's versions, and the snapshot no longer indexes any version
`-updates` has since replaced. A build with `updates: security` failed the
version check on 14 packages:

```text
not in snapshot: base-files:arm64=14ubuntu6.2
not in snapshot: fwupd:arm64=2.1.1-1ubuntu3.1
not in snapshot: libfwupd3:arm64=2.1.1-1ubuntu3.1
not in snapshot: motd-news-config:all=14ubuntu6.2
not in snapshot: python3-distupgrade:all=1:26.04.23
not in snapshot: python3-software-properties:all=0.120.1
not in snapshot: snapd:arm64=2.76.3+ubuntu26.04
not in snapshot: software-properties-common:all=0.120.1
not in snapshot: ubuntu-kernel-accessories:arm64=1.570.3
not in snapshot: ubuntu-minimal:arm64=1.570.3
not in snapshot: ubuntu-release-upgrader-core:all=1:26.04.23
not in snapshot: ubuntu-server:arm64=1.570.3
not in snapshot: ubuntu-server-minimal:arm64=1.570.3
not in snapshot: ubuntu-standard:arm64=1.570.3
```

**Yes with `updates: all`.** The same commit with `updates: all` passed:
every one of 731 installed packages is at a version the snapshot or the local
repository serves.

**What `updates: all` costs.** On the Apple-silicon HVF development path,
the unattended-upgrades step took about 3 minutes (21:57:17 to 22:00:15), and
the whole bake took 21m51s with `all` against 18m26s with `security`.

## Two builds, one package set

Two 26.04.1 builds of the same commit, 20 minutes apart, printed the same
line:

```text
package set sha256: 6846c64d319fb1e1eb16df28b3d0e5adc07e85cc92691dcb5a159ac9f44d53cf (731 packages)
```

An earlier pair, before the template change below, also matched each other.

The digest covers every installed package's name, architecture and version,
including the image's own packages. Those are versioned from the commit, so
two different commits never share a digest.

## 24.04: the live archive survived

The first 24.04.5 arm64 build failed the sources check:

```text
not the snapshot or the local repository: http://ports.ubuntu.com/ubuntu-ports/
```

This was not the check misfiring. The installer itself had installed from the
live archive: curtin's log shows `libc6`, the kernel and `linux-firmware`
fetched from `ports.ubuntu.com`. The installer's own records show where the
mirror went missing:

- **Subiquity did its part.** Subiquity 24.04.5 (snap revision 7426) elected
  the snapshot, and the apt configuration it handed curtin
  (`/var/log/installer/curtin-install/subiquity-curtin-apt.conf`) names the
  snapshot for `primary` and `security`.
- **Curtin dropped it.** Without a `sources_list` template, curtin builds the
  new sources from the image's existing `ubuntu.sources`, replacing each URI
  that exactly matches its built-in default mirror for the architecture. That
  is curtin at commit `8c8ca80d`, the one the 24.04 subiquity snap pins. For
  arm64 the default is `http://ports.ubuntu.com/ubuntu-ports`, but the 24.04.5
  ISO's file says `http://ports.ubuntu.com/ubuntu-ports/`. The trailing slash
  defeats the match, so the live URI passed through to the installer's apt
  and to the installed system.
- **cloud-init did not change it.** Curtin writes
  `curtin-preserve-sources.cfg`, and cloud-init's apt module kept the sources
  as they were.

26.04 was not affected, because its installer's curtin is newer. The build now
passes curtin an explicit deb822 template whose URIs are curtin's `$PRIMARY`
and `$SECURITY` placeholders. Those are rendered on every path, whatever the
image's file says.

With the template, the 24.04.5 arm64 build passes both checks. The ISO is the
24.04.5 server ISO built 2026-09-08. Its `ubuntu.sources` names only the
snapshot, curtin's install log has no `ports.ubuntu.com` fetch, and every
one of 709 installed packages is at a snapshot version:

```text
package set sha256: c1ad8abca3769d8ee7b7d040adfa71832b6f8672116139eac8fbba837693a74b (709 packages)
```

amd64 was not built locally. The template does not depend on what the ISO's
file says, so it applies to amd64 the same way; the pull request's CI builds
both architectures and both series.

## When the snapshot is unreachable

With the base URL pointed at a path the service does not serve, the
installer's mirror check failed and it stopped: "aborting install since no
mirror is usable". It did not fall back to installing from the ISO alone.
Packer only sees that the installer never comes up, and fails with "Timeout
waiting for SSH" once `ssh_timeout` passes (30 minutes under KVM or HVF). The
reason is in the installer's serial log, `build-logs/non-fde-install-console.log`.

## What nodes carry

The image keeps the snapshot as its Ubuntu source. Nodes do not run apt in the
background (the apt timers are masked), but `nvke-agent` runs `apt-get update`
when it adds a third-party repository or upgrades itself, and Ubuntu
dependencies then resolve from the image's snapshot. So:

- a node needs outbound access to the snapshot host, not to
  `archive.ubuntu.com`;
- it resolves only the versions that snapshot carries;
- Canonical keeps snapshots for at least two years, after which the source
  stops resolving.

## Moving to a new snapshot

The ID moves by hand until the release lock takes it over:

```bash
make -C os-image/packer ubuntu-snapshot-check                       # proposes the current hour
make -C os-image/packer ubuntu-snapshot-check UBUNTU_SNAPSHOT=<ID>  # checks a chosen ID
```

The target checks that the ID serves `InRelease` for `noble` and `resolute`
(release, `-updates` and `-security`) and the `-updates` `binary-amd64` and
`binary-arm64` indexes. Commit the passing ID as `ubuntu_snapshot`'s default.
Until it moves, every bake ships the same Ubuntu packages, security fixes
included.
