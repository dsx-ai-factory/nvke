#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Drops every ovnkube-node container's resources so the pods run BestEffort on
# the small e2e VMs. Resources, not only requests: when a chart sets limits,
# a pod without requests gets them defaulted to the limits, which the VMs
# cannot fit, and the pods never schedule again.
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

# One "add" per container: it creates or replaces the member, and an unchanged
# value is a no-op, so the later control-plane joins do not roll the pods again.
ops=""
for field in containers initContainers; do
  i=0
  for name in $(kubectl get ds "$DS_NAME" -n "$NAMESPACE" -o jsonpath="{.spec.template.spec.${field}[*].name}" 2>/dev/null); do
    echo "Dropping resources from ${field}: ${name}"
    ops+="${ops:+,}{\"op\": \"add\", \"path\": \"/spec/template/spec/${field}/${i}/resources\", \"value\": {}}"
    i=$((i + 1))
  done
done

kubectl patch ds "$DS_NAME" -n "$NAMESPACE" --type=json -p "[${ops}]"
echo "Done."
