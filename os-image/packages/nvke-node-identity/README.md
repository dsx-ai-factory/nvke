<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# nvke-node-identity

How `nvke-agent` waits for the node's SPIRE identity, as a Debian package.
`provision.sh` used to install these two files by hand at bake time. In a
package they are versioned, tested, and the same on a new image and after an
in-place update.

The package is architecture-independent (`Architecture: all`). It downloads
nothing: both files come from [`os-image/files/`](../../files/) and the layout
is in [`packaging/nfpm.yaml`](packaging/nfpm.yaml).

## What it holds

| Path | What it does |
|---|---|
| `/lib/systemd/system/nvke-agent.service.d/50-nvke-identity.conf` | Orders `nvke-agent.service` after `spire-agent.service`, wants it, runs the socket wait before each start, and touches `/var/lib/spire-agent/.nvke-attested` after a start. |
| `/usr/lib/nvke/wait-spiffe-socket.sh` | Waits up to 60 seconds for the SPIFFE workload API socket. Skips the wait when the agent is told `--disable-mtls`, because that agent never opens one. |

## Why a package of its own

- It is neither system configuration nor boot configuration: it exists so a node
  can get an identity. Keeping it out of
  `nvke-node-sysconfig` keeps SPIRE out of that package.
- It is not part of the `nvke-agent` package on purpose. The ordering is the
  image's choice, and an agent upgrade must not change it.
- The name is not SPIRE-specific, so other identity wiring can join it later.

## Where the files go

Both install under `/lib` and `/usr/lib`, never `/etc`, so nothing is a
conffile and there is no dpkg prompt on upgrade. systemd merges drop-ins from
`/lib/systemd/system` with those in `/etc/systemd/system`, even when the unit
itself lives in `/etc` as the agent's does. A same-named file in `/etc` replaces
the packaged one.

## Moving from `provision.sh`

`provision.sh` wrote the same ordering to
`/etc/systemd/system/nvke-agent.service.d/10-spire.conf`. Left beside the
packaged file it would add every line a second time (the wait would run twice).
postinst removes it only while it is the exact bytes `provision.sh` wrote,
checked by hash. An edited copy stays, with a message.

postinst and postrm run `systemctl daemon-reload` when systemd is running and
**never restart `nvke-agent`**. A running agent keeps the ordering it started
with.

## The drop-in's `After=`

The drop-in starts with an empty `After=`, with a comment saying it replaces the
unit's `After=multi-user.target`. On systemd 255 (Ubuntu 24.04) and 259 (26.04)
it does not: a reset does not clear dependencies, so `multi-user.target` stays in
the unit's ordering. The file is moved here unchanged and nothing depends on the
reset, but the comment is wrong.

## No Depends

The package does not depend on `nvke-agent` or `spire-agent`. The drop-in names
both units and is inert if either is missing, and a hard dependency would turn
the image's install order and an agent update into a question for dpkg.

## Version

`DEB_VERSION` in the [Makefile](Makefile) is set by hand. Bump it when the
content changes.

## Build and test

```bash
make -C os-image/packages/nvke-node-identity test
make -C os-image/packages/nvke-node-identity debs OUT_DIR=/absolute/path
sudo os-image/packages/nvke-node-identity/test/install.sh /absolute/path/nvke-node-identity_0.1.0_all.deb
```

`make test` builds the deb with the pinned nfpm and checks its metadata, that it
holds exactly the two files (path, mode, bytes) and nothing under `/etc`, and that
the drop-in keeps the lines it exists for. `test/install.sh` needs a disposable
systemd host with root. It stands in for the two agents and covers upgrade from
what `provision.sh` left, reinstall, an edited old drop-in, an operator's override
in `/etc`, remove and purge, against real systemd.
