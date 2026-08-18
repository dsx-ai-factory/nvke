#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Restart kubelet service

set -e

echo "Restarting kubelet service..."
systemctl restart kubelet

echo "Kubelet service has been restarted"
