#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

export KUBECONFIG="/etc/kubernetes/admin.conf"

NAMESPACE="${NAMESPACE:-ovn-kubernetes}"
DS_NAME="${DS_NAME:-ovnkube-node}"

# Exit early if the DaemonSet does not exist. This is expected for non-OVN CNIs.
if ! ds_ref=$(kubectl get ds "$DS_NAME" -n "$NAMESPACE" --ignore-not-found -o name 2>&1); then
  echo "$ds_ref" >&2
  exit 1
fi
if [[ -z "$ds_ref" ]]; then
  echo "Skipping remove-ovn-requests: DaemonSet ${NAMESPACE}/${DS_NAME} does not exist. This is expected for non-OVN CNIs."
  exit 0
fi

echo "Patching DaemonSet ${DS_NAME} in namespace ${NAMESPACE}..."

# Get container names from the DaemonSet
containers=$(kubectl get ds "$DS_NAME" -n "$NAMESPACE" -o jsonpath='{.spec.template.spec.containers[*].name}')

for container in $containers; do
  echo "Removing requests from container: ${container}"
  kubectl patch ds "$DS_NAME" -n "$NAMESPACE" --type=json \
    -p "[{\"op\": \"remove\", \"path\": \"/spec/template/spec/containers/$(
      kubectl get ds "$DS_NAME" -n "$NAMESPACE" \
        -o jsonpath="{range .spec.template.spec.containers[*]}{.name}{'\n'}{end}" \
        | grep -n "^${container}$" | cut -d: -f1 | awk '{print $1 - 1}'
    )/resources/requests\"}]" 2>&1 || echo "  No requests to remove (or already removed)"
done

# Handle init containers if present
init_containers=$(kubectl get ds "$DS_NAME" -n "$NAMESPACE" -o jsonpath='{.spec.template.spec.initContainers[*].name}' 2>/dev/null || true)

for container in $init_containers; do
  [ -z "$container" ] && continue
  echo "Removing requests from init container: ${container}"
  kubectl patch ds "$DS_NAME" -n "$NAMESPACE" --type=json \
    -p "[{\"op\": \"remove\", \"path\": \"/spec/template/spec/initContainers/$(
      kubectl get ds "$DS_NAME" -n "$NAMESPACE" \
        -o jsonpath="{range .spec.template.spec.initContainers[*]}{.name}{'\n'}{end}" \
        | grep -n "^${container}$" | cut -d: -f1 | awk '{print $1 - 1}'
    )/resources/requests\"}]" 2>&1 || echo "  No requests to remove (or already removed)"
done

echo "Done."
