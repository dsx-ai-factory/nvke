#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -Eeuo pipefail

# Container nodes share the Docker host kernel. ARC nodes have no host module
# files mounted, and the Debian node image cannot supply AWS modules.
# Load OVS once before any nodes boot; their ovs-ctl then skips modprobe.
docker run --rm -i --privileged \
  -v /lib/modules:/host-modules:ro \
  ubuntu:24.04@sha256:008173c23f95b170204355c12626cb5a965d779a7e1283b09e9cffbb1bf33ca3 \
  bash -s <<'SCRIPT'
set -Eeuo pipefail
if [[ -d /sys/module/openvswitch ]]; then
  echo "Open vSwitch is already loaded for $(uname -r)"
  exit 0
fi
apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends kmod
if ! modprobe -d /host-modules openvswitch; then
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends "linux-modules-$(uname -r)"
  modprobe openvswitch
fi
test -d /sys/module/openvswitch
echo "Open vSwitch loaded for $(uname -r)"
SCRIPT
