#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

CWE_HTTP_PORT="${CWE_HTTP_PORT:-8080}"
API_URL="${API_URL:-http://localhost:${CWE_HTTP_PORT}/api}"
SITES_API_URL="${API_URL%/}/sites"

# /api/* is authenticated. hack/nke-token.sh prints nothing when NKE is not, so
# this works either way.
TOKEN="$("${SCRIPT_DIR}/nke-token.sh")"
AUTH_HEADER=()
if [[ -n "${TOKEN}" ]]; then
  AUTH_HEADER=(-H "Authorization: Bearer ${TOKEN}")
fi
MOCK_SITE_ID="${MOCK_SITE_ID:-00000000-0000-0000-0000-000000000001}"

# Check for 404 specifically, not any failure. A bare `curl -fsS` existence
# check reads a 401, a 500 or a refused connection as "the site does not exist"
# and falls through to a POST that then fails confusingly.
status="$(
  curl -sS -o /dev/null -w '%{http_code}' \
    "${AUTH_HEADER[@]+"${AUTH_HEADER[@]}"}" \
    "${SITES_API_URL}/${MOCK_SITE_ID}"
)" || {
  echo "register-mock-site.sh: could not reach ${SITES_API_URL} -- is NKE up?" >&2
  exit 1
}

case "${status}" in
  200)
    echo "Mock site ${MOCK_SITE_ID} already exists"
    exit 0
    ;;
  404) ;;
  401 | 403)
    echo "register-mock-site.sh: NKE answered ${status}; the API is authenticating" >&2
    echo "  and this caller was not admitted. hack/nke-token.sh mints the credential." >&2
    exit 1
    ;;
  *)
    echo "register-mock-site.sh: unexpected ${status} for GET ${SITES_API_URL}/${MOCK_SITE_ID}" >&2
    exit 1
    ;;
esac

curl "${AUTH_HEADER[@]+"${AUTH_HEADER[@]}"}" -fsS -X POST "${SITES_API_URL}" \
  -H "Content-Type: application/json" \
  -d "{\"id\":\"${MOCK_SITE_ID}\",\"vipRanges\":[]}" >/dev/null

echo "Registered mock site ${MOCK_SITE_ID}"
