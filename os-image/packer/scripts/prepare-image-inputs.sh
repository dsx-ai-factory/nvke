#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

mode="${MODE:?MODE is required}"
staging_directory="${STAGING_DIRECTORY:?STAGING_DIRECTORY is required}"

case "${staging_directory}" in
  /*/fde/.image-inputs|/*/non-fde/.image-inputs) ;;
  *)
    echo "refusing unsafe image-input staging path: ${staging_directory}" >&2
    exit 1
    ;;
esac

case "${mode}" in
  stage)
    material_path="${MATERIAL_PATH:?MATERIAL_PATH is required when MODE=stage}"
    expected_digest="${EXPECTED_DIGEST:?EXPECTED_DIGEST is required when MODE=stage}"

    if [[ -e "${staging_directory}" || -L "${staging_directory}" ]]; then
      echo "image-input staging path already exists: ${staging_directory}" >&2
      exit 1
    fi

    MATERIAL_PATH="${material_path}" EXPECTED_DIGEST="${expected_digest}" \
      "$(dirname "${BASH_SOURCE[0]}")/verify-material.sh"

    install -d -m 0700 "${staging_directory}"
    cleanup_failed_stage() {
      rm -rf -- "${staging_directory}"
    }
    trap cleanup_failed_stage ERR

    # Snapshot the caller-owned archive into private attempt state, then verify,
    # inspect, and extract only that copy. A concurrent replacement of the
    # caller path can therefore produce only a digest failure, not different
    # bytes between validation and host extraction.
    material_snapshot="$(mktemp "${staging_directory}/.material.XXXXXX")"
    install -m 0600 "${material_path}" "${material_snapshot}"
    MATERIAL_PATH="${material_snapshot}" EXPECTED_DIGEST="${expected_digest}" \
      "$(dirname "${BASH_SOURCE[0]}")/verify-material.sh"

    # The archive is authenticated by digest above, but its member types are a
    # separate host-safety boundary. Packer's SSH file provisioner follows
    # local symlinks, so only regular files and directories may reach the
    # transient upload tree.
    "${PYTHON:-python3}" - "${material_snapshot}" <<'PY'
import pathlib
import sys
import tarfile

with tarfile.open(sys.argv[1]) as archive:
    for member in archive.getmembers():
        path = pathlib.PurePosixPath(member.name)
        if path.is_absolute() or ".." in path.parts:
            raise SystemExit(f"unsafe archive path: {member.name}")
        if not (member.isfile() or member.isdir()):
            raise SystemExit(f"unsafe archive member type: {member.name}")
PY

    tar -xf "${material_snapshot}" -C "${staging_directory}"
    rm -f -- "${material_snapshot}"
    chmod 0700 "${staging_directory}"
    trap - ERR
    ;;
  clean)
    rm -rf -- "${staging_directory}"
    ;;
  *)
    echo "unsupported MODE=${mode} (expected stage or clean)" >&2
    exit 1
    ;;
esac
