#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Enable IPv4 forwarding

set -e

echo "Enabling IPv4 forwarding..."

# Create sysctl config file
echo "Creating sysctl configuration..."
cat > /etc/sysctl.d/60-enable-ipv4-forward.conf << 'EOF'
net.ipv4.ip_forward = 1
EOF

# Update sysctl
echo "Updating sysctl..."
sysctl -p

# Reload sysctl via procps restart
echo "Reloading sysctl..."
systemctl restart procps

echo "IPv4 forwarding has been enabled"
