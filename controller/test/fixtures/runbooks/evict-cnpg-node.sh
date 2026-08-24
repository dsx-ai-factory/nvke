#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

echo "Error: the evict-cnpg-node fixture must only run with the teardown-hook failpoint enabled" >&2
exit 1
