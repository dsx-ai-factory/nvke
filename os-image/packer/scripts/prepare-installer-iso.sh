#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Downloads the Ubuntu Server installer ISO (if not already cached) and
# patches its boot/grub/grub.cfg to raise GRUB's menu timeout from the
# stock 30s to 300s, producing a second, patched ISO for Packer to boot.
#
# Why: boot_command blind-types autoinstall's kernel params into GRUB's
# command line over VNC, a fixed boot_wait after VM start. That has to land
# inside the window between "GRUB is actually rendered" and "GRUB's own
# timeout expires and auto-boots the non-autoinstall default entry". Under
# tcg software emulation, how long BIOS POST takes before GRUB even renders
# is unpredictable enough that no boot_wait reliably fits inside a 30s
# window (confirmed in practice: 5s was too early, 60s was already past
# it). Patching the timeout to 300s doesn't remove the race, it just makes
# the window wide enough that any sane boot_wait lands inside it.
#
# This keeps booting through the ISO's own GRUB (and, after autoinstall's
# final reboot, the target disk's own installed GRUB) rather than a direct
# QEMU -kernel/-initrd boot: the latter re-executes the same kernel/initrd
# on every guest-triggered reboot for the life of the QEMU process, since
# those are fixed at QEMU invocation time -- which looped back into the
# live installer forever instead of ever reaching the newly-installed
# system (confirmed by reproducing it: subiquity's own services restarted
# a second time in the serial console log right after the post-install
# reboot).
#
# Usage: ISO_URL=... ISO_CHECKSUM=... OUT_DIR=... ./prepare-installer-iso.sh
#
# Outputs (idempotent -- skipped if already present and valid):
#   $OUT_DIR/<basename of ISO_URL>           the downloaded/cached original ISO
#   $OUT_DIR/<basename of ISO_URL>.patched.iso   grub.cfg timeout=300, same El
#                                                 Torito BIOS+UEFI boot catalog
#                                                 (this is what iso_url points at)

set -euo pipefail

ISO_URL="${ISO_URL:?ISO_URL is required}"
ISO_CHECKSUM="${ISO_CHECKSUM:?ISO_CHECKSUM is required}" # file:<url to a SHA256SUMS-style manifest>
OUT_DIR="${OUT_DIR:?OUT_DIR is required}"

command -v xorriso >/dev/null 2>&1 || { echo "prepare-installer-iso: xorriso is required" >&2; exit 1; }
command -v 7z >/dev/null 2>&1 || { echo "prepare-installer-iso: 7z (p7zip-full) is required" >&2; exit 1; }

mkdir -p "${OUT_DIR}"
ISO_NAME="$(basename "${ISO_URL}")"
ISO_PATH="${OUT_DIR}/${ISO_NAME}"
PATCHED_PATH="${ISO_PATH}.patched.iso"

CHECKSUM_URL="${ISO_CHECKSUM#file:}"
EXPECTED_SHA256="$(curl -fsSL "${CHECKSUM_URL}" | grep -E " \*?${ISO_NAME}\$" | awk '{print $1}' | head -1)"
if [[ -z "${EXPECTED_SHA256}" ]]; then
  echo "prepare-installer-iso: no checksum for ${ISO_NAME} found in ${CHECKSUM_URL}" >&2
  exit 1
fi

verify_iso() {
  [[ -f "${ISO_PATH}" ]] && echo "${EXPECTED_SHA256}  ${ISO_PATH}" | sha256sum -c - >/dev/null 2>&1
}

if verify_iso; then
  echo "prepare-installer-iso: ${ISO_PATH} already present and checksum-valid"
else
  echo "prepare-installer-iso: downloading ${ISO_URL}"
  curl -fL --retry 3 -o "${ISO_PATH}.tmp" "${ISO_URL}"
  mv "${ISO_PATH}.tmp" "${ISO_PATH}"
  verify_iso || { echo "prepare-installer-iso: checksum mismatch for ${ISO_PATH}" >&2; exit 1; }
fi

if [[ -s "${PATCHED_PATH}" && "${PATCHED_PATH}" -nt "${ISO_PATH}" ]]; then
  echo "prepare-installer-iso: ${PATCHED_PATH} already up to date"
else
  echo "prepare-installer-iso: patching GRUB timeout in ${ISO_NAME}"
  WORK="$(mktemp -d)"
  trap 'rm -rf "${WORK}"' EXIT
  7z e -y -o"${WORK}" "${ISO_PATH}" boot/grub/grub.cfg >/dev/null
  sed 's/^set timeout=.*/set timeout=300/' "${WORK}/grub.cfg" > "${WORK}/grub.cfg.patched"
  rm -f "${PATCHED_PATH}.tmp"
  xorriso -indev "${ISO_PATH}" -outdev "${PATCHED_PATH}.tmp" \
    -boot_image any replay \
    -update "${WORK}/grub.cfg.patched" /boot/grub/grub.cfg \
    -- -commit >/dev/null
  mv "${PATCHED_PATH}.tmp" "${PATCHED_PATH}"
fi

echo "prepare-installer-iso: iso=${ISO_PATH} patched=${PATCHED_PATH}"
