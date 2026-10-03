#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

usage() {
  echo "usage: $0 SOURCE_ISO OUTPUT_ISO" >&2
  exit 2
}

[[ $# -eq 2 ]] || usage
readonly source_iso="$1"
readonly output_iso="$2"
output_root="$(dirname "${output_iso}")"
readonly output_root

for tool in 7z grep mv sed xorriso; do
  command -v "${tool}" >/dev/null 2>&1 || {
    echo "patch-installer-iso: ${tool} is required" >&2
    exit 1
  }
done

if [[ ! -f "${source_iso}" || -L "${source_iso}" ]]; then
  echo "patch-installer-iso: source is not one regular file: ${source_iso}" >&2
  exit 1
fi
mkdir -p "${output_root}"
if [[ ! -d "${output_root}" || -L "${output_root}" ]]; then
  echo "patch-installer-iso: output root is unsafe: ${output_root}" >&2
  exit 1
fi
if [[ -L "${output_iso}" || ( -e "${output_iso}" && ! -f "${output_iso}" ) ]]; then
  echo "patch-installer-iso: refusing unsafe output: ${output_iso}" >&2
  exit 1
fi

work="$(mktemp -d "${output_root}/.installer.XXXXXX")"
cleanup() {
  rm -rf -- "${work}"
}
trap cleanup EXIT HUP INT TERM

7z e -y -o"${work}" "${source_iso}" boot/grub/grub.cfg >/dev/null
readonly grub_cfg="${work}/grub.cfg"
if [[ ! -f "${grub_cfg}" || -L "${grub_cfg}" ]]; then
  echo "patch-installer-iso: ISO has no regular boot/grub/grub.cfg" >&2
  exit 1
fi
timeout_count="$(grep -Ec '^set timeout=' "${grub_cfg}" || true)"
if [[ "${timeout_count}" != 1 ]]; then
  echo "patch-installer-iso: expected one GRUB timeout, found ${timeout_count}" >&2
  exit 1
fi
sed 's/^set timeout=.*/set timeout=300/' "${grub_cfg}" >"${work}/grub.cfg.patched"
if [[ "$(grep -Ec '^set timeout=300$' "${work}/grub.cfg.patched" || true)" != 1 ]]; then
  echo "patch-installer-iso: GRUB timeout transform failed" >&2
  exit 1
fi

xorriso -indev "${source_iso}" -outdev "${work}/installer.iso" \
  -boot_image any replay \
  -update "${work}/grub.cfg.patched" /boot/grub/grub.cfg \
  -- -commit >/dev/null
if [[ ! -s "${work}/installer.iso" || -L "${work}/installer.iso" ]]; then
  echo "patch-installer-iso: xorriso produced no regular ISO" >&2
  exit 1
fi
mv -f -- "${work}/installer.iso" "${output_iso}"
