#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Guest provisioner. Artifacts are already in /tmp/nke-image.

set -euo pipefail

SRC=/tmp/nke-image

sudo install -d -m 0755 /usr/lib/nke /etc/nke /var/lib/nke /usr/local/bin
sudo install -m 0755 "${SRC}/spire-agent" /usr/local/bin/spire-agent
sudo install -m 0755 "${SRC}/spire-configure.sh" /usr/lib/nke/spire-configure.sh
sudo install -m 0755 "${SRC}/wait-spiffe-socket.sh" /usr/lib/nke/wait-spiffe-socket.sh
sudo install -m 0644 "${SRC}/spire-agent.conf.tmpl" /usr/lib/nke/spire-agent.conf.tmpl
sudo install -m 0644 "${SRC}/spire-agent.service" /etc/systemd/system/spire-agent.service
sudo install -m 0644 "${SRC}/nke-spire-configure.service" /etc/systemd/system/nke-spire-configure.service

if [[ -f "${SRC}/nke-agent.deb" ]]; then
  # policy-rc.d 101: do not start services during the golden-image build.
  # The stock postinst would otherwise systemctl restart nke-agent before SPIRE
  # exists on this VM.
  echo '#!/bin/sh' | sudo tee /usr/sbin/policy-rc.d >/dev/null
  echo 'exit 101' | sudo tee -a /usr/sbin/policy-rc.d >/dev/null
  sudo chmod 0755 /usr/sbin/policy-rc.d
  sudo dpkg -i "${SRC}/nke-agent.deb"
  sudo rm -f /usr/sbin/policy-rc.d
  sudo install -d -m 0755 /etc/systemd/system/nke-agent.service.d
  sudo install -m 0644 "${SRC}/nke-agent-spiffe.conf" /etc/systemd/system/nke-agent.service.d/spiffe.conf
else
  sudo install -m 0755 "${SRC}/nke-agent" /usr/local/bin/nke-agent
  sudo install -m 0644 "${SRC}/nke-agent.service" /etc/systemd/system/nke-agent.service
fi

if [[ ! -f /etc/default/nke-agent ]]; then
  echo 'AGENT_ARGS=""' | sudo tee /etc/default/nke-agent >/dev/null
fi

if ! id -u spire >/dev/null 2>&1; then
  sudo useradd -r -s /bin/false -d /var/lib/spire-agent spire
fi
sudo install -d -m 0750 -o spire -g spire /var/lib/spire-agent

sudo systemctl daemon-reload
sudo systemctl enable nke-spire-configure.service
sudo systemctl enable spire-agent.service
sudo systemctl enable nke-agent.service

# Golden image: next boot must look like a new instance, without the Packer
# SSH key or a machine-id from this build VM.
sudo cloud-init clean --logs --seed || true
if [[ -f /etc/machine-id ]]; then
  sudo truncate -s 0 /etc/machine-id
fi
sudo rm -f /var/lib/dbus/machine-id
sudo rm -f /home/ubuntu/.ssh/authorized_keys
