#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

API_URL="${API_URL:-http://localhost:8080/api}"
DEFAULT_SITE_ID="${DEFAULT_SITE_ID:-00000000-0000-0000-0000-000000000000}"
# VIP CIDR allocated to clusters on the default container-node site. Distinct from
# nico-vms NICO_VMS_VIP_CIDR (172.31.0.0/24) so co-located e2e does not
# collide (NKX-13387). Must be reachable from the CAPD and NICo worker paths —
# paired with the host-mode FRR helper. Keep this off the kind Docker
# subnet (172.18.0.0/16) so packets exit via the default gateway.
DEFAULT_VIP_RANGE="${DEFAULT_VIP_RANGE:-172.31.1.0/24}"
SITES_API_URL="${API_URL%/}/sites"

# /api/sites is authenticated. nke-token.sh prints nothing when NKE is not, and
# the array expansion below then drops the header, so this works either way.
TOKEN="$("${SCRIPT_DIR}/nke-token.sh")"
AUTH_HEADER=()
if [[ -n "${TOKEN}" ]]; then
  AUTH_HEADER=(-H "Authorization: Bearer ${TOKEN}")
fi

# Checks for 404 specifically. A bare `curl -fsS` would read a 401, a 500 or a
# refused connection as "does not exist" and fall through to a POST that fails
# confusingly.
status="$(
  curl -sS -o /dev/null -w '%{http_code}' \
    "${AUTH_HEADER[@]+"${AUTH_HEADER[@]}"}" \
    "${SITES_API_URL}/${DEFAULT_SITE_ID}"
)" || {
  echo "register-default-site.sh: could not reach ${SITES_API_URL} -- is NKE up?" >&2
  exit 1
}

case "${status}" in
  200)
    echo "Default site ${DEFAULT_SITE_ID} already exists"
    exit 0
    ;;
  404)
    # Not registered yet; fall through and create it.
    ;;
  401 | 403)
    echo "register-default-site.sh: NKE answered ${status} for GET ${SITES_API_URL}/${DEFAULT_SITE_ID}." >&2
    echo "  The API is authenticating and this caller was not admitted." >&2
    echo "  hack/nke-token.sh mints the credential; check that" >&2
    echo "  deploy/k8s/nke-auth-dev.yaml is applied and that its admit rules name" >&2
    echo "  the service account it mints for." >&2
    exit 1
    ;;
  *)
    echo "register-default-site.sh: unexpected ${status} for GET ${SITES_API_URL}/${DEFAULT_SITE_ID}" >&2
    exit 1
    ;;
esac

curl -fsS -X POST "${SITES_API_URL}" \
  "${AUTH_HEADER[@]+"${AUTH_HEADER[@]}"}" \
  -H "Content-Type: application/json" \
  -d "{\"id\":\"${DEFAULT_SITE_ID}\",\"vipRanges\":[\"${DEFAULT_VIP_RANGE}\"]}" >/dev/null

echo "Registered default site ${DEFAULT_SITE_ID} with vipRanges=[${DEFAULT_VIP_RANGE}]"
