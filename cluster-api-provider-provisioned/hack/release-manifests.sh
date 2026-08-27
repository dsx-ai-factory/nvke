#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Render the two artifacts that clusterctl expects in a provider release.

set -Eeuo pipefail

package_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
release_dir="${RELEASE_DIR:-${package_root}/out}"
controller_image="${CONTROLLER_IMG:-docker.io/library/cluster-api-provider-provisioned:latest}"
default_image="docker.io/library/cluster-api-provider-provisioned:latest"

mkdir -p "${release_dir}"

kubectl kustomize "${package_root}/config/default" |
  sed "s|image: ${default_image}|image: ${controller_image}|" \
    >"${release_dir}/infrastructure-components.yaml"
cp "${package_root}/metadata.yaml" "${release_dir}/metadata.yaml"

if [[ ! -s "${release_dir}/infrastructure-components.yaml" ]]; then
  echo "infrastructure-components.yaml is empty" >&2
  exit 1
fi

printf 'Generated clusterctl artifacts in %s\n' "${release_dir}"
