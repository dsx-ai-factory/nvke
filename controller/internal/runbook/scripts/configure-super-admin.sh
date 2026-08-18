#!/bin/bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0


set -euo pipefail

# Error handling
error() {
    echo "Error: $1" >&2
    exit 1
}

check_super_admin_conf() {
  local super_admin_conf_path="/etc/kubernetes/super-admin.conf"

  if [[ ! -f $super_admin_conf_path ]]; then
    # Super admin config might be a directory if it's been mounted, so remove it
    rm -rf $super_admin_conf_path

    # Create the super admin config
    kubeadm init phase kubeconfig super-admin || error "Failed to create super admin config"

    echo "Super admin config created successfully."
  else
    echo "Super admin config already exists. No action needed."
  fi
}

main() {
  check_super_admin_conf
}

main
