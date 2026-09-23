#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

MATERIAL_PATH="${MATERIAL_PATH:?MATERIAL_PATH is required}"
EXPECTED_DIGEST="${EXPECTED_DIGEST:?EXPECTED_DIGEST is required}"

if [[ ! -f "${MATERIAL_PATH}" || -L "${MATERIAL_PATH}" ]]; then
  echo "verify-material: material is not a regular file: ${MATERIAL_PATH}" >&2
  exit 1
fi

if [[ ! "${EXPECTED_DIGEST}" =~ ^sha256:[0-9a-f]{64}$ ]]; then
  echo "verify-material: expected digest is not a canonical sha256 digest" >&2
  exit 1
fi

actual="$(sha256sum -- "${MATERIAL_PATH}")"
actual="sha256:${actual%% *}"
if [[ "${actual}" != "${EXPECTED_DIGEST}" ]]; then
  echo "verify-material: digest mismatch for ${MATERIAL_PATH}" >&2
  exit 1
fi
