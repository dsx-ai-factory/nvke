#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Installs the first-boot storage-growth service for the plain-LVM (no
# encryption) build variant. Counterpart to configure-fde.sh, minus
# everything LUKS/Clevis/TPM — this variant follows the same
# installer+autoinstall+LVM flow as nvke-node-fde.pkr.hcl so the two are
# comparable, but ships no encryption at all.

set -euo pipefail

SRC=/var/tmp/nke-image/lvm

echo "Installing LVM first-boot growth script + service..."
sudo install -m 0755 "${SRC}/lvm-growth-firstboot.sh" /usr/local/bin/lvm-growth-firstboot.sh
sudo install -m 0644 "${SRC}/lvm-growth-firstboot.service" /etc/systemd/system/lvm-growth-firstboot.service
sudo systemctl daemon-reload
sudo systemctl enable lvm-growth-firstboot.service

echo "LVM configuration complete. Storage growth deferred to first boot on target hardware."
