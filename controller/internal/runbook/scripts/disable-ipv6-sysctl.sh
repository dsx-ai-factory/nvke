#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Disable IPv6 via sysctl configuration

set -e

echo "Configuring sysctl IPv6 settings..."

# Create sysctl config file
cat > /etc/sysctl.d/60-disable-ipv6.conf << 'EOF'
net.ipv6.conf.all.disable_ipv6 = 1
net.ipv6.conf.default.disable_ipv6 = 1
net.ipv6.conf.lo.disable_ipv6 = 1
EOF

# Update sysctl
echo "Updating sysctl..."
sysctl -p

# Reload sysctl via procps restart
echo "Reloading sysctl..."
systemctl restart procps
