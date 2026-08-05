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

set -euo pipefail

bundle_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

id -u spire >/dev/null 2>&1 || useradd -r -s /bin/false -d /var/lib/spire-agent spire
install -d -m 0755 /usr/local/lib/spire-agent /etc/spire-agent /var/lib/spire-agent
install -m 0755 "${bundle_dir}/bin/spire-agent" /usr/local/bin/spire-agent
install -m 0755 "${bundle_dir}/bin/tpm_attestor_agent" /usr/local/lib/spire-agent/tpm_attestor_agent
install -m 0644 "${bundle_dir}/spire-agent.service" /etc/systemd/system/spire-agent.service
install -m 0644 "${bundle_dir}/spire-agent.env" /etc/spire-agent/spire-agent.env
