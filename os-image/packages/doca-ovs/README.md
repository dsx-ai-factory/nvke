<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# doca-ovs

Fetches NVIDIA's DOCA build of Open vSwitch, `doca-openvswitch-switch`, which
the node image runs in place of Ubuntu's `openvswitch-switch`. ovn-kubernetes
runs against the host's OVS (`values-nke.yaml`: `ovs-node: false`), and the
image runs it in kernel mode, which needs no OS configuration beyond the
package.

Unlike the other directories here, nothing is built. The debs come from
NVIDIA's public DOCA apt repository,
`https://linux.mellanox.com/public/repo/doca/<version>/ubuntu<series>/<arch>/`,
which publishes no source packages. `make debs` copies them into the image's
local repository, and `provision.sh` installs `doca-openvswitch-switch` at bake
time. Nodes never contact the DOCA repository.

`doca-openvswitch-switch` declares `Provides`, `Conflicts` and `Replaces:
openvswitch-switch` and ships the same systemd units. Two differences from
Ubuntu's package are handled in `provision.sh`: its postinst does not enable
`openvswitch-switch.service`, and it adds `ovs-exporter.service`, an
unauthenticated metrics endpoint on TCP 6103, which the image masks.
Installing OVS on a live node disrupts its networking, so a new
DOCA version reaches nodes only through a new image.

## What is pinned

`PACKAGES` in [`fetch.sh`](fetch.sh) lists `doca-openvswitch-switch` and every
dependency the DOCA repository serves itself. The rest of its dependencies come
from the Ubuntu archive at bake time. For DOCA 3.5.0:

| Package | Why |
|---|---|
| `doca-openvswitch-switch`, `doca-openvswitch-common`, `python3-doca-openvswitch` | Open vSwitch itself |
| `collectx-clxapi`, `libibmad5`, `libibumad3` | Telemetry library OVS links, and its dependencies |
| `libibverbs1`, `ibverbs-providers` | DOCA's rdma-core, which replaces Ubuntu's |

Not `doca-networking` or `doca-ofed`: those pull in DOCA's kernel modules,
which replace the inbox mlx5 drivers. Kernel-mode OVS needs only the userspace
packages above, and the nodes depend on the inbox drivers: on GB200 the
BlueField-3 runs on them, and swapping them out from under a node has already
cost it all host networking.

DOCA builds each Ubuntu release separately, so the lock has one row per
package, release (24.04 and 26.04) and architecture. Both releases pin the same
DOCA version, so every image runs the same OVS.

## Building

```bash
make debs OUT_DIR=/absolute/path UBUNTU_SERIES=26.04            # amd64
make debs OUT_DIR=/absolute/path UBUNTU_SERIES=24.04 ARCH=arm64
```

`debs` verifies every download against `lock/<version>.lock` and reindexes
`OUT_DIR` with `dpkg-scanpackages`. `fetch-artifacts.sh` runs it with the
image's release.

## Bumping DOCA

Set `DOCA_VERSION` in the [`Makefile`](Makefile), then on a Linux host with
`gpg` and `gpgv`:

```bash
make lock
git diff -- lock
```

`make lock` checks each release's `InRelease` against the committed DOCA signing
key, [`nvidia-doca-host.asc`](nvidia-doca-host.asc), checks `Packages` against
the digest `InRelease` gives, and takes each deb's SHA-256 from that index. It
fails if a package in `PACKAGES` is missing or listed twice.

It does not resolve dependencies. Before committing, check that `PACKAGES` is
still the set the DOCA repository has to supply: the set changed between DOCA
3.2.3 and 3.5.0. A missing one fails the image bake, when apt cannot find it in
the Ubuntu archive.
