#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/../.." && pwd)"

image_url="${NICO_VMS_OS_IMAGE_URL:-}"
image_destination="${NICO_VMS_OS_IMAGE_DESTINATION:-images/nvke-node.qcow2}"
files_dir="${NICO_SIM_FILES_DIR:-${REPO_ROOT}/controller/nico-sim/.dev/files}"
cache_dir="${NICO_VMS_IMAGE_CACHE_DIR:-${HOME}/.cache/nico-sim/images}"
pull_nvkr_release="${NICO_VMS_PULL_NVKR_RELEASE:-false}"

if [[ "${pull_nvkr_release}" == "true" ]]; then
  image_source="$(bash "${SCRIPT_DIR}/pull-nvkr-os-image.sh")"
  image_url="file://${image_source}"
elif [[ "${pull_nvkr_release}" != "false" ]]; then
  echo "NICO_VMS_PULL_NVKR_RELEASE must be true or false: ${pull_nvkr_release}" >&2
  exit 1
elif [[ -z "${image_url}" ]]; then
  echo "Set NICO_VMS_OS_IMAGE_URL or NICO_VMS_PULL_NVKR_RELEASE=true for a nico-vms node image." >&2
  exit 1
fi
case "${image_url}" in
  https://*|file://*) ;;
  *)
    echo "NICO_VMS_OS_IMAGE_URL must use https:// or file://: ${image_url}" >&2
    exit 1
    ;;
esac
case "${image_destination}" in
  ""|/*|.|./*|..|../*|*/../*|*/..|*/./*|*/.)
    echo "NICO_VMS_OS_IMAGE_DESTINATION must be a relative path below the nico-sim files root: ${image_destination}" >&2
    exit 1
    ;;
esac

if [[ "${image_url}" == file://* ]]; then
  image_source="${image_url#file://}"
  if [[ ! -f "${image_source}" ]]; then
    echo "NICO_VMS_OS_IMAGE_URL does not name a file: ${image_url}" >&2
    exit 1
  fi
fi

image_path="${files_dir}/${image_destination}"
image_dir="$(dirname "${image_path}")"

mkdir -p "${image_dir}"
tmp_path="${image_path}.tmp.$$"

cleanup() {
  rm -f "${tmp_path}"
}
trap cleanup EXIT

echo "Staging ${image_url}" >&2
curl --fail --location --proto '=https,file' --proto-redir '=https' \
  --retry 3 --retry-all-errors --show-error --silent --output "${tmp_path}" "${image_url}"
if ! qemu-img info --output=json "${tmp_path}" | grep -Eq '"format"[[:space:]]*:[[:space:]]*"qcow2"'; then
  echo "Downloaded nico-vms OS image is not a valid QCOW2 image: ${image_url}" >&2
  exit 1
fi
mv "${tmp_path}" "${image_path}"

files_url_path="/files/${image_destination}"

# nico-sim intentionally keys localhost downloads by URL path, so changing the
# artifact behind this stable /files URL requires invalidating that cache entry.
if command -v sha256sum >/dev/null 2>&1; then
  cache_key="$(printf '%s' "${files_url_path}" | sha256sum | cut -c1-32)"
else
  cache_key="$(printf '%s' "${files_url_path}" | shasum -a 256 | cut -c1-32)"
fi
rm -f "${cache_dir}/${cache_key}.qcow2"

echo "Staged ${image_url} at ${image_path}" >&2
echo "nico-sim serves this image at ${files_url_path}" >&2
