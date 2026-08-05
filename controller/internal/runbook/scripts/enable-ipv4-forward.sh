#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
# property and proprietary rights in and to this material, related
# documentation and any modifications thereto. Any use, reproduction,
# disclosure or distribution of this material and related documentation
# without an express license agreement from NVIDIA CORPORATION or
# its affiliates is strictly prohibited.

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
