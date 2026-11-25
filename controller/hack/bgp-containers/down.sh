#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Stop the FRR BGP speaker.
#
# FRR's zebra retracts its installed routes during graceful shutdown. If FRR was
# already hard-killed, inspect the host routing table manually instead of having
# this helper guess which BGP routes are safe to delete.

set -euo pipefail
CONTAINER_NAME="${CONTAINER_NAME:-bgp-containers}"
NETWORK_NAME="${NETWORK_NAME:-kind}"
# shellcheck source=../container-engine/lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../container-engine/lib.sh"
ce_init || exit 1
ENGINE="$(ce_cli)"

info() { echo "[bgp-containers:down] $*"; }

ce_remove_same_bridge_forwarding "${NETWORK_NAME}"

if "${ENGINE}" ps -a --format '{{.Names}}' | grep -qx "${CONTAINER_NAME}"; then
  # Try graceful stop first so zebra has a chance to retract its routes.
  info "Stopping ${CONTAINER_NAME} (SIGTERM, 10s grace)..."
  "${ENGINE}" stop --time 10 "${CONTAINER_NAME}" >/dev/null || true
  "${ENGINE}" rm -f "${CONTAINER_NAME}" >/dev/null || true
  info "Removed ${CONTAINER_NAME}."
else
  info "${CONTAINER_NAME} not present."
fi
