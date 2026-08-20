#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

SOCKET="${SPIFFE_SOCKET_PATH:-/tmp/spire-agent/public/api.sock}"
SOCKET="${SOCKET#unix://}"

for _ in $(seq 1 60); do
  if [[ -S "${SOCKET}" ]]; then
    exit 0
  fi
  sleep 1
done

echo "nke-agent: timed out waiting for SPIFFE workload API socket ${SOCKET}" >&2
exit 1
