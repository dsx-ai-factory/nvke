#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
# Use the ctr supplied by the node runtime.
export NKE_CTR_BIN="$(command -v ctr)"
# KIND supplies a running containerd binary, outside dpkg.
containerd --version >/dev/null
dpkg --ignore-depends=containerd,nvke-kubeadm-distro -i ./nvke-cni-ovn-*.deb
