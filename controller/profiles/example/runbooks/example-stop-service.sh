#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Runs from the example profile's beforeRemoveNode hook: stop a site service
# before the node leaves the cluster.

set -euo pipefail

: "${EXAMPLE_SERVICE:?EXAMPLE_SERVICE is required}"

if systemctl is-active --quiet "${EXAMPLE_SERVICE}"; then
  systemctl stop "${EXAMPLE_SERVICE}"
fi
