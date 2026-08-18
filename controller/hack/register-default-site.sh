#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

API_URL="${API_URL:-http://localhost:8080/api}"
DEFAULT_SITE_ID="${DEFAULT_SITE_ID:-00000000-0000-0000-0000-000000000000}"
# VIP CIDR allocated to clusters on the default container-node site. Distinct from
# forge-vms FORGE_VMS_VIP_CIDR (172.31.0.0/24) so co-located e2e does not
# collide (NKX-13387). Must be reachable from the CAPD and NICo worker paths —
# paired with the host-mode FRR helper. Keep this off the kind Docker
# subnet (172.18.0.0/16) so packets exit via the default gateway.
DEFAULT_VIP_RANGE="${DEFAULT_VIP_RANGE:-172.31.1.0/24}"
SITES_API_URL="${API_URL%/}/sites"

if curl -fsS "${SITES_API_URL}/${DEFAULT_SITE_ID}" >/dev/null 2>&1; then
  echo "Default site ${DEFAULT_SITE_ID} already exists"
  exit 0
fi

curl -fsS -X POST "${SITES_API_URL}" \
  -H "Content-Type: application/json" \
  -d "{\"id\":\"${DEFAULT_SITE_ID}\",\"vipRanges\":[\"${DEFAULT_VIP_RANGE}\"]}" >/dev/null

echo "Registered default site ${DEFAULT_SITE_ID} with vipRanges=[${DEFAULT_VIP_RANGE}]"
