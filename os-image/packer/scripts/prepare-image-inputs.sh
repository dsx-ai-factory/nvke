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
    tar -xf "${material_path}" -C "${staging_directory}"
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
