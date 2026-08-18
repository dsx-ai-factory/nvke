#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

bundle_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

id -u spire >/dev/null 2>&1 || useradd -r -s /bin/false -d /var/lib/spire-agent spire
install -d -m 0755 /usr/local/lib/spire-agent /etc/spire-agent /var/lib/spire-agent
install -m 0755 "${bundle_dir}/bin/spire-agent" /usr/local/bin/spire-agent
install -m 0755 "${bundle_dir}/bin/tpm_attestor_agent" /usr/local/lib/spire-agent/tpm_attestor_agent
install -m 0644 "${bundle_dir}/spire-agent.service" /etc/systemd/system/spire-agent.service
install -m 0644 "${bundle_dir}/spire-agent.env" /etc/spire-agent/spire-agent.env
