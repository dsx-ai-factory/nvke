#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Stop lab Keycloak and undo Kind /etc/hosts plus CoreDNS wiring.

set -euo pipefail

export PATH="/opt/homebrew/bin:/usr/local/bin:${PATH}"

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-controller-capi}"
KUBE_CONTEXT="${KUBE_CONTEXT:-kind-${KIND_CLUSTER_NAME}}"
HOSTNAME_NAME="${KEYCLOAK_HOSTNAME:-keycloak.nke.test}"
COREFILE_BEGIN="# nke-keycloak-begin"
COREFILE_END="# nke-keycloak-end"

info() { echo "[keycloak:down] $*"; }

"${HERE}/lib.sh" remove-kind-hosts "${HOSTNAME_NAME}" || true
if command -v kubectl >/dev/null 2>&1; then
  "${HERE}/lib.sh" unpatch-coredns "${KUBE_CONTEXT}" "${COREFILE_BEGIN}" "${COREFILE_END}" || true
fi

CONTAINER_NAME="${CONTAINER_NAME:-nke-keycloak}"
if docker ps -a --format '{{.Names}}' | grep -qx "${CONTAINER_NAME}"; then
  info "Stopping ${CONTAINER_NAME}..."
  docker stop --time 20 "${CONTAINER_NAME}" >/dev/null || true
  docker rm -f "${CONTAINER_NAME}" >/dev/null || true
  info "Removed ${CONTAINER_NAME}."
else
  info "${CONTAINER_NAME} not present."
fi
info "Leave the ${HOSTNAME_NAME} line in the laptop /etc/hosts; it is harmless."
