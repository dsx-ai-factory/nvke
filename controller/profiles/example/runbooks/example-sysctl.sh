#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

: "${EXAMPLE_SYSCTL_VALUE:?EXAMPLE_SYSCTL_VALUE is required}"

install -d -m 0755 /etc/sysctl.d
printf 'net.ipv4.ip_forward = %s\n' "${EXAMPLE_SYSCTL_VALUE}" >/etc/sysctl.d/99-nke-example.conf
sysctl --system
