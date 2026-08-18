#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

CWE_HTTP_PORT="${CWE_HTTP_PORT:-8080}"
API_URL="${API_URL:-http://localhost:${CWE_HTTP_PORT}/api}"
SITES_API_URL="${API_URL%/}/sites"
MOCK_SITE_ID="${MOCK_SITE_ID:-00000000-0000-0000-0000-000000000001}"

if curl -fsS "${SITES_API_URL}/${MOCK_SITE_ID}" >/dev/null 2>&1; then
  echo "Mock site ${MOCK_SITE_ID} already exists"
  exit 0
fi

curl -fsS -X POST "${SITES_API_URL}" \
  -H "Content-Type: application/json" \
  -d "{\"id\":\"${MOCK_SITE_ID}\",\"vipRanges\":[]}" >/dev/null

echo "Registered mock site ${MOCK_SITE_ID}"
