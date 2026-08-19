#!/bin/bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# simple_node_uptime.sh
#
# This script displays the current uptime of the local node in human-readable format
# and the boot timestamp in ISO 8601 format.

# Function to convert seconds to human readable format
seconds_to_human() {
    local seconds=$1
    local days=$((seconds/86400))
    local hours=$(((seconds%86400)/3600))
    local minutes=$(((seconds%3600)/60))
    local secs=$((seconds%60))

    echo "${days}d ${hours}h ${minutes}m ${secs}s"
}

# Get hostname
hostname=$(hostname)

# Read uptime from /proc/uptime
uptime_data=$(cat /proc/uptime)
uptime_sec=$(echo "$uptime_data" | awk '{print $1}' | cut -d. -f1)

# Calculate boot timestamp.
# Prefer journalctl --list-boots: on bare-metal this equals /proc/uptime-derived time,
# but on Docker container nodes it reflects the container restart time (not the VM kernel
# boot time that /proc/uptime reports — containers share the host kernel).
boot_timestamp=""
if command -v journalctl >/dev/null 2>&1; then
    boot_entry=$(journalctl --list-boots 2>/dev/null | grep '^\s*0 ' | awk '{print $4, $5}')
    if [ -n "$boot_entry" ]; then
        boot_timestamp=$(date -d "$boot_entry" '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null)
    fi
fi
if [ -z "$boot_timestamp" ]; then
    current_time=$(date +%s)
    boot_time=$((current_time - uptime_sec))
    boot_timestamp=$(date -d "@${boot_time}" '+%Y-%m-%dT%H:%M:%SZ')
fi

# Get human readable uptime
uptime_human=$(seconds_to_human $uptime_sec)

# Print result
echo "Node: $hostname"
echo "Boot Time: $boot_timestamp"
echo "Uptime: $uptime_human"
