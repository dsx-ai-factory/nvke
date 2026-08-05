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


set -euo pipefail

# Runbook to update Kubernetes version field in kubeadm ConfigMap
# Reads ConfigMap, updates version, and applies changes

error() {
    echo "ERROR: $1" >&2
    exit 1
}

# Set KUBECONFIG
export KUBECONFIG="${KUBECONFIG:-"/etc/kubernetes/admin.conf"}"

# Validate KUBECONFIG exists
if [ ! -f "$KUBECONFIG" ]; then
    error "KUBECONFIG file not found at $KUBECONFIG"
fi

# Get parameters
KUBERNETES_VERSION="${KUBERNETES_VERSION:-}"
CONFIGMAP_NAME="${CONFIGMAP_NAME:-kubeadm-config}"
NAMESPACE="${NAMESPACE:-kube-system}"

if [ -z "$KUBERNETES_VERSION" ]; then
    error "KUBERNETES_VERSION environment variable is required (e.g., 1.30.14 or v1.30.14)"
fi

# Normalize version (remove 'v' prefix if present)
KUBERNETES_VERSION="${KUBERNETES_VERSION#v}"

# Validate version format (X.Y.Z)
if [[ ! $KUBERNETES_VERSION =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    error "Invalid KUBERNETES_VERSION format. Expected X.Y.Z (e.g., 1.30.14), got: $KUBERNETES_VERSION"
fi

# Prepare version with 'v' prefix for kubeadm ConfigMap
VERSION_WITH_V="v${KUBERNETES_VERSION}"

# Setup backup paths (always create backups)
BACKUP_DIR="/tmp/kubeadm-configmap-backups"
TIMESTAMP=$(date +%Y%m%d_%H%M%S)
BACKUP_FILE="$BACKUP_DIR/kubeadm-config-$TIMESTAMP.yaml"
UPDATED_FILE="$BACKUP_DIR/kubeadm-config-updated-$TIMESTAMP.yaml"

mkdir -p "$BACKUP_DIR" || error "Failed to create backup directory"

# Step 1: Read current kubeadm ConfigMap as JSON and save as backup
kubectl get configmap "$CONFIGMAP_NAME" -n "$NAMESPACE" -o json > "$BACKUP_FILE" 2>/dev/null || \
    error "Failed to get ConfigMap: $CONFIGMAP_NAME in namespace: $NAMESPACE"

# Step 2: Extract current Kubernetes version from the YAML string inside JSON
# Note: ClusterConfiguration is stored as a YAML string, not a JSON object
CURRENT_VERSION=$(jq -r '.data.ClusterConfiguration' "$BACKUP_FILE" 2>/dev/null | \
    sed -n 's/^kubernetesVersion: *v*\([0-9.]*\).*/\1/p' || echo "unknown")

# Check if already at target version
if [ "$CURRENT_VERSION" = "$KUBERNETES_VERSION" ]; then
    echo "ALREADY_UP_TO_DATE"
    echo "Current version: v$CURRENT_VERSION"
    exit 0
fi

# Step 3: Update the kubernetesVersion field
# Extract the ClusterConfiguration YAML, update it, and put it back into JSON
CLUSTER_CONFIG=$(jq -r '.data.ClusterConfiguration' "$BACKUP_FILE")
UPDATED_CLUSTER_CONFIG=$(echo "$CLUSTER_CONFIG" | sed "s/kubernetesVersion: v*[0-9]\+\.[0-9]\+\.[0-9]\+/kubernetesVersion: $VERSION_WITH_V/g")

# Update the JSON with the modified ClusterConfiguration
jq --arg config "$UPDATED_CLUSTER_CONFIG" '.data.ClusterConfiguration = $config' "$BACKUP_FILE" > "$UPDATED_FILE" 2>/dev/null || \
    error "Failed to update kubernetesVersion with jq"

# Step 4: Apply the updated ConfigMap
KUBECTL_ERROR=$(kubectl apply -f "$UPDATED_FILE" 2>&1)
if [ $? -ne 0 ]; then
    error "Failed to apply updated ConfigMap: $KUBECTL_ERROR"
fi

# Step 5: Verify the update
UPDATED_VERSION=$(kubectl get configmap "$CONFIGMAP_NAME" -n "$NAMESPACE" -o jsonpath='{.data.ClusterConfiguration}' 2>/dev/null | \
    sed -n 's/^kubernetesVersion: *v*\([0-9.]*\).*/\1/p' || echo "unknown")

if [ "$UPDATED_VERSION" != "$KUBERNETES_VERSION" ]; then
    error "Verification failed. Expected version $KUBERNETES_VERSION but found $UPDATED_VERSION"
fi

# Step 6: Output success
echo "SUCCESS"
echo "Previous: v$CURRENT_VERSION"
echo "Updated: v$UPDATED_VERSION"
echo "Backup: $BACKUP_FILE"
echo "Applied: $UPDATED_FILE"
