#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Disable IPv6 via GRUB configuration

set -e

GRUB_FILE="/etc/default/grub"

echo "Checking GRUB configuration..."

# Check if ipv6.disable=1 is already present
if grep -q '^GRUB_CMDLINE_LINUX=".*ipv6\.disable=1.*' "$GRUB_FILE"; then
    echo "IPv6 is already disabled in GRUB"
    exit 0
fi

# Create backup
cp "$GRUB_FILE" "${GRUB_FILE}.bak.$(date +%F_%T)"

# Add ipv6.disable=1 to GRUB_CMDLINE_LINUX
echo "Adding ipv6.disable=1 to GRUB configuration..."
sed -i 's/^GRUB_CMDLINE_LINUX="\(.*\)"$/GRUB_CMDLINE_LINUX="\1 ipv6.disable=1"/' "$GRUB_FILE"

# Update GRUB
echo "Updating GRUB..."
update-grub

echo "GRUB configuration updated. A reboot is required for changes to take effect."
