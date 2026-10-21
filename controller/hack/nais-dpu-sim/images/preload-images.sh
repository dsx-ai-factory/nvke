#!/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
set -eu

image_dir=/var/lib/nvke-nais-dpu-sim
ctr -n k8s.io images import --no-unpack "$image_dir/images.tar"
ctr -n k8s.io images import --no-unpack --digests \
    --base-name ghcr.io/k8snetworkplumbingwg/multus-cni "$image_dir/multus-oci.tar"
# Preserve the registry digest instead of the OCI archive's outer index.
ctr -n k8s.io images tag --force ghcr.io/k8snetworkplumbingwg/multus-cni:multus \
    "$(cat "$image_dir/multus-image")"
