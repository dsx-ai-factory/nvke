#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Sourced by nico-vms-e2e.sh. This local input establishes exact bytes and
# QCOW2 consistency only; publication authentication belongs to its caller.
nico_vms_local_os_image() {
  local image="${NKE_E2E_OS_IMAGE_FILE:-}" pin="${NKE_E2E_OS_IMAGE_SHA256:-}" actual
  [[ -z "${NKE_E2E_OS_IMAGE_TAG:-}" ]] || fail "local OS image and OS image tag are mutually exclusive"
  [[ "${image}" == /* && -f "${image}" && ! -L "${image}" && -s "${image}" ]] || fail "local OS image must be an absolute nonempty regular non-symlink file"
  # The existing iPXE command contains an unquoted image_url token. AWK's
  # -v binding also interprets backslash escapes when repointing that command.
  case "${image}" in
    *[[:space:]]*|*\\*|*'$'*) fail "local OS image path is not a literal iPXE path" ;;
  esac
  [[ "${pin}" =~ ^[0-9a-f]{64}$ ]] || fail "local OS image requires its exact 64-character lowercase SHA256"
  actual="$(timeout --foreground --kill-after=5s 300 sha256sum <"${image}")" || fail "cannot hash local OS image"
  [[ "${actual%% *}" == "${pin}" ]] || fail "local OS image differs from its SHA256 pin"
  timeout --foreground --kill-after=5s 300 qemu-img check -f qcow2 "${image}" >&2 || fail "local OS image failed QCOW2 consistency validation"
  printf '%s\n' "${image}"
}
