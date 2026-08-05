#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# Shared helper for building the custom kindest node image used by local CAPD
# and NICo flows. The Dockerfile remains the source of truth for image-content
# defaults like KINDEST_NODE_TAG and NODE_AGENT_VERSION. This helper selects the
# image platform and matching NKE bundle architecture for the Docker daemon, and
# can package a sibling node-agent checkout into the image for local CAPD tests.

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"

: "${CAPD_KINDEST_NODE_IMAGE:=cwe-capd-kindest-node:local}"
: "${CAPD_KINDEST_NODE_TAG:=v1.34.8}"
: "${CAPD_NODE_AGENT_LOCAL_BUILD:=auto}"
: "${CAPD_SPIRE_ATTESTATION_METHOD:=tpm}"
: "${NODE_AGENT_REPO_PATH:=${repo_root}/../node-agent}"
: "${BUNDLE_SERVER_URL:=https://d3mq564cn7sth7.cloudfront.net/}"
: "${INDEX_FILE_URL:=${BUNDLE_SERVER_URL%/}/nke_bundles/Ubuntu_24.04/index.json}"

if [[ "$(uname -s)" == "Darwin" && -z "${DOCKER_HOST:-}" ]]; then
  : "${COLIMA_DOCKER_HOST:=unix://${HOME}/.colima/default/docker.sock}"
  export DOCKER_HOST="${COLIMA_DOCKER_HOST}"
fi

detect_default_platform() {
  local arch
  arch="$(docker version --format '{{.Server.Arch}}' 2>/dev/null || true)"
  if [[ -z "${arch}" ]]; then
    arch="$(uname -m)"
  fi

  case "${arch}" in
    amd64 | x86_64)
      printf 'linux/amd64\n'
      ;;
    arm64 | aarch64)
      printf 'linux/arm64\n'
      ;;
    *)
      printf 'unsupported\n'
      ;;
  esac
}

default_platform="$(detect_default_platform)"
if [[ "${default_platform}" == "unsupported" ]]; then
  echo "Unsupported Docker architecture; set CAPD_KINDEST_NODE_PLATFORM explicitly." >&2
  exit 1
fi

: "${CAPD_KINDEST_NODE_PLATFORM:=${default_platform}}"
effective_platform="${CAPD_KINDEST_NODE_PLATFORM}"

case "${effective_platform}" in
  linux/amd64* | amd64 | x86_64)
    : "${NODE_AGENT_BASE_URL:=https://d3mq564cn7sth7.cloudfront.net/nke_bundles/Ubuntu_24.04/x86_64}"
    : "${NODE_AGENT_BUNDLE_ARCH:=x86_64}"
    : "${NODE_AGENT_DIR_ARCH:=amd64}"
    : "${SPIRE_BUNDLES_BASE_URL:=https://d3mq564cn7sth7.cloudfront.net/nke_bundles/Ubuntu_24.04/x86_64}"
    : "${SPIRE_BUNDLE_ARCH:=x86_64}"
    ;;
  linux/arm64* | arm64 | aarch64)
    : "${NODE_AGENT_BASE_URL:=https://d3mq564cn7sth7.cloudfront.net/nke_bundles/Ubuntu_24.04/aarch64}"
    : "${NODE_AGENT_BUNDLE_ARCH:=aarch64}"
    : "${NODE_AGENT_DIR_ARCH:=arm64}"
    : "${SPIRE_BUNDLES_BASE_URL:=https://d3mq564cn7sth7.cloudfront.net/nke_bundles/Ubuntu_24.04/aarch64}"
    : "${SPIRE_BUNDLE_ARCH:=aarch64}"
    ;;
  *)
    echo "Unsupported CAPD_KINDEST_NODE_PLATFORM=${effective_platform}" >&2
    exit 1
    ;;
esac

build_context="$(mktemp -d)"
trap 'rm -rf "${build_context}"' EXIT
cp "${script_dir}/Dockerfile" "${build_context}/Dockerfile"
cp -R "${script_dir}/scripts" "${build_context}/scripts"
cp -R "${script_dir}/systemd" "${build_context}/systemd"

should_build_local_node_agent() {
  case "${CAPD_NODE_AGENT_LOCAL_BUILD}" in
    1 | true | TRUE | yes | YES)
      return 0
      ;;
    0 | false | FALSE | no | NO)
      return 1
      ;;
    auto | AUTO)
      [[ -z "${NODE_AGENT_VERSION:-}" && -f "${NODE_AGENT_REPO_PATH}/Makefile" ]]
      return
      ;;
    *)
      echo "Unsupported CAPD_NODE_AGENT_LOCAL_BUILD=${CAPD_NODE_AGENT_LOCAL_BUILD}; use auto, true, or false." >&2
      exit 1
      ;;
  esac
}

