#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0


export NEEDRESTART_MODE=l
set -e

# Script to upgrade kubeadm and kubelet on a Kubernetes node
# This script follows a clear 4-phase approach:
# 1. Update package repositories and setup
# 2. Upgrade Kubernetes control plane (kubeadm)
# 3. Update kubelet and kubectl packages
# 4. Restart kubelet service

log() {
    echo "$(date '+%Y-%m-%d %H:%M:%S') - $1"
}

error() {
    echo "ERROR: $1" >&2
    exit 1
}

# Default values
NODE_NAME="${NODE_NAME:-}"
IS_MASTER="${IS_MASTER:-false}"
DRAIN_NODE="${DRAIN_NODE:-false}"
CORDON_NODE="${CORDON_NODE:-false}"
UNCORDON_NODE="${UNCORDON_NODE:-false}"
VERSION="${VERSION:-}"
KUBEADM_PATCHES_DIR="${KUBEADM_PATCHES_DIR:-"/etc/kubernetes/patches"}"
KUBEADM_UPGRADE_ONLY="${KUBEADM_UPGRADE_ONLY:-false}"

# Validate required arguments
if [ -z "$VERSION" ]; then
    error "Version is required"
fi

# Extract version components
if [[ ! $VERSION =~ ^([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
    error "Invalid version format. Expected format: X.Y.Z (e.g., 1.29.10), got: $VERSION"
fi

if [ -z "$KUBEADM_PATCHES_DIR" ]; then
    error "KUBEADM_PATCHES_DIR is an empty string - cannot create directory"
fi

MAJOR="${BASH_REMATCH[1]}"
MINOR="${BASH_REMATCH[2]}"
PATCH="${BASH_REMATCH[3]}"
PKG_VERSION_STRING="$MAJOR.$MINOR"
UPDATE_STRING="$MAJOR.$MINOR.$PATCH-*"
GIT_VERSION="v$VERSION"

# Check if kubelet is already at the desired version (skip if only upgrading Kubernetes)
if [ "$KUBEADM_UPGRADE_ONLY" != true ]; then
    if kubelet --version | grep -q "$VERSION"; then
        log "Kubelet is already at version $VERSION"
        exit 0
    fi
fi

# Ensure the kubeadm patches directory exists
ensure_kubeadm_patches_directory() {
    if [ ! -d "$KUBEADM_PATCHES_DIR" ]; then
        log "Creating Kubeadm Patches dir $KUBEADM_PATCHES_DIR"
        mkdir -p "$KUBEADM_PATCHES_DIR" || error "Failed to create $KUBEADM_PATCHES_DIR directory"
    fi
}

# Cordon node if requested
cordon_node() {
    if [ "$CORDON_NODE" = true ]; then
        if [ -z "$NODE_NAME" ]; then
            error "Node name is required for cordon operation"
        fi

        log "Cordoning node: $NODE_NAME"
        if ! kubectl cordon "$NODE_NAME"; then
            error "Failed to cordon node: $NODE_NAME"
        fi

        log "Node cordoned successfully"
    fi
}

# Drain node if requested
drain_node() {
    if [ "$DRAIN_NODE" = true ]; then
        if [ -z "$NODE_NAME" ]; then
            error "Node name is required for drain operation"
        fi

        log "Draining node: $NODE_NAME"
        if ! kubectl drain "$NODE_NAME" --ignore-daemonsets --grace-period=60 --timeout=100s; then
            error "Failed to drain node: $NODE_NAME"
        fi

        log "Labeling node as drained"
        kubectl label node "$NODE_NAME" "dgx.nvidia.com/drained=true" --overwrite

        log "Node drained successfully"
    fi
}

# Phase 1: Update package repositories and setup
update_packages() {
    log "=== Phase 1: Updating package repositories ==="
    
    log "Unholding kubeadm package"
    apt-mark unhold kubeadm || error "Failed to unhold kubeadm"

    # Remove existing keys and sources
    log "Removing existing Kubernetes GPG keys and sources"
    rm -f /etc/apt/keyrings/kubernetes-apt-keyring.gpg
    rm -f /etc/apt/sources.list.d/kubernetes.list

    log "Fetching GPG key for Kubernetes version $PKG_VERSION_STRING"
    curl -fsSL "https://pkgs.k8s.io/core:/stable:/v$PKG_VERSION_STRING/deb/Release.key" | gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg || error "Failed to download GPG key"

    # Set proper permissions for the GPG key
    chmod 644 /etc/apt/keyrings/kubernetes-apt-keyring.gpg

    # Add the key to apt keyring
    apt-key add /etc/apt/keyrings/kubernetes-apt-keyring.gpg || error "Failed to add GPG key to apt keyring"

    log "Adding Kubernetes $PKG_VERSION_STRING repository to apt sources"
    echo "deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v$PKG_VERSION_STRING/deb/ /" | tee /etc/apt/sources.list.d/kubernetes.list

    log "Updating Kubernetes repositories"
    apt-get update -o Dir::Etc::sourcelist="sources.list.d/kubernetes.list" -o Dir::Etc::sourceparts="-" -o APT::Get::List-Cleanup="0" -qq || error "Failed to update Kubernetes repositories"

    log "Installing kubeadm version $UPDATE_STRING"
    apt-get install -qqy "kubeadm=$UPDATE_STRING" 1>/dev/null || error "Failed to install kubeadm"

    log "Holding kubeadm package"
    apt-mark hold kubeadm || error "Failed to hold kubeadm"

    log "Phase 1 complete: Package repositories updated and kubeadm installed"
}

# Phase 2: Upgrade Kubernetes control plane
upgrade_kubernetes() {
    log "=== Phase 2: Upgrading Kubernetes control plane ==="

    if [ "$IS_MASTER" = true ]; then
        log "Running kubeadm upgrade plan for control plane node"
        kubeadm upgrade plan || error "Failed to get upgrade plan"

        log "Applying kubeadm upgrade to version: $GIT_VERSION"
        kubeadm upgrade apply "$GIT_VERSION" --patches="$KUBEADM_PATCHES_DIR" -y || error "Failed to upgrade Kubernetes control plane"
        
        log "Control plane upgrade complete"
    else
        log "Running kubeadm upgrade node for worker node"
        kubeadm upgrade node --patches="$KUBEADM_PATCHES_DIR" || error "Failed to upgrade node"
        
        log "Worker node upgrade complete"
    fi

    log "Phase 2 complete: Kubernetes control plane upgraded"
}

# Phase 3: Update kubelet and kubectl packages
update_kubelet_packages() {
    log "=== Phase 3: Updating kubelet and kubectl packages ==="

    log "Unholding kubelet and kubectl packages"
    apt-mark unhold kubelet kubectl || error "Failed to unhold kubelet and kubectl"

    log "Updating Kubernetes repositories"
    apt-get update -o Dir::Etc::sourcelist="sources.list.d/kubernetes.list" -o Dir::Etc::sourceparts="-" -o APT::Get::List-Cleanup="0" -qq || error "Failed to update Kubernetes repositories"

    log "Installing kubelet and kubectl version $UPDATE_STRING"
    apt-get install -qqy "kubelet=$UPDATE_STRING" "kubectl=$UPDATE_STRING" || error "Failed to install kubelet and kubectl"

    log "Holding kubelet and kubectl packages"
    apt-mark hold kubelet kubectl || error "Failed to hold kubelet and kubectl"

    log "Phase 3 complete: Kubelet and kubectl packages updated"
}

# Phase 4: Restart kubelet service
restart_kubelet() {
    log "=== Phase 4: Restarting kubelet service ==="

    log "Reloading systemd daemon"
    systemctl daemon-reload || error "Failed to reload systemd"

    log "Restarting kubelet service"
    systemctl restart kubelet || error "Failed to restart kubelet"

    log "Waiting for kubelet to stabilize..."
    sleep 5

    log "Checking kubelet status"
    if systemctl is-active --quiet kubelet; then
        log "Kubelet is running"
    else
        error "Kubelet failed to start"
    fi

    log "Phase 4 complete: Kubelet service restarted"
}

# Uncordon node if requested
uncordon_node() {
    if [ "$UNCORDON_NODE" = true ]; then
        if [ -z "$NODE_NAME" ]; then
            error "Node name is required for uncordon operation"
        fi

        log "Uncordoning node: $NODE_NAME"
        if ! kubectl uncordon "$NODE_NAME"; then
            error "Failed to uncordon node: $NODE_NAME"
        fi

        log "Node uncordoned successfully"
    fi
}

# Verify upgrade success
verify_upgrade() {
    log "=== Verifying upgrade ==="
    
    if [ "$KUBEADM_UPGRADE_ONLY" = true ]; then
        log "✓ Kubeadm upgrade completed successfully!"
        return 0
    fi
    
    if kubelet --version | grep -q "$VERSION"; then
        log "✓ Kubelet version verified: $(kubelet --version)"
        log "✓ Node upgrade completed successfully!"
    else
        error "Kubelet version mismatch after upgrade! Expected: $VERSION, got: $(kubelet --version)"
    fi
}

# ============================================================================
# Main execution
# ============================================================================

log "=========================================="
log "Starting Kubernetes node upgrade to version $VERSION"
log "Current kubelet version: $(kubelet --version)"
log "Node type: $([ "$IS_MASTER" = true ] && echo "Control Plane" || echo "Worker")"
log "Kubeadm upgrade only: $KUBEADM_UPGRADE_ONLY"
log "=========================================="

# Pre-upgrade steps
ensure_kubeadm_patches_directory
cordon_node
drain_node

# Execute the upgrade based on mode
if [ "$KUBEADM_UPGRADE_ONLY" = true ]; then
    log "Running in KUBEADM_UPGRADE_ONLY mode - will skip package downloads, kubelet updates and restart"
    log "Assuming kubeadm is already at correct version from OS profile"
    # Phase 2 only: kubeadm upgrade apply (no package downloads)
    upgrade_kubernetes
else
    log "Running full upgrade - all phases will be executed"
    # All 4 phases: complete upgrade (package downloads + kubeadm + kubelet)
    update_packages
    upgrade_kubernetes
    update_kubelet_packages
    restart_kubelet
fi

# Post-upgrade steps
uncordon_node
verify_upgrade

log "=========================================="
log "Upgrade process completed successfully!"
log "=========================================="
