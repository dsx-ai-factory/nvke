#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2024 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Wait for any in-flight apt/dpkg operations to finish before rebooting,
# to avoid corrupting package state or interrupting an active install.
APT_LOCKS="/var/lib/dpkg/lock-frontend /var/lib/dpkg/lock /var/cache/apt/archives/lock"
TIMEOUT=300
ELAPSED=0
if ! command -v fuser >/dev/null 2>&1; then
    echo "fuser not found; skipping apt/dpkg lock check before reboot."
fi
while command -v fuser >/dev/null 2>&1 && fuser $APT_LOCKS >/dev/null 2>&1; do
    if [ "$ELAPSED" -ge "$TIMEOUT" ]; then
        echo "Timed out after ${TIMEOUT}s waiting for apt/dpkg locks; proceeding with reboot anyway."
        break
    fi
    echo "Waiting for apt/dpkg locks to clear (${ELAPSED}s elapsed)..."
    sleep 5
    ELAPSED=$((ELAPSED + 5))
done

# Reboot the system without waiting for the command to return
echo "Rebooting the system now..."
nohup reboot >/dev/null 2>&1 &
