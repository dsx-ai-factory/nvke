#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

# nke-agent.service runs this before every start. An agent told to skip mTLS
# never opens a workload socket, so waiting for one keeps it from starting at
# all -- the flag alone would leave the unit failing after this times out.
if [[ "${AGENT_ARGS:-}" == *--disable-mtls* ]]; then
  echo "nke-agent: --disable-mtls set, not waiting for a SPIFFE socket"
  exit 0
fi

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
