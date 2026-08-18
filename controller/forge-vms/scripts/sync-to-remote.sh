#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Sync forge-vms code to a remote machine.
#
# Usage: ./scripts/sync-to-remote.sh [--ssh|--tsh] <host>
#
# Examples:
#   ./scripts/sync-to-remote.sh my-machine.nvidia.com                    # auto-detect (tries ssh)
#   ./scripts/sync-to-remote.sh --ssh my-machine.nvidia.com              # force ssh
#   ./scripts/sync-to-remote.sh --tsh nke-site-forge-az24-dev8-...  # force tsh

set -euo pipefail

TRANSPORT=""
HOST=""

for arg in "$@"; do
  case "$arg" in
    --ssh) TRANSPORT="ssh" ;;
    --tsh) TRANSPORT="tsh ssh" ;;
    *)     HOST="$arg" ;;
  esac
done

if [ -z "$HOST" ]; then
  echo "Usage: $0 [--ssh|--tsh] <host>" >&2
  exit 1
fi

# Auto-detect transport if not specified
if [ -z "$TRANSPORT" ]; then
  if command -v tsh >/dev/null 2>&1 && tsh status >/dev/null 2>&1; then
    TRANSPORT="tsh ssh"
  else
    TRANSPORT="ssh"
  fi
fi

REMOTE_DIR="${FORGE_VMS_REMOTE_DIR:-~/dev/nke/cloud-workflow-engine/forge-vms}/"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

echo "Syncing $REPO_ROOT → $HOST:$REMOTE_DIR (via $TRANSPORT)"

cd "$REPO_ROOT"

rsync -avz -e "$TRANSPORT" \
  --exclude='.git' \
  --exclude='.cache' \
  --exclude='.gocache' \
  --exclude='.gomodcache' \
  --exclude='var' \
  --exclude='images' \
  --exclude='bin' \
  --exclude='vendor' \
  "$REPO_ROOT/" \
  "$HOST:$REMOTE_DIR"

echo "Done."
