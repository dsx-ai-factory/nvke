#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

out_dir="${1:?usage: build-arm64.sh OUT_DIR}"
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

mkdir -p "${out_dir}"
out_dir="$(cd "${out_dir}" && pwd)"

# The manifests contain no executable architecture-specific content. Build
# only that Debian package as Architecture: all. Runtime container images are
# resolved for ARM64 by the cluster's registry path, not baked into this image.
make -C "${script_dir}" content-debs OUT_DIR="${out_dir}"