effective_node_agent_version="${NODE_AGENT_VERSION:-v0.1.16}"
if should_build_local_node_agent; then
  if [[ ! -f "${NODE_AGENT_REPO_PATH}/Makefile" ]]; then
    echo "NODE_AGENT_REPO_PATH=${NODE_AGENT_REPO_PATH} does not look like a node-agent checkout." >&2
    exit 1
  fi

  effective_node_agent_version="${NODE_AGENT_VERSION:-$(git -C "${NODE_AGENT_REPO_PATH}" describe --tags --always --dirty)}"
  node_agent_cgo_enabled="${CGO_ENABLED:-0}"
  echo "Building local node-agent ${effective_node_agent_version} for ${NODE_AGENT_DIR_ARCH} from ${NODE_AGENT_REPO_PATH} with CGO_ENABLED=${node_agent_cgo_enabled}..."
  CGO_ENABLED="${node_agent_cgo_enabled}" make -C "${NODE_AGENT_REPO_PATH}" "package-${NODE_AGENT_DIR_ARCH}" VERSION="${effective_node_agent_version}"

  local_node_agent_tgz="${NODE_AGENT_REPO_PATH}/release/agent_${effective_node_agent_version}_${NODE_AGENT_DIR_ARCH}.tgz"
  if [[ ! -f "${local_node_agent_tgz}" ]]; then
    echo "Expected local node-agent bundle was not produced: ${local_node_agent_tgz}" >&2
    exit 1
  fi
  cp "${local_node_agent_tgz}" "${build_context}/node-agent.tgz"
elif [[ -n "${NODE_AGENT_BUNDLE_PATH:-}" ]]; then
  if [[ ! -f "${NODE_AGENT_BUNDLE_PATH}" ]]; then
    echo "NODE_AGENT_BUNDLE_PATH does not exist: ${NODE_AGENT_BUNDLE_PATH}" >&2
    exit 1
  fi
  cp "${NODE_AGENT_BUNDLE_PATH}" "${build_context}/node-agent.tgz"
else
  node_agent_url="${NODE_AGENT_BASE_URL}/node-agent_${effective_node_agent_version}_${NODE_AGENT_BUNDLE_ARCH}.tgz"
  echo "Downloading published node-agent bundle ${node_agent_url}"
  curl -fsSL "${node_agent_url}" -o "${build_context}/node-agent.tgz"
fi

if [[ -n "${SPIRE_AGENT_BUNDLE_PATH:-}" ]]; then
  if [[ ! -f "${SPIRE_AGENT_BUNDLE_PATH}" ]]; then
    echo "SPIRE_AGENT_BUNDLE_PATH does not exist: ${SPIRE_AGENT_BUNDLE_PATH}" >&2
    exit 1
  fi
  cp "${SPIRE_AGENT_BUNDLE_PATH}" "${build_context}/spire-agent.tgz"
else
  spire_bundle_name="spire_agent_v1.14.1_tpm_attestor_v1.9.0_${SPIRE_BUNDLE_ARCH}.tgz"
  spire_bundle_url="${SPIRE_BUNDLES_BASE_URL}/${spire_bundle_name}"
  echo "Downloading published SPIRE agent bundle ${spire_bundle_url}"
  curl -fsSL "${spire_bundle_url}" -o "${build_context}/spire-agent.tgz"
fi

echo "Ensuring kindest/node:${CAPD_KINDEST_NODE_TAG} is available for ${effective_platform}..."
docker pull --platform "${effective_platform}" "kindest/node:${CAPD_KINDEST_NODE_TAG}"

build_args=(
  build
  -f "${build_context}/Dockerfile"
  -t "${CAPD_KINDEST_NODE_IMAGE}"
  --build-arg "CAPD_BASE_PLATFORM=${effective_platform}"
  --build-arg "NODE_AGENT_VERSION=${effective_node_agent_version}"
  --build-arg "NODE_AGENT_DIR_ARCH=${NODE_AGENT_DIR_ARCH}"
  --build-arg "CAPD_SPIRE_ATTESTATION_METHOD=${CAPD_SPIRE_ATTESTATION_METHOD}"
  --build-arg "BUNDLE_SERVER_URL=${BUNDLE_SERVER_URL}"
  --build-arg "INDEX_FILE_URL=${INDEX_FILE_URL}"
)

if [[ -n "${CAPD_KINDEST_NODE_TAG:-}" ]]; then
  build_args+=(--build-arg "KINDEST_NODE_TAG=${CAPD_KINDEST_NODE_TAG}")
fi

if [[ -n "${CAPD_KINDEST_NODE_PLATFORM}" ]]; then
  build_args+=(--platform "${CAPD_KINDEST_NODE_PLATFORM}")
fi

build_args+=("${build_context}")

docker "${build_args[@]}"
