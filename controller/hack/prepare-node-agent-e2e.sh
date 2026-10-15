#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Build real agent releases in a private fixture namespace. The failed target
# contains a real binary with the wrong version, so verification must restore it.
set -euo pipefail
repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
output_directory="$1"
architecture="$2"
case "${architecture}" in
  amd64) bundle_architecture=x86_64 ;;
  arm64) bundle_architecture=aarch64 ;;
  *) printf 'unsupported architecture: %s\n' "${architecture}" >&2; exit 1 ;;
esac
mkdir -p "${output_directory}/agent-e2e"
for version in v0.1.991 v0.1.992 v0.1.993; do
  CGO_ENABLED=0 make -C "${repository_root}/agent" "package-${architecture}" VERSION="${version}"
  cp "${repository_root}/agent/release/agent_${version}_${architecture}.tgz" \
    "${output_directory}/agent-e2e/agent_${version}.tgz"
done
cp "${output_directory}/agent-e2e/agent_v0.1.993.tgz" "${output_directory}/agent-e2e/repaired.tgz"
cp "${output_directory}/agent-e2e/agent_v0.1.992.tgz" "${output_directory}/agent-e2e/agent_v0.1.993.tgz"
jq --arg architecture "${bundle_architecture}" \
  '.agent = {entries: ["0.1.991", "0.1.992", "0.1.993"] | map({version: ., arch: $architecture, path: ("agent-e2e/agent_v" + . + ".tgz")})}' \
  "${output_directory}/index.json" >"${output_directory}/index.next.json"
mv "${output_directory}/index.next.json" "${output_directory}/index.json"
