#!/bin/bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
# property and proprietary rights in and to this material, related
# documentation and any modifications thereto. Any use, reproduction,
# disclosure or distribution of this material and related documentation
# without an express license agreement from NVIDIA CORPORATION or
# its affiliates is strictly prohibited.


# Default values from the Slack thread
NET_CORE_RMEM_MAX="${NET_CORE_RMEM_MAX:-"5242880"}"
NET_CORE_WMEM_MAX="${NET_CORE_WMEM_MAX:-"5242880"}"
NET_IPV4_TCP_RMEM="${NET_IPV4_TCP_RMEM:-"4096 87380 16777216"}"
NET_IPV4_TCP_WMEM="${NET_IPV4_TCP_WMEM:-"4096 65536 16777216"}"
SYSCTL_CONF="${SYSCTL_CONF:-"/etc/sysctl.d/70-deepstream-network.conf"}"

configure_deepstream_sysctl() {
  echo "Configuring network sysctl parameters for Deepstream..."

  cat > $SYSCTL_CONF <<EOF
# Network parameters for Deepstream workloads
net.core.rmem_max=$NET_CORE_RMEM_MAX
net.core.wmem_max=$NET_CORE_WMEM_MAX
net.ipv4.tcp_rmem=$NET_IPV4_TCP_RMEM
net.ipv4.tcp_wmem=$NET_IPV4_TCP_WMEM
EOF

  # Apply the settings
  sysctl --system
  echo "Deepstream network sysctl configuration completed"
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  configure_deepstream_sysctl
fi
