#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# nkectl e2e entrypoint, invoked by `make test-e2e` and the "nkectl e2e" CI
# workflow. Grown incrementally, one stage per change, alongside
# `nkectl bootstrap site`'s real implementation — each stage below is added
# only once the code it exercises exists, so this script and the command it
# drives are never out of sync.
#
# Current stage: inventory load + validation only. `bootstrap site` stops at
# its not-yet-implemented dispatch stub, which this script asserts by name so
# a change that silently starts dispatching (or stops validating) is caught
# here rather than downstream.

set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLI_DIR="$(dirname "${SCRIPT_DIR}")"
NKECTL="${CLI_DIR}/bin/nkectl"
INVENTORY="${SCRIPT_DIR}/inventory.ci.yaml"

log() { printf '[nkectl-e2e] %s\n' "$*"; }

log "running: nkectl bootstrap site -i ${INVENTORY}"
set +e
output="$("${NKECTL}" bootstrap site -i "${INVENTORY}" 2>&1)"
status=$?
set -e
printf '%s\n' "${output}"

if [ "${status}" -eq 0 ]; then
  log "FAIL: expected the bootstrap-dispatch stub to error, but the command exited 0"
  exit 1
fi
if ! grep -q "not yet implemented" <<<"${output}"; then
  log "FAIL: command failed, but not with the expected dispatch stub error"
  exit 1
fi

log "PASS: inventory loaded and validated; reached the bootstrap-dispatch stub"
