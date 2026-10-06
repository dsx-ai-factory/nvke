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
readonly python="${PYTHON:-python3}"

for tool in 7z grep sed xorriso "${python}"; do
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
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

# Snapshot through one no-follow descriptor so both ISO tools read the same
# runner-owned bytes. The runner authenticates the input before this call.
"${python}" -I - "${source_iso}" "${work}/source.iso" <<'PY'
import os
import shutil
import stat
import sys

fd = os.open(sys.argv[1], os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
with os.fdopen(fd, "rb") as source:
    if not stat.S_ISREG(os.fstat(source.fileno()).st_mode):
        raise SystemExit("patch-installer-iso: source is not one regular file")
    with open(sys.argv[2], "xb") as destination:
        shutil.copyfileobj(source, destination, length=1 << 20)
PY

7z e -y -o"${work}" "${work}/source.iso" boot/grub/grub.cfg >/dev/null
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

xorriso -indev "${work}/source.iso" -outdev "${work}/installer.iso" \
  -boot_image any replay \
  -update "${work}/grub.cfg.patched" /boot/grub/grub.cfg \
  -- -commit >/dev/null
if [[ ! -s "${work}/installer.iso" || -L "${work}/installer.iso" ]]; then
  echo "patch-installer-iso: xorriso produced no regular ISO" >&2
  exit 1
fi
# os.replace rejects a directory swapped in at the final destination and is
# atomic on this filesystem, unlike mv's implicit move-into-directory mode.
"${python}" -I - "${work}/installer.iso" "${output_iso}" <<'PY'
import os
import sys

os.replace(sys.argv[1], sys.argv[2])
PY
