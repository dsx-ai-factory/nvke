#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Guest provisioner. Artifacts are already in /tmp/nke-image.

set -euo pipefail

SRC=/tmp/nke-image

sudo install -d -m 0755 /usr/lib/nke /etc/nke /var/lib/nke /usr/local/bin /usr/local/lib/spire-agent
sudo install -m 0755 "${SRC}/spire-agent" /usr/local/bin/spire-agent
sudo install -m 0755 "${SRC}/tpm_attestor_agent" /usr/local/lib/spire-agent/tpm_attestor_agent
sudo install -m 0755 "${SRC}/get_tpm_pubhash" /usr/local/bin/get_tpm_pubhash
sudo install -m 0755 "${SRC}/spire-configure.sh" /usr/lib/nke/spire-configure.sh
sudo install -m 0755 "${SRC}/configure-nke-agent-plaintext.sh" /usr/lib/nke/configure-nke-agent-plaintext.sh
sudo install -m 0755 "${SRC}/wait-spiffe-socket.sh" /usr/lib/nke/wait-spiffe-socket.sh
sudo install -m 0644 "${SRC}/spire-agent.conf.tmpl" /usr/lib/nke/spire-agent.conf.tmpl
sudo install -m 0644 "${SRC}/spire-agent-tpm.conf.tmpl" /usr/lib/nke/spire-agent-tpm.conf.tmpl
sudo install -m 0644 "${SRC}/spire-agent.service" /etc/systemd/system/spire-agent.service
sudo install -m 0644 "${SRC}/nke-spire-configure.service" /etc/systemd/system/nke-spire-configure.service

sudo install -m 0755 "${SRC}/nke-agent" /usr/local/bin/nke-agent
sudo install -m 0755 "${SRC}/nke-os-image-check" /usr/local/bin/nke-os-image-check
sudo install -m 0644 "${SRC}/nke-agent.service" /etc/systemd/system/nke-agent.service
sudo dpkg -i "${SRC}/calico-manifests.deb"

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

# The production qcow-imager defaults to locating the root filesystem by this
# label before it injects Forge cloud-init data and updates GRUB. Subiquity's
# LVM layout does not set a filesystem label, so add the label explicitly.
root_device="$(findmnt -n -o SOURCE /)"
if [[ ! -b "${root_device}" ]]; then
  echo "Root filesystem source is not a block device: ${root_device}" >&2
  exit 1
fi
sudo e2label "${root_device}" cloudimg-rootfs
if [[ "$(sudo e2label "${root_device}")" != "cloudimg-rootfs" ]]; then
  echo "Failed to label the root filesystem cloudimg-rootfs" >&2
  exit 1
fi

# Golden image: next boot must look like a new instance, without the Packer
# SSH key or a machine-id from this build VM.
sudo cloud-init clean --logs --seed || true
if [[ -f /etc/machine-id ]]; then
  sudo truncate -s 0 /etc/machine-id
fi
sudo rm -f /var/lib/dbus/machine-id
sudo rm -f /home/ubuntu/.ssh/authorized_keys

# Smaller qcow2: apt's package cache and now-unreferenced blocks (deleted
# files, old package versions) don't shrink the disk image on their own --
# fstrim tells the guest kernel to discard them, which the disk_discard =
# "unmap" builder setting (nke-node-fde.pkr.hcl / nke-node-non-fde.pkr.hcl)
# passes through to the backing qcow2 so Packer's own post-build compaction
# ("Converting hard drive...") actually has free space to reclaim. Must run
# last: any write after this leaves blocks un-trimmed again.
echo "Cleaning up package cache and trimming filesystem..."
sudo apt-get -y autoremove
sudo apt-get clean
sudo fstrim -av || true
