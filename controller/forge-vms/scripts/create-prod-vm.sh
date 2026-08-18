#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Create a production FDE VM with real cloud-init (kubeadm + OVN).
# Usage: ./scripts/create-prod-vm.sh [server-url]
set -euo pipefail

URL="${1:-http://localhost:18080}"
NAME="prod-manual-$(date +%s)"

IPXE_SCRIPT='#!ipxe
set boot-loader https://d3mq564cn7sth7.cloudfront.net/nke-isos/qcow/efi/x86_64/2026-03-10/1773155704/qcow-imager.efi
set image-url https://d3mq564cn7sth7.cloudfront.net/nke-isos/qcow/images/24.04/x86_64/lvm/2026-03-23/1774290565/nke-24.04-x86_64-lvm-2026-03-23-1774290565.qcow2
set disk-strategy smallest
set console-config console=tty0 console=ttyS1,115200
set kernel-params pci=realloc=off reboot=pci loglevel=1 iommu=pt intel_iommu=on
set luks-key luks.key=changeit
set cloud-init-ds ds=nocloud-net;s=${cloudinit-url}
chain ${boot-loader} ${console-config} ${kernel-params} ${luks-key} image_url=${image-url} disk_strategy=${disk-strategy} ${cloud-init-ds} update_grub_template=yes update_grub_cfg=yes
boot'

USER_DATA="#cloud-config
hostname: ${NAME}
preserve_hostname: false
manage_etc_hosts: true
ssh_pwauth: true
package_update: true
package_upgrade: false

users:
  - name: testuser
    plain_text_passwd: testpass
    lock_passwd: false
    sudo: \"ALL=(ALL) NOPASSWD:ALL\"
    shell: /bin/bash

runcmd:
  - echo prod-manual-ok > /tmp/prod-marker
  - /usr/local/bin/fetch-and-install-bundle.sh https://d3mq564cn7sth7.cloudfront.net/nke_bundles/Ubuntu_24.04/x86_64/kubeadm_v1.34.2-5_x86_64.tgz ./kubeadm-install.py
  - DEBIAN_FRONTEND=noninteractive apt --fix-broken install -y
  - /usr/local/bin/fetch-and-install-bundle.sh https://d3mq564cn7sth7.cloudfront.net/nke_bundles/Ubuntu_24.04/x86_64/ovn_1.4.0-2_x86_64.tgz ./install.sh -i
  - echo SERVER=REPLACED-BY-SWE >> /etc/dgx/attestation/vars
  - sysctl --system
  - systemctl daemon-reload
  - systemctl restart nke-agent.service
  - systemctl restart procps

phone_home:
    POST: all
    url: http://169.254.169.254:7777/latest/meta-data/phone_home"

echo "Creating VM: ${NAME}"
go run ./cmd/forge-vmsctl instance create \
  --name "$NAME" \
  --ipxe-script "$IPXE_SCRIPT" \
  --user-data "$USER_DATA" \
  --phone-home-enabled \
  --url "$URL" \
  --token dummy
