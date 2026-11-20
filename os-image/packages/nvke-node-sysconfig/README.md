<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# nvke-node-sysconfig

System configuration for an NVKE node, as a Debian package. `provision.sh`
used to install these files and run these commands one by one at bake time. In
a package they are versioned, tested, and the same on a new image and after an
in-place update.

The package is architecture-independent (`Architecture: all`). It downloads
nothing: every file comes from [`os-image/files/`](../../files/) or is declared
in [`packaging/nfpm.yaml`](packaging/nfpm.yaml), which also carries the reason
for each mask.

## What it holds

### conntrack

| Path | What it does |
|---|---|
| `/usr/lib/nvke/configure-conntrack.sh` | Computes the conntrack limits from the booting node's RAM. |
| `/lib/systemd/system/conntrack-tune.service` | Runs the script at every boot, before `systemd-sysctl.service`. Enabled by postinst. |
| `/usr/lib/sysctl.d/50-nvke-conntrack-timeouts.conf` | Sets TIME_WAIT to 60s. |
| `/usr/lib/modules-load.d/50-nvke-conntrack.conf` | Loads `nf_conntrack` early enough for both of the above to apply. |

The reasons are in the files themselves and in
[SPEC.md](../../SPEC.md#conntrack-limits): why the limits are generated at boot
and never baked, why the generated file lives in `/run`, and what the shorter
TIME_WAIT costs.

### etcd data directory

| Path | What it does |
|---|---|
| `/usr/lib/sysusers.d/50-nvke-etcd.conf` | Declares the `etcd` system user, with no login. |
| `/usr/lib/tmpfiles.d/50-nvke-etcd.conf` | Declares `/var/lib/etcd` as `etcd:etcd`, mode `0700`. |

CIS 1.1.12 checks the owner of the etcd data directory. kubeadm creates it as
`root:root` and has no setting for that, but it reuses a directory that already
exists, so the image has one in place before any control-plane node is
provisioned. etcd itself still runs as root in a static pod, so this changes what
the node reports, not what etcd can do.

postinst runs both files, and `systemd-tmpfiles` runs the second at every boot. It
creates a missing directory and corrects the owner and mode of an existing one,
and never touches what is inside, so a node that got a `root:root` directory
from an earlier `kubeadm init` ends up the same as a new image. The directory is
not a packaged entry: its owner does not exist when dpkg unpacks.

### Masked units

A link to `/dev/null` in `/etc/systemd/system`, which is what `systemctl mask`
writes, for each of:

| Unit | Why |
|---|---|
| `multipathd.service`, `multipathd.socket` | With no blacklist, multipathd claims Longhorn's iSCSI volumes through device-mapper and `mkfs` then fails with EBUSY. Both units, and masked rather than disabled, because the socket re-activates the service. |
| `apt-daily.timer`, `apt-daily-upgrade.timer`, `apt-daily.service`, `apt-daily-upgrade.service`, `unattended-upgrades.service` | Their first run after boot restarts journald, networkd and udevd, which breaks kubeadm's `daemon-reload` in the kubelet-start phase. They also hold the dpkg lock against cloud-init. Nodes take updates through a new image. |
| `ovs-exporter.service` | DOCA's unauthenticated metrics endpoint on TCP 6103. Stock Open vSwitch has no such listener. |

A mask holds if the owning package is installed or reinstalled later, and works
before the unit exists. `preinst` stops and disables each unit first, because a
mask does not stop a running unit and, once in place, stops us disabling it.

Install order matters for `ovs-exporter.service`: DOCA's postinst enables it,
and a package that enables a masked unit fails to configure. `provision.sh`
installs this package after DOCA. A later in-place DOCA upgrade on a node that
already has the mask is exposed to the same thing; that was equally true of the
mask `provision.sh` wrote before this package.

### File numbering

Our drop-ins are numbered 50–59. `systemd-sysctl` reads `sysctl.d` files in
file-name order and the last value for a key wins, so the `50-` prefix sorts
after Ubuntu's `10-` to `49-` files and before the `60-` to `99-` files that
operators add. To change a value, put it in a later-sorting file such as
`/etc/sysctl.d/99-local.conf`.

The file `conntrack-tune.service` generates, `/run/sysctl.d/50-nvke-conntrack.conf`,
follows the same rule. It was `10-nke-conntrack.conf` before this package. An
operator file of that old name in `/etc/sysctl.d` no longer replaces it, and
sorts before it, so move such a value to a later-sorting file such as
`/etc/sysctl.d/99-local.conf`.

### Where files live

Regular files go in `/usr/lib` or `/lib`, not `/etc`. Nothing is a conffile, and
an operator's file of the same name in `/etc/sysctl.d`, `/etc/modules-load.d` or
`/etc/systemd/system` always replaces ours. The only things under `/etc` are the
mask links, which have to be there.

## Upgrading an image built before this package

`postinst` retires what `provision.sh` left:

- `/etc/systemd/system/conntrack-tune.service`, always. A unit under `/etc`
  would mask the vendor unit.
- `/etc/sysctl.d/10-nke-conntrack-timeouts.conf`, only while it still has the
  bytes `provision.sh` wrote. An edited copy stays, with a message, and the
  packaged `50-` file overrides it.
- `/etc/modules-load.d/nf_conntrack.conf`, while it holds only `nf_conntrack`.
- `/run/sysctl.d/10-nke-conntrack.conf`, the generated file under its old name.
  The unit writes the new name straight away.

The masks `provision.sh` wrote are the same links the package ships, so dpkg takes
them over. The existing `etcd` user and directory are left as they are.

Where systemd is running, `postinst` also applies the conntrack settings straight
away, so an in-place update does not wait for a reboot.

## Removing it

`dpkg -r` and `dpkg -P` take back the files, the unit, the masks and the generated
file. The `etcd` user and `/var/lib/etcd` stay: the directory holds a
control-plane node's data. The units that were masked are left disabled, not
re-enabled.

## Build and test

```bash
make nfpm                         # the pinned nfpm, into ./bin
make debs OUT_DIR=/absolute/path  # nvke-node-sysconfig_<version>_all.deb plus a flat apt index
make test                         # build the package and inspect it
sudo test/install.sh /absolute/path/to/nvke-node-sysconfig_<version>_all.deb
```

`make test` checks the package's metadata, every file's path, mode and bytes, that
the mask links are exactly the list above, and that the only thing under `/etc` is
those links.

`test/install.sh` needs root on a disposable systemd host. It starts from a node
that has been running (apt timers active, a `root:root` etcd directory holding
data, the files `provision.sh` left), then upgrades, reinstalls, repairs a drifted
directory, leaves an edited legacy file alone, removes, takes over the links
`systemctl mask` wrote, and purges. After each step it checks that dpkg owns every
file, the units are masked and stopped, the etcd directory is `etcd:etcd` `0700`,
and the etcd data was never touched.

`DEB_VERSION` in the Makefile is the package version. Bump it when the content
changes.
