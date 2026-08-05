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

# Purpose: Configure kubelet reserved resources for CPU, memory, and ephemeral storage
# Prerequisites:
# - yq tool (will be installed if not present)
# - Kubelet configuration file (defaults to /var/lib/kubelet/config.yaml)

set -euo pipefail

export KUBELET_CONFIG="${KUBELET_CONFIG:-"/var/lib/kubelet/config.yaml"}"
export KUBE_RESERVED_CPU="${KUBE_RESERVED_CPU:-"1000m"}"
export KUBE_RESERVED_MEM="${KUBE_RESERVED_MEM:-"2Gi"}"
export KUBE_RESERVED_STORAGE="${KUBE_RESERVED_STORAGE:-"1Gi"}"
export SYSTEM_RESERVED_MEM="${SYSTEM_RESERVED_MEM:-"16Gi"}"

error() {
  echo "Error: $1" >&2
  exit 1
}

# Optional: Function to backup files before modification
backup_file() {
  local file="$1"
  if [[ -f "$file" ]]; then
      cp "$file" "${file}.bak" || error "Failed to backup $file"
  fi
}

# Optional: Check for required dependencies
check_dependencies() {
  if ! command -v yq >/dev/null; then
      wget -qO /usr/bin/yq https://github.com/mikefarah/yq/releases/latest/download/yq_linux_amd64
      chmod +x /usr/bin/yq
  fi
}

# Optional: Validate input or environment variables
validate_input() {
  # Check if kubelet config exists
  if [ ! -f "$KUBELET_CONFIG" ]; then
      error "Error: Kubelet config file not found at $KUBELET_CONFIG"
  fi
}

# Add your functions here
update_kubelet_config() {
  # Update the configuration using yq
  yq eval-all --prettyPrint '
    select(fileIndex == 0) * select(fileIndex == 1) |
    .kubeReserved = {
      "cpu": "'"$KUBE_RESERVED_CPU"'",
      "memory": "'"$KUBE_RESERVED_MEM"'",
      "ephemeral-storage": "'"$KUBE_RESERVED_STORAGE"'"
    } |
    .systemReserved = {
      "memory": "'"$SYSTEM_RESERVED_MEM"'"
    }
  ' "$KUBELET_CONFIG" <(echo "{}") > "${KUBELET_CONFIG}.tmp"

  # Replace the original file with the new content
  mv "${KUBELET_CONFIG}.tmp" "$KUBELET_CONFIG"
}

# Main function to orchestrate the script
main() {
  backup_file "$KUBELET_CONFIG"
  check_dependencies
  validate_input
  update_kubelet_config
}

# Standard boilerplate to run main() only if script is executed directly.
# This allows sourcing this script for testing purposes without executing the main function.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    main "$@"
fi
