#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# verify-routes.sh - quick BGP-state snapshot from FRR. Use this at three
# points during a drain-kube-vip test:
#
#   1. BEFORE drain - should see N established sessions (one per CP) and the
#      VIP route advertised by N peers (ECMP-style multipath).
#   2. AFTER drain on cp-X - that peer drops to "Active"/"Idle" and the VIP
#      route should now be advertised by N-1 peers. The route from cp-X is
#      gone from `show bgp ipv4 unicast`.
#   3. AFTER reset - peer state should remain Idle, and the route from cp-X
#      remains withdrawn.

set -euo pipefail
CONTAINER_NAME="${CONTAINER_NAME:-bgp-containers}"

if ! docker ps --format '{{.Names}}' | grep -qx "${CONTAINER_NAME}"; then
  echo "FRR container '${CONTAINER_NAME}' is not running. Run up.sh first." >&2
  exit 1
fi

echo "=== BGP peer summary ==="
status=0
docker exec "${CONTAINER_NAME}" vtysh -c 'show bgp summary' || status=$?

echo
echo "=== BGP ipv4 unicast (received routes) ==="
docker exec "${CONTAINER_NAME}" vtysh -c 'show bgp ipv4 unicast' || status=$?

echo
echo "=== Detailed peer info ==="
docker exec "${CONTAINER_NAME}" vtysh -c 'show bgp neighbors' 2>/dev/null \
  | grep -E "^BGP neighbor|BGP state|Last reset|Local host|Foreign host|Update source" || status=$?

exit "${status}"
