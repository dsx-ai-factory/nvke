#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# Stop the FRR BGP speaker.
#
# FRR's zebra retracts its installed routes during graceful shutdown. If FRR was
# already hard-killed, inspect the host routing table manually instead of having
# this helper guess which BGP routes are safe to delete.

set -euo pipefail
CONTAINER_NAME="${CONTAINER_NAME:-capd-tor}"

info() { echo "[capd-bgp:down] $*"; }

if docker ps -a --format '{{.Names}}' | grep -qx "${CONTAINER_NAME}"; then
  # Try graceful stop first so zebra has a chance to retract its routes.
  info "Stopping ${CONTAINER_NAME} (SIGTERM, 10s grace)..."
  docker stop --time 10 "${CONTAINER_NAME}" >/dev/null || true
  docker rm -f "${CONTAINER_NAME}" >/dev/null || true
  info "Removed ${CONTAINER_NAME}."
else
  info "${CONTAINER_NAME} not present."
fi
