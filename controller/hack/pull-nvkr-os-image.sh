#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

release_reference="${NVKR_RELEASE_REFERENCE:-}"
release_output="${NVKR_RELEASE_OUTPUT:-}"
if [[ -z "${release_output}" ]]; then
  case "$(uname -m)" in
    x86_64 | amd64) release_output="nvke-node-fde-amd64" ;;
    aarch64 | arm64) release_output="nvke-node-fde-arm64" ;;
    *)
      echo "Unsupported host architecture for the default NVKR release output: $(uname -m)" >&2
      exit 1
      ;;
  esac
fi
release_timeout="${NVKR_RELEASE_TIMEOUT:-2h}"
pull_cache_dir="${NVKR_RELEASE_CACHE_DIR:-${HOME}/.cache/nico-sim/nvkr-releases}"
nvkr_bin="${NVKR_BIN:-nvkr}"

if [[ -z "${release_reference}" ]]; then
  echo "Set NVKR_RELEASE_REFERENCE to an immutable OCI release reference." >&2
  exit 1
fi
if [[ ! "${release_reference}" =~ @sha256:([0-9a-f]{64})$ ]]; then
  echo "NVKR_RELEASE_REFERENCE must end in @sha256:<64 lowercase hexadecimal characters>." >&2
  exit 1
fi
release_digest="${BASH_REMATCH[1]}"
if [[ ! "${release_output}" =~ ^[A-Za-z0-9._-]+$ ]]; then
  echo "NVKR_RELEASE_OUTPUT contains unsupported characters: ${release_output}" >&2
  exit 1
fi

if [[ "${nvkr_bin}" != */* ]]; then
  nvkr_command="${nvkr_bin}"
  nvkr_bin="$(command -v "${nvkr_command}" 2>/dev/null || true)"
  if [[ -z "${nvkr_bin}" ]]; then
    echo "NVKR_BIN is not available on PATH: ${nvkr_command}" >&2
    exit 1
  fi
elif [[ ! -x "${nvkr_bin}" ]]; then
  echo "NVKR_BIN is not executable: ${nvkr_bin}" >&2
  exit 1
fi

echo "Preparing NVKR release output ${release_output} from ${release_reference}." >&2
pull_dir="${pull_cache_dir}/${release_digest}-${release_output}"
image_path="${pull_dir}/${release_output}.qcow2"

mkdir -p "${pull_cache_dir}"
if [[ ! -s "${image_path}" ]]; then
  tmp_root="$(mktemp -d "${pull_cache_dir}/.${release_digest}-${release_output}.tmp.XXXXXX")"
  tmp_pull_dir="${tmp_root}/output"
  cleanup() {
    rm -rf -- "${tmp_root}"
  }
  trap cleanup EXIT

  echo "Pulling ${release_output} into ${tmp_pull_dir}..." >&2
  "${nvkr_bin}" release pull \
    --reference "${release_reference}" \
    --output "${release_output}" \
    --out "${tmp_pull_dir}" \
    --timeout "${release_timeout}" >&2

  tmp_image_path="${tmp_pull_dir}/${release_output}.qcow2"
  if [[ ! -s "${tmp_image_path}" ]]; then
    echo "NVKR release pull did not produce ${tmp_image_path}." >&2
    exit 1
  fi
  if [[ ! -s "${image_path}" ]]; then
    rm -rf -- "${pull_dir}"
    mv "${tmp_pull_dir}" "${pull_dir}"
  fi
  trap - EXIT
  rm -rf -- "${tmp_root}"
else
  echo "Using cached NVKR release output at ${image_path}." >&2
fi

echo "NVKR release output ${release_output} is ready at ${image_path}." >&2
printf '%s\n' "${image_path}"
