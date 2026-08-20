#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

HELM_REAL_BIN="${CAPD_E2E_HELM_REAL_BIN:-/usr/local/bin/helm-real}"
MIRROR_HOST="${CAPD_E2E_DOCKERHUB_MIRROR:-}"

if [[ -n "${MIRROR_HOST}" && ! "${MIRROR_HOST}" =~ ^[a-zA-Z0-9.-]+(:[0-9]{1,5})?$ ]]; then
  printf 'invalid CAPD_E2E_DOCKERHUB_MIRROR: %s\n' "${MIRROR_HOST}" >&2
  exit 1
fi

args=()
for arg in "$@"; do
  if [[ -n "${MIRROR_HOST}" && "${arg}" == oci://registry-1.docker.io/* ]]; then
    arg="oci://${MIRROR_HOST}/${arg#oci://registry-1.docker.io/}"
  fi
  args+=("${arg}")
done

exec "${HELM_REAL_BIN}" "${args[@]}"
