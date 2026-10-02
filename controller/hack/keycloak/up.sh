#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Start lab Keycloak on localhost:8180 and, when a Kind cluster is present, on
# the kind Docker network under the hostname keycloak.nke.test.
#
# Usage:
#   ./up.sh           # start and wait until OIDC discovery answers
#   ./up.sh --verify  # also mint a password-grant token and probe Kind

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONTAINER_NAME="${CONTAINER_NAME:-nke-keycloak}"
KIND_NETWORK="${KIND_NETWORK:-kind}"
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-cwe-capi}"
KUBE_CONTEXT="${KUBE_CONTEXT:-kind-${KIND_CLUSTER_NAME}}"
HOSTNAME_NAME="${KEYCLOAK_HOSTNAME:-keycloak.nke.test}"
HOST_PORT="${KEYCLOAK_HOST_PORT:-8180}"
ISSUER="http://${HOSTNAME_NAME}:${HOST_PORT}/realms/nke"
COREFILE_BEGIN="# nke-keycloak-begin"
COREFILE_END="# nke-keycloak-end"

VERIFY=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --verify) VERIFY=1; shift ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

info() { echo "[keycloak:up] $*"; }
die() { echo "[keycloak:up] ERROR: $*" >&2; exit 1; }

export PATH="/opt/homebrew/bin:/usr/local/bin:${PATH}"
command -v docker >/dev/null 2>&1 || die "docker is required"
command -v python3 >/dev/null 2>&1 || die "python3 is required"

IMAGE="${KEYCLOAK_IMAGE:-quay.io/keycloak/keycloak:26.3.3}"

if docker ps -a --format '{{.Names}}' | grep -qx "${CONTAINER_NAME}"; then
  info "Replacing existing ${CONTAINER_NAME}."
  CONTAINER_NAME="${CONTAINER_NAME}" "${HERE}/down.sh"
fi

info "Starting ${CONTAINER_NAME} from ${IMAGE}..."
docker run -d \
  --name "${CONTAINER_NAME}" \
  --publish "${HOST_PORT}:${HOST_PORT}" \
  --volume "${HERE}/realm:/opt/keycloak/data/import:ro" \
  --env KC_BOOTSTRAP_ADMIN_USERNAME=admin \
  --env KC_BOOTSTRAP_ADMIN_PASSWORD=admin \
  --env KC_HTTP_ENABLED=true \
  --env KC_HTTP_HOST=0.0.0.0 \
  --env "KC_HTTP_PORT=${HOST_PORT}" \
  --env "KC_HOSTNAME=http://${HOSTNAME_NAME}:${HOST_PORT}" \
  --env "KC_HOSTNAME_ADMIN=http://localhost:${HOST_PORT}" \
  --env KC_HOSTNAME_STRICT=true \
  "${IMAGE}" \
  start-dev --import-realm >/dev/null

kind_ip=""
if docker network inspect "${KIND_NETWORK}" >/dev/null 2>&1; then
  connected="$(docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}' "${CONTAINER_NAME}")"
  if [[ " ${connected} " != *" ${KIND_NETWORK} "* ]]; then
    info "Attaching ${CONTAINER_NAME} to Docker network ${KIND_NETWORK}."
    docker network connect "${KIND_NETWORK}" "${CONTAINER_NAME}"
  fi
  kind_ip="$(docker inspect -f "{{(index .NetworkSettings.Networks \"${KIND_NETWORK}\").IPAddress}}" "${CONTAINER_NAME}")"
  [[ -n "${kind_ip}" ]] || die "Keycloak has no address on ${KIND_NETWORK}"
  info "Kind reachability: ${HOSTNAME_NAME} -> ${kind_ip}:${HOST_PORT}"
  "${HERE}/lib.sh" inject-kind-hosts "${HOSTNAME_NAME}" "${kind_ip}"
  if kubectl --context "${KUBE_CONTEXT}" get ns kube-system >/dev/null 2>&1; then
    "${HERE}/lib.sh" patch-coredns "${KUBE_CONTEXT}" "${HOSTNAME_NAME}" "${kind_ip}" "${COREFILE_BEGIN}" "${COREFILE_END}"
  else
    info "No kube-system in context ${KUBE_CONTEXT}; skip CoreDNS (Kind cluster not ready)."
  fi
else
  info "Docker network ${KIND_NETWORK} not found; serving localhost only. Re-run after Kind is up to wire the cluster."
fi

if python3 - "$HOSTNAME_NAME" "${HOST_PORT}" <<'PY'; then
import socket, sys
host, port = sys.argv[1], int(sys.argv[2])
try:
    infos = socket.getaddrinfo(host, port, type=socket.SOCK_STREAM)
except socket.gaierror:
    sys.exit(1)
addrs = {i[4][0] for i in infos}
sys.exit(0 if "127.0.0.1" in addrs or "::1" in addrs else 1)
PY
  info "Laptop resolver already maps ${HOSTNAME_NAME} to loopback."
else
  info "Add this line to /etc/hosts so the laptop uses the same issuer host as Kind:"
  info "  127.0.0.1 ${HOSTNAME_NAME}"
  info "  sudo sh -c 'grep -q ${HOSTNAME_NAME} /etc/hosts || echo \"127.0.0.1 ${HOSTNAME_NAME}\" >> /etc/hosts'"
fi

info "Waiting for ${ISSUER}/.well-known/openid-configuration ..."
"${HERE}/lib.sh" wait-discovery "http://127.0.0.1:${HOST_PORT}" "${HOSTNAME_NAME}" "${HOST_PORT}" "${ISSUER}"

info "Admin console: http://localhost:${HOST_PORT}  (admin / admin, lab only)"
info "Issuer:        ${ISSUER}"
info "CLI client:    nke-cli (public, PKCE, password grant enabled)"
info "Users:         nke-dev / nke-dev  (group nke-operators)"
info "Static client: nke-static / nke-static-lab-secret"

if [[ "${VERIFY}" -eq 1 ]]; then
  "${HERE}/lib.sh" verify-token "${HOSTNAME_NAME}" "${HOST_PORT}" "${ISSUER}"
  if [[ -n "${kind_ip}" ]] && kubectl --context "${KUBE_CONTEXT}" get ns kube-system >/dev/null 2>&1; then
    "${HERE}/lib.sh" verify-kind "${KUBE_CONTEXT}" "${HOSTNAME_NAME}" "${HOST_PORT}" "${ISSUER}"
  fi
fi
