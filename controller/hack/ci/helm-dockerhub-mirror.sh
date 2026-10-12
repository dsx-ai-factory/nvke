#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

HELM_REAL_BIN="${NKE_E2E_HELM_REAL_BIN:-/usr/local/bin/helm-real}"
MIRROR_HOST="${NKE_E2E_DOCKERHUB_MIRROR:-}"

if [[ -n "${MIRROR_HOST}" && ! "${MIRROR_HOST}" =~ ^[a-zA-Z0-9.-]+(:[0-9]{1,5})?$ ]]; then
  printf 'invalid NKE_E2E_DOCKERHUB_MIRROR: %s\n' "${MIRROR_HOST}" >&2
  exit 1
fi

args=()
mirrored=false
for arg in "$@"; do
  if [[ -n "${MIRROR_HOST}" && "${arg}" == oci://registry-1.docker.io/* ]]; then
    arg="oci://${MIRROR_HOST}/${arg#oci://registry-1.docker.io/}"
    mirrored=true
  fi
  args+=("${arg}")
done

if [[ "${mirrored}" != true ]]; then
  exec "${HELM_REAL_BIN}" "${args[@]}"
fi

# The mirror sometimes 404s a chart blob it has not cached yet; a second fetch gets it.
err=$(mktemp)
trap 'rm -f "${err}"' EXIT
for attempt in 1 2; do
  rc=0
  "${HELM_REAL_BIN}" "${args[@]}" 2>"${err}" || rc=$?
  cat "${err}" >&2
  if ((rc == 0 || attempt == 2)) || ! grep -q 'failed to perform "Fetch"' "${err}"; then
    exit "${rc}"
  fi
  printf 'helm: chart fetch from %s failed; retrying in 15s\n' "${MIRROR_HOST}" >&2
  sleep 15
done
