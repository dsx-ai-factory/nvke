#!/bin/bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Entrypoint for a local-csc node container: turns the CAPD node image into a
# pre-provisioned host for `nvkectl bootstrap site --disable-mtls`, then hands
# off to the image's own entrypoint. Runs before systemd, on every start.
set -euo pipefail

f=/etc/default/nke-agent
grep -q -- '--secure-server-port 15005' "$f"
grep -q -- '--disable-mtls' "$f" || sed -i 's/^AGENT_ARGS="\(.*\)"$/AGENT_ARGS="\1 --disable-mtls"/' "$f"
grep -q -- '--disable-mtls' "$f"

install -d /var/lib/nico-containers/cloud-init
printf 'done\n' >/var/lib/nico-containers/cloud-init/status

# Must precede first boot: nke-container-network moves the uplink into OVS.
SYSTEMD_OFFLINE=1 systemctl disable nke-container-network.service swtpm-local.service spire-agent.service

# A new netns copies the VM's value at creation; kubeadm preflight requires 1.
sysctl -w net.ipv4.ip_forward=1

exec /usr/local/bin/entrypoint /sbin/init
