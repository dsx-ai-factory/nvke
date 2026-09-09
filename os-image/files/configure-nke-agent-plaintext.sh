#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Select plaintext bootstrap mode only when the agent does not already answer
# a real plaintext gRPC request.

set -euo pipefail

agent_port="${NKE_AGENT_PORT:-15005}"
if /usr/local/bin/nke-os-image-check \
  --plaintext \
  --endpoint "127.0.0.1:${agent_port}" \
  --timeout 5s >/dev/null 2>&1; then
  exit 0
fi

install -d -m 0755 /etc/default /etc/systemd/system/nke-agent.service.d
cat >/etc/default/nke-agent <<EOF
AGENT_ARGS="--disable-mtls --secure-server-port ${agent_port}"
EOF
chmod 0644 /etc/default/nke-agent

cat >/etc/systemd/system/nke-agent.service.d/bootstrap-plaintext.conf <<'EOF'
[Unit]
ConditionPathExists=
After=
After=network-online.target
Wants=
Wants=network-online.target

[Service]
ExecStartPre=
ExecStartPost=
EOF
chmod 0644 /etc/systemd/system/nke-agent.service.d/bootstrap-plaintext.conf

systemctl disable --now spire-agent.service nke-spire-configure.service >/dev/null 2>&1 || true
systemctl daemon-reload
systemctl enable nke-agent.service >/dev/null
systemctl restart --no-block nke-agent.service
