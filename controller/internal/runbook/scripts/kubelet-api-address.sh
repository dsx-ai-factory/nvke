#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Purpose: Update the 'server' field in /etc/kubernetes/kubelet.conf
#          Changes are made in a temporary file and then replace the original file (no kubelet restart)
# Prerequisites:
# - Kubelet conf file (defaults to /etc/kubernetes/kubelet.conf)
# - Running as root or with sudo privileges to modify file provided for $KUBELET_CONF var

set -euo pipefail

API_SERVER=${API_SERVER:-""}
KUBELET_CONF=${KUBELET_CONF:-"/etc/kubernetes/kubelet.conf"}

error() {
    echo "Error: $1" >&2
    exit 1
}

backup_file() {
    local file="$1"
    if [[ -f "$file" ]]; then
        cp "$file" "${file}.bak.$(date +%s)" || error "Failed to backup $file"
        echo "Backup of kubelet conf created at ${file}.bak.$(date +%s)"
    fi
}

validate_input() {
    if [[ -z "$API_SERVER" ]]; then
        error "API_SERVER must be set"
    fi

    if [[ ! "$API_SERVER" =~ ^https://[a-zA-Z0-9._:-]+:6443$ ]]; then
        error "API_SERVER must be a valid address of the form: https://<host/ip>:6443"
    fi

    if [[ ! -f "$KUBELET_CONF" ]]; then
        error "Kubelet config file $KUBELET_CONF does not exist"
    fi
}

check_for_expected() {
    current_server=$(sed -nE 's/^[[:space:]]*server:[[:space:]]*(.*)$/\1/p' "$KUBELET_CONF")

    if [[ "$current_server" == "$API_SERVER" ]]; then
        echo "✅ No update needed; server is already set to $API_SERVER"
        exit 0
    fi
}

update_kubelet_api_address() {
    # backup file
    backup_file "$KUBELET_CONF"

    local tmpfile
    tmpfile=$(mktemp /tmp/kubelet.conf.XXXXXX)
    # Update the configuration using yq and save to tmp file
    sed "s|^\([[:space:]]*server:\).*|\1 $API_SERVER|" "$KUBELET_CONF" > "$tmpfile"

    # save current server
    current_server=$(sed -nE 's/^[[:space:]]*server:[[:space:]]*(.*)$/\1/p' "$KUBELET_CONF")

    # Replace the original file with the new content
    mv "$tmpfile" "$KUBELET_CONF"
    chmod 600 "$KUBELET_CONF" # ensures only root has rw access

    echo "✅ Updated server in kubelet conf file $KUBELET_CONF from $current_server -> $API_SERVER"
}

main() {
    validate_input
    check_for_expected
    update_kubelet_api_address
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    main "$@"
fi
