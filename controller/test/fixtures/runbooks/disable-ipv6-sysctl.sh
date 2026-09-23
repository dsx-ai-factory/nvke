#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
set -e

echo "Configuring sysctl IPv6 settings..."

# Create sysctl config file
cat > /etc/sysctl.d/60-disable-ipv6.conf << 'EOF'
net.ipv6.conf.all.disable_ipv6 = 1
net.ipv6.conf.default.disable_ipv6 = 1
net.ipv6.conf.lo.disable_ipv6 = 1
EOF

# Update sysctl. Not a bare "sysctl -p": that reads /etc/sysctl.conf, which
# Ubuntu 26.04 does not ship, so it fails on a 26.04 node. --system reads every
# sysctl.d directory and /etc/sysctl.conf when it exists, so it works on both.
echo "Updating sysctl..."
sysctl --system

# Reload sysctl via procps restart
echo "Reloading sysctl..."
systemctl restart procps
