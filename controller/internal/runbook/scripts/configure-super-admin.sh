#!/bin/bash
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
