<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# nvke-node-boot

Boot configuration for an NVKE node, as a Debian package. Today that is one
tool: `configure-console.sh`, which points the kernel console at a serial
device. Before this package the autoinstall late-command wrote the same grub
drop-in inline, so nothing owned it and an in-place update could not change it.

The package is architecture-independent (`Architecture: all`). It downloads
nothing: the script comes from [`os-image/files/`](../../files/) and the layout
is in [`packaging/nfpm.yaml`](packaging/nfpm.yaml).

## What it holds

| Path | What it does |
|---|---|
| `/usr/lib/nvke/configure-console.sh` | `configure-console.sh <tty>` writes `/etc/default/grub.d/50-nvke-console.cfg` and runs `update-grub`. |

## Why the package carries the tool and not the setting

Without a `console=` parameter an installer-based image sends boot and kernel
messages, including any LUKS unlock prompt, only to the VGA framebuffer.
`-nographic` tooling, `virsh console` and a BMC's serial-over-LAN never see
them. The setting is `console=tty0 console=<tty>,115200`: the last `console=`
owns `/dev/console`, so the serial device goes last and the getty follows it.

The device is not the same everywhere: `ttyS0` on amd64, `ttyAMA0` on arm64, and
`ttyS1` on CSC control-plane hardware (`SERIAL_CONSOLE`). A packaged file could
hold only one value, and `update-grub` reads only `/etc/default/grub.d`, where a
packaged file would be a conffile with a dpkg prompt on upgrade. So the image
build chooses the device (`locals.pkr.hcl`) and passes it to the script from
`provision.sh`.

`configure-console.sh`:

- accepts only `^tty[A-Za-z0-9]+$`. The value ends up in a file grub sources as
  shell, so anything else is rejected before a file is written (exit 2);
- writes the drop-in under a temporary name and renames it, so an interrupted
  run leaves no half-written file;
- is safe to run again, and a later run with another device replaces the first;
- retires `/etc/default/grub.d/99-console.cfg` if it is exactly the line the
  autoinstall late-command wrote. It sets the same variable and sorts after 50,
  so it would keep the old device. An edited copy stays, with a message;
- fails if `update-grub` fails, so a node whose `grub.cfg` was not regenerated
  does not look configured.

## Left out on purpose

`/etc/default/grub.d/99-luks-key.cfg` carries the build-time LUKS passphrase and
stays in the FDE autoinstall late-commands. It is not configuration to
distribute, and `luks-tpm-firstboot.sh` removes it after the TPM bind.

## Numbering

The drop-in is `50-nvke-console.cfg`, in the 50-59 range for NVKE's own files.
An operator's `60-` or later file that sets `GRUB_CMDLINE_LINUX` wins.

## Removing the package

`dpkg -r` leaves the drop-in, as it would a conffile: removing the tool is not a
request to lose a node's serial console. `dpkg -P` removes the drop-in and runs
`update-grub`.

## Version

`DEB_VERSION` in the [Makefile](Makefile) is set by hand. Bump it when the
content changes.

## Build and test

```bash
make -C os-image/packages/nvke-node-boot test
make -C os-image/packages/nvke-node-boot debs OUT_DIR=/absolute/path
```

`make test` builds the deb with the pinned nfpm, checks its metadata and
contents, and runs `configure-console.sh` against a temporary directory and a
stand-in for `update-grub`: valid and rejected devices, repeat runs, the old
file, and an `update-grub` failure.
