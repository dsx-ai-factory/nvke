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

# Purpose: Apply (key=value:effect) or remove (key=value:effect-) specified node taint. Operates on one or more
# nodes specified in NODE_NAMES input.
# Prerequisites:
# - Must be run on a control plane node with admin kubeconfig access
# - Requires two arguments:
#   1. Node name
#   2. Taint to apply in key=value:effect format

set -euo pipefail

export KUBECONFIG="${KUBECONFIG:-"/etc/kubernetes/admin.conf"}"
export NODE_NAMES="${NODE_NAMES:-}"
export TAINT_KEY="${TAINT_KEY:-}"
export TAINT_VALUE="${TAINT_VALUE:-}"
export TAINT_EFFECT="${TAINT_EFFECT:-}"

error() {
    echo "Error: $1" >&2
    exit 1
}

# Validate input or environment variables
validate_input() {
    local missing_vars=()
    
    [[ -z "$NODE_NAMES" ]] && missing_vars+=("NODE_NAMES")
    [[ -z "$TAINT_KEY" ]] && missing_vars+=("TAINT_KEY")
    [[ -z "$TAINT_VALUE" ]] && missing_vars+=("TAINT_VALUE")
    [[ -z "$TAINT_EFFECT" ]] && missing_vars+=("TAINT_EFFECT")

    if [[ ${#missing_vars[@]} -gt 0 ]]; then
        error "Missing environment variables: ${missing_vars[*]}"
    fi

    if [[ ! -f "$KUBECONFIG" ]]; then
        error "Admin kubeconfig $KUBECONFIG does not exist, is this running on a control plane node?"
    fi
}

# Apply taint to nodes
apply_taint() {
    # Convert comma-delimited strings to arrays
    IFS=',' read -ra node_array <<< "$NODE_NAMES"
    if [[ ${#node_array[@]} -eq 0 ]]; then
        echo "No nodes specified"
        return 0
    fi

    # Apply taint to each node
    for node in "${node_array[@]}"; do
        
        # Build the taint arguments for kubectl
        taint_args=("$TAINT_KEY=$TAINT_VALUE:$TAINT_EFFECT")

        # Remove Taint when effect ends with hyphen
        if [[ "$TAINT_EFFECT" == *"-" ]]; then
            echo "Attempting to remove taint $taint_args from node $node."
            
            if ! output=$(kubectl taint node "$node" "$taint_args" 2>&1); then
                if echo "$output" | grep -qi "taint.*not found"; then
                    echo "Considered successful. 'taint not found' error for $node"
                else
                    error "Failed to remove taint from node $node: $output"
                fi
            else
                echo "Taint successfully removed"
            fi
        # Apply Taint
        else
            echo "Applying taint $taint_args to node $node"
            if ! kubectl taint node "$node" "$taint_args" --overwrite; then
                error "Failed to apply node taint $node"
            fi
            echo "Taint successfully applied"
        fi
        printf "\n"
    done
    printf "\n"

    echo "Successfully processed all specified nodes"
}

# Main function to orchestrate the script
main() {
    validate_input
    apply_taint
}

# Run main only if script is executed directly
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    main
fi
