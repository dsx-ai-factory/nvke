#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
set -e

echo "Enabling IPv4 forwarding..."

# Create sysctl config file
echo "Creating sysctl configuration..."
cat > /etc/sysctl.d/60-enable-ipv4-forward.conf << 'EOF'
net.ipv4.ip_forward = 1
EOF

# Update sysctl. Not a bare "sysctl -p": that reads /etc/sysctl.conf, which
# Ubuntu 26.04 does not ship, so it fails on a 26.04 node. --system reads every
# sysctl.d directory and /etc/sysctl.conf when it exists, so it works on both.
echo "Updating sysctl..."
sysctl --system

# Reload sysctl via procps restart
echo "Reloading sysctl..."
systemctl restart procps

echo "IPv4 forwarding has been enabled"
