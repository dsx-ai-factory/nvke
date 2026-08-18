#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Fetch a remote kind cluster's kubeconfig to the Mac, rewritten to point at a
# locally-forwarded port and given a target-specific context name. Optionally
# wires the matching Mutagen "kube-api" forward and restarts the project.
#
# Docs: docs/remote-colossus-tilt.md
#
# The remote kind API server binds 127.0.0.1:<random-port>; its TLS cert has a
# 127.0.0.1 SAN, so kubectl works over the forwarded tunnel regardless of port.
#
# Copy this script next to your personal Mutagen project dirs, e.g.:
#   ~/dev/remote-sync/fetch-kubeconfig.sh
#   ~/dev/remote-sync/colossus/mutagen.yml
#
# Usage:
#   ./fetch-kubeconfig.sh [--update-mutagen] <ssh-host> [kind-cluster] [local-api-port]
#
# Defaults: kind-cluster=cwe-capi, local-api-port=16443
#
# Examples:
#   ./fetch-kubeconfig.sh colossus
#   ./fetch-kubeconfig.sh --update-mutagen colossus
#     -> writes ~/.kube/<host>-kind-<cluster>.yaml, sets the kube-api forward in
#        <host>/mutagen.yml to the discovered remote port, restarts the project.

set -euo pipefail

UPDATE_MUTAGEN=false
ARGS=()
for a in "$@"; do
  case "$a" in
    --update-mutagen|-u) UPDATE_MUTAGEN=true ;;
    -h|--help) sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    -*) echo "unknown option: $a" >&2; exit 1 ;;
    *) ARGS+=("$a") ;;
  esac
done

HOST="${ARGS[0]:?usage: $0 [--update-mutagen] <ssh-host> [kind-cluster] [local-api-port]}"
CLUSTER="${ARGS[1]:-cwe-capi}"
LOCAL_PORT="${ARGS[2]:-16443}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="${SCRIPT_DIR}/${HOST}"
PROJECT_YAML="${PROJECT_DIR}/mutagen.yml"

CONTEXT="${HOST}-kind-${CLUSTER}"
OUT="${HOME}/.kube/${CONTEXT}.yaml"

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

echo "Fetching kubeconfig for kind cluster '${CLUSTER}' from ${HOST}..."
ssh "$HOST" "kind get kubeconfig --name ${CLUSTER}" > "$tmp"

remote_port="$(awk -F: '/server:/{print $NF}' "$tmp" | tr -d ' ')"
if [[ -z "${remote_port}" ]]; then
  echo "error: could not determine remote API server port from kubeconfig" >&2
  exit 1
fi

mkdir -p "${HOME}/.kube"
sed -E "s#https://(127\.0\.0\.1|0\.0\.0\.0|\[::1\]):[0-9]+#https://127.0.0.1:${LOCAL_PORT}#" "$tmp" \
  | sed "s/kind-${CLUSTER}/${CONTEXT}/g" \
  > "$OUT"

echo "Wrote ${OUT} (context: ${CONTEXT}, server: https://127.0.0.1:${LOCAL_PORT}, remote port: ${remote_port})"

if [[ "${UPDATE_MUTAGEN}" == true ]]; then
  if [[ ! -f "${PROJECT_YAML}" ]]; then
    echo "error: ${PROJECT_YAML} not found; cannot update Mutagen" >&2
    exit 1
  fi
  if ! command -v yq >/dev/null 2>&1; then
    echo "warn: yq not installed; cannot auto-edit ${PROJECT_YAML}." >&2
    echo "      Add this under 'forward:' manually, then restart the project:" >&2
    echo "        kube-api:" >&2
    echo "          source: \"tcp:localhost:${LOCAL_PORT}\"" >&2
    echo "          destination: \"${HOST}:tcp:localhost:${remote_port}\"" >&2
    exit 1
  fi
  echo "Updating kube-api forward in ${PROJECT_YAML}..."
  yq -i ".forward[\"kube-api\"].source = \"tcp:localhost:${LOCAL_PORT}\"" "${PROJECT_YAML}"
  yq -i ".forward[\"kube-api\"].destination = \"${HOST}:tcp:localhost:${remote_port}\"" "${PROJECT_YAML}"
  echo "Restarting Mutagen project (${HOST})..."
  ( cd "${PROJECT_DIR}" && mutagen project terminate >/dev/null 2>&1 || true; mutagen project start )
  echo "kube-api forward live: localhost:${LOCAL_PORT} -> ${HOST}:${remote_port}"
fi

cat <<EOF

Use it:
  kubectl --kubeconfig ${OUT} get nodes
  alias k-${HOST}='kubectl --kubeconfig ${OUT}'

Note: the remote kind API port is dynamic. If you recreate the kind cluster,
re-run this script (with --update-mutagen) to refresh the kubeconfig + forward.
EOF
