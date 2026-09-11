#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# nico-vms-console-log.sh — View VM console output from the serial log.
#
# Usage:
#   ./scripts/nico-vms-console-log.sh        # print current log
#   ./scripts/nico-vms-console-log.sh -f     # follow (tail -f)
#   ./scripts/nico-vms-console-log.sh -n 50  # last 50 lines

set -euo pipefail

FOLLOW=false
LINES=100

while [[ $# -gt 0 ]]; do
    case "$1" in
        -f|--follow) FOLLOW=true; shift ;;
        -n) LINES="$2"; shift 2 ;;
        -h|--help)
            echo "Usage: $0 [-f] [-n LINES]"
            echo "  -f        follow log (like tail -f)"
            echo "  -n LINES  show last N lines (default: 100)"
            exit 0
            ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
done

# Find the console log — look in common integration test locations
LOGFILE=""
for dir in /tmp/nico-vms-integration; do
    found=$(find "$dir" -name "*.console.log" 2>/dev/null | head -1)
    if [ -n "$found" ]; then
        LOGFILE="$found"
        break
    fi
done

if [ -z "$LOGFILE" ]; then
    echo "No console log found. Is a VM running?"
    echo "Searched: /tmp/nico-vms-integration/**/*.console.log"
    exit 1
fi

echo "Console log: $LOGFILE ($(wc -c < "$LOGFILE") bytes)"
echo "---"

# strip_escapes: remove ANSI escape sequences and non-printable chars
strip_escapes() {
    strings | sed 's/\x1b\[[0-9;]*[a-zA-Z]//g'
}

if $FOLLOW; then
    tail -f "$LOGFILE" | strip_escapes
else
    tail -n "$LINES" "$LOGFILE" | strip_escapes
fi
