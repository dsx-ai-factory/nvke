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

# Purpose: Create kubeadm patches directory and generate desired patches
# Prerequisites:
# - Write access to /etc/kubernetes/patches directory

set -euo pipefail

export KUBEADM_PATCHES_DIR="${KUBEADM_PATCHES_DIR:-"/etc/kubernetes/patches"}"
export KUBE_RESERVED_CPU="${KUBE_RESERVED_CPU:-"1000m"}"
export KUBE_RESERVED_MEM="${KUBE_RESERVED_MEM:-"2Gi"}"
export KUBE_RESERVED_STORAGE="${KUBE_RESERVED_STORAGE:-"1Gi"}"
export SYSTEM_RESERVED_CPU="${SYSTEM_RESERVED_CPU:-""}"
export SYSTEM_RESERVED_MEM="${SYSTEM_RESERVED_MEM:-"16Gi"}"
export SYSTEM_RESERVED_STORAGE="${SYSTEM_RESERVED_STORAGE:-""}"
export MAX_PODS="${MAX_PODS:-""}"

error() {
  echo "Error: $1" >&2
  exit 1
}

validate_input() {
  if [ -z "$KUBEADM_PATCHES_DIR" ]; then
    error "KUBEADM_PATCHES_DIR is an empty string - cannot create directory"
  fi
  if [[ -n "$MAX_PODS" ]] && ! [[ "$MAX_PODS" =~ ^[1-9][0-9]*$ ]]; then
    error "MAX_PODS must be a positive integer, got: '$MAX_PODS'"
  fi
}

# Ensure the kubeadm patches directory exists if it is not an empty string
ensure_kubeadm_patches_directory() {
  if [ ! -d "$KUBEADM_PATCHES_DIR" ]; then
      echo "Creating Kubeadm Patches dir $KUBEADM_PATCHES_DIR"
      mkdir -p "$KUBEADM_PATCHES_DIR" || error "Failed to create $KUBEADM_PATCHES_DIR directory"
  fi
}

# Create kubelet configuration patch file
create_kubelet_patch() {
  local patch_file="$KUBEADM_PATCHES_DIR/kubeletconfiguration0+merge.yaml"

  echo "Creating kubelet configuration patch at: $patch_file"

  cat > "$patch_file" << EOF
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
kubeReserved:
  cpu: "$KUBE_RESERVED_CPU"
  memory: "$KUBE_RESERVED_MEM"
  ephemeral-storage: "$KUBE_RESERVED_STORAGE"
systemReserved:
  memory: "$SYSTEM_RESERVED_MEM"
EOF

  if [[ -n "$SYSTEM_RESERVED_CPU" ]]; then
    echo "  cpu: \"$SYSTEM_RESERVED_CPU\"" >> "$patch_file"
  fi
  if [[ -n "$SYSTEM_RESERVED_STORAGE" ]]; then
    echo "  ephemeral-storage: \"$SYSTEM_RESERVED_STORAGE\"" >> "$patch_file"
  fi
  if [[ -n "$MAX_PODS" ]]; then
    echo "maxPods: $MAX_PODS" >> "$patch_file"
  fi

  if [[ -f "$patch_file" ]]; then
    echo "Successfully created kubelet patch file: $patch_file"
    echo "Patch contents:"
    cat "$patch_file"
  else
    error "Failed to create patch file: $patch_file"
  fi
}

# Main function to orchestrate the script
main() {
  validate_input
  ensure_kubeadm_patches_directory
  create_kubelet_patch
}

# Standard boilerplate to run main() only if script is executed directly.
# This allows sourcing this script for testing purposes without executing the main function.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    main "$@"
fi
