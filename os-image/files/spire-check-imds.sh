#!/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
# Fail explicitly before SPIRE starts when the selected optional plugin is absent.
set -eu
if grep -Eq '^[[:space:]]*NodeAttestor[[:space:]]+"dsx_imds"[[:space:]]*\{' /etc/spire-agent/agent.conf; then
  if [ ! -x /usr/local/lib/spire-agent/dsx-imds-node-attestor-agent ]; then
    echo 'spire-agent: dsx_imds selected but /usr/local/lib/spire-agent/dsx-imds-node-attestor-agent is missing or not executable; install a spire-agent package built with IMDS_AGENT_FILE, IMDS_AGENT_SHA256 and IMDS_AGENT_ARCH' >&2
    exit 1
  fi
fi
