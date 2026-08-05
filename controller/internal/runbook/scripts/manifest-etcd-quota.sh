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

get_quota_backend_bytes() {
    local manifest="${ETCD_MANIFEST_PATH:-/etc/kubernetes/manifests/etcd.yaml}"

    local quota
    quota=$(grep -oP '(?<=--quota-backend-bytes=)[0-9]+' "$manifest" || true)

    if [[ -z "${quota:-}" ]]; then
        return 1
    fi

    echo "$quota"
}

main() {
    get_quota_backend_bytes
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    main "$@"
fi
