#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Ensure that supermin can read a runner kernel. The optional paths let tests
# use temporary directories without changing the host's /boot or os-release.
ensure_appliance_kernel() {
  local boot_dir="${1:-/boot}" os_release="${2:-/etc/os-release}"
  local pass vmlinuz found readable ID="" VERSION_ID=""
  for pass in 0 1; do
    found=""
    readable=""
    for vmlinuz in "${boot_dir}"/vmlinuz-*; do
      [[ -f "${vmlinuz}" ]] || continue
      found="${vmlinuz}"
      if [[ ! -r "${vmlinuz}" ]] && ! sudo chmod 0644 "${vmlinuz}"; then
        echo "::warning::cannot make kernel readable: ${vmlinuz}" >&2
        continue
      fi
      [[ ! -r "${vmlinuz}" ]] || readable="${vmlinuz}"
    done
    [[ -z "${readable}" ]] || return 0
    if [[ -n "${found}" ]]; then
      echo "::error::no readable kernel in ${boot_dir}" >&2
      return 1
    fi
    if [[ "${pass}" -eq 1 ]]; then
      echo "::error::kernel installation left no readable kernel in ${boot_dir}" >&2
      return 1
    fi

    echo "No kernel in ${boot_dir} for the libguestfs appliance; installing one..."
    # The runner never boots this kernel. Keep APT errors visible and omit
    # recommended bootloader packages that the appliance does not need.
    if sudo apt-get install -y --no-install-recommends linux-image-virtual; then
      continue
    fi

    # Use the runner release, not the release of the image being inspected.
    # shellcheck disable=SC1090 # The runner supplies its own os-release file.
    source "${os_release}" || return 1
    if [[ "${ID}" != ubuntu || ! "${VERSION_ID}" =~ ^[0-9]+\.[0-9]+$ ]]; then
      echo "::error::no Ubuntu HWE kernel fallback for this runner" >&2
      return 1
    fi
    echo "Default kernel installation failed; trying linux-image-virtual-hwe-${VERSION_ID}..."
    sudo apt-get install -y --no-install-recommends "linux-image-virtual-hwe-${VERSION_ID}" || return 1
  done
}
