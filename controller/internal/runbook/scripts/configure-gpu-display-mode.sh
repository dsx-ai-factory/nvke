#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Purpose: Configures the GPU display mode using the displaymodeselector tool
# Prerequisites:
# - displaymodeselector must be installed in /usr/local/nke/bin
# - System must have NVIDIA GPUs installed

set -euo pipefail

# Environment variables with defaults
DISPLAYMODESELECTOR_VERSION="1.67.0"
GPU_DISPLAY_MODE=${GPU_DISPLAY_MODE:-"compute"}
DISPLAYMODESELECTOR_PATH=${DISPLAYMODESELECTOR_PATH:-"/usr/local/bin/displaymodeselector"}

error() {
    echo "Error: $1" >&2
    exit 1
}

check_dependencies() {
    if [[ ! -x "$DISPLAYMODESELECTOR_PATH" ]]; then
        error "displaymodeselector not found or not executable at $DISPLAYMODESELECTOR_PATH"
    fi
}

create_directories() {
    echo "Creating required directories"
    local dirs=("/home/nke/assets" "/usr/local/nke/bin")
    for dir in "${dirs[@]}"; do
        if [[ ! -d "$dir" ]]; then
            mkdir -p "$dir" || error "Failed to create directory: $dir"
            chmod 0755 "$dir" || error "Failed to set permissions on: $dir"
        fi
    done
}

configure_gpu_mode() {
    local mode="$1"
    echo "Setting GPU display mode to $mode"

    if ! "$DISPLAYMODESELECTOR_PATH" --gpumode "$mode" --auto; then
        error "Failed to set GPU display mode to $mode"
    fi

    echo "GPU display mode configuration completed successfully"
}

main() {
    check_dependencies
    create_directories
    configure_gpu_mode "$GPU_DISPLAY_MODE"
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    main
fi
