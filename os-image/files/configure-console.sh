#!/bin/bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Point the kernel console at a serial device, as well as the VGA console.
# Installed by the nvke-node-boot package as /usr/lib/nvke/configure-console.sh.
#
#   configure-console.sh <tty>      for example ttyS0, ttyS1 or ttyAMA0
#
# It writes /etc/default/grub.d/50-nvke-console.cfg and runs update-grub. Without
# a console= parameter an installer-based image sends boot and kernel messages,
# including any LUKS prompt, only to the VGA framebuffer, so -nographic tooling,
# `virsh console` and a BMC's serial-over-LAN never see them. The last console=
# on the command line owns /dev/console, so the serial one is listed last and
# the getty follows it.
#
# The device is a parameter, not part of the package, because it differs by
# architecture (ttyS0 on amd64, ttyAMA0 on arm64) and by hardware (CSC
# control-plane nodes use ttyS1). The image build chooses it.
#
# Environment (for tests; the defaults are the real paths and command):
#   NVKE_GRUB_DROPIN_DIR  directory holding the drop-in (/etc/default/grub.d)
#   NVKE_UPDATE_GRUB      command that regenerates grub.cfg (update-grub)
set -euo pipefail

dropin_dir="${NVKE_GRUB_DROPIN_DIR:-/etc/default/grub.d}"
update_grub="${NVKE_UPDATE_GRUB:-update-grub}"
dropin="${dropin_dir}/50-nvke-console.cfg"
# What the autoinstall late-command wrote before this package existed.
old_dropin="${dropin_dir}/99-console.cfg"

if [[ $# -ne 1 ]]; then
  echo "usage: $(basename "$0") <tty>   for example ttyS0" >&2
  exit 2
fi
tty="$1"

# The value goes into a file grub sources as shell, so it must be a bare device
# name. This is the check that keeps `ttyS0"; rm -rf /; "` out of grub's
# configuration, and it is the same pattern Packer applies to serial_console.
if [[ ! "${tty}" =~ ^tty[A-Za-z0-9]+$ ]]; then
  echo "configure-console: '${tty}' is not a tty device name such as ttyS0, ttyS1 or ttyAMA0" >&2
  exit 2
fi

install -d -m 0755 "${dropin_dir}"

# Retire the file the late-command wrote, but only if it is that file. It sets
# the same variable, and 99 sorts after 50, so leaving it would override this
# one with the old device. An edited copy is the operator's, so it stays.
if [[ -e "${old_dropin}" ]]; then
  if grep -Eqx 'GRUB_CMDLINE_LINUX="console=tty0 console=tty[A-Za-z0-9]+,115200"' "${old_dropin}" &&
    [[ "$(wc -l <"${old_dropin}")" -eq 1 ]]; then
    rm -f "${old_dropin}"
  else
    echo "configure-console: ${old_dropin} was edited, so it is left in place." >&2
    echo "  It sorts after 50-nvke-console.cfg and its GRUB_CMDLINE_LINUX wins; move your changes to a file numbered below 50 or remove it." >&2
  fi
fi

# Write to a temporary name and rename, so an interrupted run never leaves a
# half-written file for update-grub to source.
tmp="$(mktemp "${dropin_dir}/.50-nvke-console.XXXXXX")"
trap 'rm -f "${tmp}"' EXIT
cat >"${tmp}" <<CFG
# Installed by the nvke-node-boot package's configure-console.sh. See os-image/SPEC.md.
# Send kernel and boot output to the serial console as well as the VGA console.
# The last console= owns /dev/console, so the serial device is listed last.
GRUB_CMDLINE_LINUX="console=tty0 console=${tty},115200"
CFG
chmod 0644 "${tmp}"
mv -f "${tmp}" "${dropin}"
trap - EXIT

"${update_grub}"
