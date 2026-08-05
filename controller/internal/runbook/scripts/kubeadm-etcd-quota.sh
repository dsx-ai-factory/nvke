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

export KUBECONFIG="${KUBECONFIG:-/etc/kubernetes/admin.conf}"

get_quota() {
    local config quota=""

    config=$(kubectl get cm kubeadm-config -n kube-system -o jsonpath='{.data.ClusterConfiguration}')

    quota=$(echo "$config" | grep -A1 'name: quota-backend-bytes' 2>/dev/null \
        | grep 'value:' | sed 's/.*value:[[:space:]]*//' | tr -d '"' || true)

    if [[ -z "$quota" ]]; then
        quota=$(echo "$config" | grep 'quota-backend-bytes:' 2>/dev/null \
            | sed 's/.*quota-backend-bytes:[[:space:]]*//' | tr -d '"' || true)
    fi

    if [[ -n "$quota" ]]; then
        echo "$quota"
        return 0
    fi

    echo "ERROR: quota-backend-bytes not found"
    return 1
}

main() {
    get_quota
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    main "$@"
fi
