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

# Purpose: Remove control-plane NoSchedule taint from all nodes
# Use case: Control-plane-only clusters where workloads should be able to schedule on control plane nodes
# Prerequisites:
# - Must be run on a control plane node with admin kubeconfig access

set -euo pipefail

export KUBECONFIG="${KUBECONFIG:-/etc/kubernetes/admin.conf}"

error() {
    echo "Error: $1" >&2
    exit 1
}

validate_prerequisites() {
    if [[ ! -f "$KUBECONFIG" ]]; then
        error "Admin kubeconfig $KUBECONFIG does not exist. Must run on a control plane node."
    fi

    if ! command -v kubectl &> /dev/null; then
        error "kubectl not found in PATH"
    fi
}

untaint_control_plane_nodes() {
    echo "Removing node-role.kubernetes.io/control-plane:NoSchedule taint from all nodes..."
    
    # Attempt to remove the taint from all nodes, ignoring errors
    # --all will process all nodes, and kubectl will gracefully handle nodes that don't have the taint
    kubectl taint nodes --all node-role.kubernetes.io/control-plane:NoSchedule- 2>&1 || true
    
    # Verify no nodes still have the taint using go-template
    echo "Verifying taint removal..."
    local nodes_with_taint
    nodes_with_taint=$(kubectl get nodes -o go-template='{{range .items}}{{$node := .metadata.name}}{{range .spec.taints}}{{if and (eq .key "node-role.kubernetes.io/control-plane") (eq .effect "NoSchedule")}}{{$node}}{{"\n"}}{{end}}{{end}}{{end}}')
    
    if [[ -n "$nodes_with_taint" ]]; then
        echo "ERROR: The following nodes still have the control-plane taint:"
        echo "$nodes_with_taint"
        error "Failed to remove taint from all nodes"
    fi
    
    echo "Successfully verified: no nodes have the control-plane:NoSchedule taint"
}

main() {
    validate_prerequisites
    untaint_control_plane_nodes
}

# Run main only if script is executed directly
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    main "$@"
fi

