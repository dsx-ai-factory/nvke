#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Shared helper for building the custom kindest node image used by local CAPD
# and NICo flows. The Dockerfile remains the source of truth for image-content
# defaults like KINDEST_NODE_TAG and NODE_AGENT_VERSION. This helper selects the
# image platform and matching NVKE bundle architecture for the container runtime, and
# can package a sibling agent checkout into the image for local CAPD tests.

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"
# shellcheck source=../../../hack/read-tool-version.sh
. "$(cd "${script_dir}/../../.." && pwd)/hack/read-tool-version.sh"

: "${CAPD_KINDEST_NODE_IMAGE:=cwe-capd-kindest-node:local}"
: "${CAPD_KINDEST_NODE_TAG:=v1.34.8}"
: "${CAPD_NODE_AGENT_LOCAL_BUILD:=auto}"
: "${CAPD_SPIRE_ATTESTATION_METHOD:=tpm}"
: "${DSX_IMDS_AGENT_PLUGIN_PATH:=}"
: "${DSX_IDENTITY_CA_BUNDLE_PATH:=}"
: "${CAPD_CNI_MANIFESTS_DEB_PATH:=}"
: "${CAPD_CNI_CHARTS_DEB_PATH:=}"
: "${CAPD_CNI_IMAGES_DEB_PATH:=}"
: "${CAPD_HELM_DEB_PATH:=}"
: "${NKE_E2E_DOCKERHUB_MIRROR:=}"
: "${NODE_AGENT_REPO_PATH:=${repo_root}/../agent}"
# Empty by default: a bundle server is a deployment choice, and the CDN that used
# to be the default cannot be reached from the CI runners. Set BUNDLE_SERVER_URL
# to opt in; the index is derived from it unless given separately.
: "${BUNDLE_SERVER_URL:=}"
if [[ -n "${BUNDLE_SERVER_URL}" ]]; then
  : "${INDEX_FILE_URL:=${BUNDLE_SERVER_URL%/}/nke_bundles/Ubuntu_24.04/index.json}"
fi
: "${INDEX_FILE_URL:=}"

# Where the SPIRE agent bundle comes from.
#   github - assemble it from the upstream public releases (default)
#   bundle - download the pre-assembled bundle from BUNDLE_SERVER_URL
# Downstream builds that want the internal bundle set SPIRE_AGENT_SOURCE=bundle.
: "${SPIRE_AGENT_SOURCE:=github}"
# The same two upstream releases the node .deb wraps and that
# hack/ci/prepare-capd-bundles.sh assembles the CAPD bundle from: the image
# built here has to attest against the versions the nodes actually run. Read
# from make/tool-versions.mk rather than restated, so bumping it there reaches
# all three. Assigning unconditionally first means a missing pin fails loudly
# here instead of defaulting a download to an empty version.
spire_version_pin="$(read_tool_version SPIRE_VERSION)"
tpm_plugin_version_pin="$(read_tool_version TPM_PLUGIN_VERSION)"
: "${SPIRE_AGENT_VERSION:=${spire_version_pin}}"
: "${TPM_ATTESTOR_VERSION:=${tpm_plugin_version_pin}}"
: "${SPIRE_RELEASE_BASE_URL:=https://github.com/spiffe/spire/releases/download}"
: "${TPM_ATTESTOR_RELEASE_BASE_URL:=https://github.com/spiffe/spire-tpm-plugin/releases/download}"
# Retry budget for every artifact download.
: "${DOWNLOAD_RETRIES:=5}"
: "${DOWNLOAD_RETRY_DELAY:=3}"
# The container CLI to build through. Docker by default, for the consumers
# that run on it (controller/Makefile.dev, controller/hack/ci/backend-e2e.sh).
# cli/e2e/bootstrap passes nerdctl, because that suite has no Docker daemon
# and the image has to land in the containerd store nvkectl loads from.
: "${CONTAINER_CLI:=docker}"

if [[ "$(uname -s)" == "Darwin" && -z "${DOCKER_HOST:-}" ]]; then
  : "${COLIMA_DOCKER_HOST:=unix://${HOME}/.colima/default/docker.sock}"
  export DOCKER_HOST="${COLIMA_DOCKER_HOST}"
fi

# Download with retry and exponential backoff.
#
# NOTE: deliberately no `--remove-on-error`. That option needs curl >= 7.83.0
# and Ubuntu 22.04 ships 7.81.0, where the whole command fails with
# "option --remove-on-error: is unknown". We remove the partial file ourselves,
# which works on every curl.
fetch_url() {
  local url="$1" dest="$2" attempt=1 delay="${DOWNLOAD_RETRY_DELAY}"
  while :; do
    if curl -fsSL "${url}" -o "${dest}"; then
      return 0
    fi
    rm -f "${dest}"
    if (( attempt >= DOWNLOAD_RETRIES )); then
      echo "failed to download after ${DOWNLOAD_RETRIES} attempts: ${url}" >&2
      return 1
    fi
    echo "  download attempt ${attempt}/${DOWNLOAD_RETRIES} failed, retrying in ${delay}s: ${url}" >&2
    sleep "${delay}"
    attempt=$(( attempt + 1 ))
    delay=$(( delay * 2 ))
  done
}

# Build the SPIRE agent bundle from the upstream public releases, producing the
# same layout the internal bundle has and the Dockerfile expects:
#
#   <root>/install.sh  <root>/spire-agent.service  <root>/spire-agent.env
#   <root>/bin/spire-agent  <root>/bin/tpm_attestor_agent
assemble_spire_bundle_from_upstream() {
  local out="$1"
  local spire_arch tpm_arch
  case "${SPIRE_BUNDLE_ARCH}" in
    x86_64)  spire_arch="amd64"; tpm_arch="amd64" ;;
    aarch64) spire_arch="arm64"; tpm_arch="arm64" ;;
    *) echo "unsupported SPIRE bundle arch: ${SPIRE_BUNDLE_ARCH}" >&2; return 1 ;;
  esac

  local work; work="$(mktemp -d)"
  # shellcheck disable=SC2064
  trap "rm -rf '${work}'" RETURN

  local spire_url="${SPIRE_RELEASE_BASE_URL}/v${SPIRE_AGENT_VERSION}/spire-${SPIRE_AGENT_VERSION}-linux-${spire_arch}-musl.tar.gz"
  local tpm_url="${TPM_ATTESTOR_RELEASE_BASE_URL}/v${TPM_ATTESTOR_VERSION}/spire_tpm_plugin_tpm_attestor_agent_linux_${tpm_arch}_v${TPM_ATTESTOR_VERSION}.tar.gz"

  echo "Assembling SPIRE agent bundle from upstream releases"
  echo "  spire-agent        ${spire_url}"
  echo "  tpm_attestor_agent ${tpm_url}"
  fetch_url "${spire_url}" "${work}/spire.tar.gz"
  fetch_url "${tpm_url}"   "${work}/tpm.tar.gz"

  # Same top-level directory name the internal bundle uses; the Dockerfile
  # extracts with --strip-components=1, so there must be exactly one.
  local root="${work}/spire_agent_v${SPIRE_AGENT_VERSION}_tpm_attestor_v${TPM_ATTESTOR_VERSION}"
  mkdir -p "${root}/bin"
  tar -xzf "${work}/spire.tar.gz" -C "${work}"
  cp "${work}/spire-${SPIRE_AGENT_VERSION}/bin/spire-agent" "${root}/bin/spire-agent"
  tar -xzf "${work}/tpm.tar.gz" -C "${work}"
  cp "$(find "${work}" -maxdepth 2 -name tpm_attestor_agent -type f | head -1)" \
     "${root}/bin/tpm_attestor_agent"
  chmod 0755 "${root}/bin/spire-agent" "${root}/bin/tpm_attestor_agent"

  cp "${repo_root}/hack/ci/assets/spire-agent-install.sh" "${root}/install.sh"
  cp "${repo_root}/hack/ci/assets/spire-agent.service" "${root}/spire-agent.service"
  chmod 0755 "${root}/install.sh"

  # The agent config resolves ${TPM_PLUGIN_CHECKSUM}; SPIRE verifies the plugin
  # against it at load time. Compute it from the binary we just fetched rather
  # than hardcoding a value that would silently drift on a version bump.
  local sum
  sum="$(sha256sum "${root}/bin/tpm_attestor_agent" | cut -d' ' -f1)"
  printf 'TPM_PLUGIN_CHECKSUM=%s\n' "${sum}" > "${root}/spire-agent.env"
  echo "  tpm plugin checksum ${sum}"

  tar -czf "${out}" -C "${work}" "$(basename "${root}")"
}

detect_default_platform() {
  local arch
  arch="$("${CONTAINER_CLI}" version --format '{{.Server.Arch}}' 2>/dev/null || true)"
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
  echo "Unsupported ${CONTAINER_CLI} architecture; set CAPD_KINDEST_NODE_PLATFORM explicitly." >&2
  exit 1
fi

: "${CAPD_KINDEST_NODE_PLATFORM:=${default_platform}}"
effective_platform="${CAPD_KINDEST_NODE_PLATFORM}"

case "${effective_platform}" in
  linux/amd64* | amd64 | x86_64)
    : "${NODE_AGENT_BUNDLE_ARCH:=x86_64}"
    : "${NODE_AGENT_DIR_ARCH:=amd64}"
    : "${SPIRE_BUNDLE_ARCH:=x86_64}"
    ;;
  linux/arm64* | arm64 | aarch64)
    : "${NODE_AGENT_BUNDLE_ARCH:=aarch64}"
    : "${NODE_AGENT_DIR_ARCH:=arm64}"
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

# Docker COPY requires the source to exist for every attestation mode. The
# empty placeholder is never installed. A supplied plugin is installed even in
# TPM image mode so one local image can bootstrap both old and new nodes.
: > "${build_context}/dsx-imds-node-attestor-agent"
if [[ -n "${DSX_IMDS_AGENT_PLUGIN_PATH}" ]]; then
  if [[ ! -x "${DSX_IMDS_AGENT_PLUGIN_PATH}" ]]; then
    echo "DSX_IMDS_AGENT_PLUGIN_PATH must name an executable plugin" >&2
    exit 1
  fi
  cp "${DSX_IMDS_AGENT_PLUGIN_PATH}" "${build_context}/dsx-imds-node-attestor-agent"
elif [[ "${CAPD_SPIRE_ATTESTATION_METHOD}" == "dsxImds" ]]; then
  echo "CAPD_SPIRE_ATTESTATION_METHOD=dsxImds requires an executable DSX_IMDS_AGENT_PLUGIN_PATH" >&2
  exit 1
fi

# The local identity-exchange endpoint uses a cert-manager-issued certificate.
# Trust its public CA in the node image so the agent verifies HTTPS normally.
: > "${build_context}/dsx-identity-ca.crt"
if [[ -n "${DSX_IDENTITY_CA_BUNDLE_PATH}" ]]; then
  if [[ ! -s "${DSX_IDENTITY_CA_BUNDLE_PATH}" ]]; then
    echo "DSX_IDENTITY_CA_BUNDLE_PATH must name a non-empty CA bundle" >&2
    exit 1
  fi
  cp "${DSX_IDENTITY_CA_BUNDLE_PATH}" "${build_context}/dsx-identity-ca.crt"
fi

if [[ -n "${NKE_E2E_DOCKERHUB_MIRROR}" ]]; then
  if [[ ! "${NKE_E2E_DOCKERHUB_MIRROR}" =~ ^[a-zA-Z0-9.-]+(:[0-9]{1,5})?$ ]]; then
    echo "NKE_E2E_DOCKERHUB_MIRROR must be a registry host with an optional port" >&2
    exit 1
  fi
  # Reachability is logged, not enforced: the image still builds and pulls fall
  # through to the next host.
  # A registry answers /v2/ with 200 or, when it wants a token first, 401;
  # mirror.gcr.io does the latter, and curl -f used to report that as silence.
  probe_status="$(curl -sS -m 5 -o /dev/null -w '%{http_code}' "https://${NKE_E2E_DOCKERHUB_MIRROR}/v2/" || echo 000)"
  if [[ "${probe_status}" == "200" || "${probe_status}" == "401" ]]; then
    echo "[build-image] mirror ${NKE_E2E_DOCKERHUB_MIRROR} answered /v2/ (HTTP ${probe_status})"
  else
    echo "[build-image] mirror ${NKE_E2E_DOCKERHUB_MIRROR} did not answer /v2/ (HTTP ${probe_status}); pulls will fall through"
  fi
  echo "[build-image] pull order: ${NKE_E2E_DOCKERHUB_MIRROR} -> local dev cache -> upstream"
else
  echo "[build-image] NKE_E2E_DOCKERHUB_MIRROR unset; pull order: local dev cache -> upstream"
fi

# A local CAPD run may inject a manifests deb built from public CNI sources.
# Keep it optional so ordinary local image builds and published-agent builds
# retain their existing inputs. The Dockerfile scans this always-present
# scripts directory, which also keeps direct Dockerfile validation working.
if [[ -n "${CAPD_CNI_MANIFESTS_DEB_PATH}" ]]; then
  if [[ ! -f "${CAPD_CNI_MANIFESTS_DEB_PATH}" ]]; then
    echo "CAPD_CNI_MANIFESTS_DEB_PATH does not exist: ${CAPD_CNI_MANIFESTS_DEB_PATH}" >&2
    exit 1
  fi
  cp "${CAPD_CNI_MANIFESTS_DEB_PATH}" "${build_context}/scripts/cni-manifests.deb"
fi
if [[ -n "${CAPD_CNI_CHARTS_DEB_PATH}" ]]; then
  if [[ ! -f "${CAPD_CNI_CHARTS_DEB_PATH}" ]]; then
    echo "CAPD_CNI_CHARTS_DEB_PATH does not exist: ${CAPD_CNI_CHARTS_DEB_PATH}" >&2
    exit 1
  fi
  cp "${CAPD_CNI_CHARTS_DEB_PATH}" "${build_context}/scripts/cni-charts.deb"
fi

for entry in "${CAPD_CNI_IMAGES_DEB_PATH}:cni-images.deb" "${CAPD_HELM_DEB_PATH}:helm.deb"; do
  deb_path="${entry%:*}"
  [[ -n "${deb_path}" ]] || continue
  [[ -f "${deb_path}" ]] || { echo "CNI/Helm package does not exist: ${deb_path}" >&2; exit 1; }
  cp "${deb_path}" "${build_context}/scripts/${entry##*:}"
done

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
    echo "NODE_AGENT_REPO_PATH=${NODE_AGENT_REPO_PATH} does not look like an agent checkout." >&2
    exit 1
  fi

  # --match, then strip the prefix, exactly as agent/Makefile does. Keep the two
  # in step: this value is passed back to that Makefile as VERSION, and it names
  # the tarball read below.
  #
  # A bare `git describe --tags` returns the nearest tag from any series, so it
  # returned agent/v0.2.0-... here and put a slash in the middle of a filename.
  # The path it built could not exist, and the failure came from the `cp` several
  # lines later rather than from the version.
  effective_node_agent_version="${NODE_AGENT_VERSION:-$(
    git -C "${NODE_AGENT_REPO_PATH}" describe --match 'agent/v*' --tags --always --dirty 2>/dev/null \
      || git -C "${NODE_AGENT_REPO_PATH}" rev-parse --short HEAD
  )}"
  effective_node_agent_version="${effective_node_agent_version#agent/}"

  if [[ "${effective_node_agent_version}" == */* ]]; then
    echo "Agent version contains a slash and cannot name a file: ${effective_node_agent_version}" >&2
    exit 1
  fi

  node_agent_cgo_enabled="${CGO_ENABLED:-0}"
  echo "Building local agent ${effective_node_agent_version} for ${NODE_AGENT_DIR_ARCH} from ${NODE_AGENT_REPO_PATH} with CGO_ENABLED=${node_agent_cgo_enabled}..."
  CGO_ENABLED="${node_agent_cgo_enabled}" make -C "${NODE_AGENT_REPO_PATH}" "package-${NODE_AGENT_DIR_ARCH}" VERSION="${effective_node_agent_version}"

  local_node_agent_tgz="${NODE_AGENT_REPO_PATH}/release/agent_${effective_node_agent_version}_${NODE_AGENT_DIR_ARCH}.tgz"
  if [[ ! -f "${local_node_agent_tgz}" ]]; then
    echo "Expected local agent bundle was not produced: ${local_node_agent_tgz}" >&2
    exit 1
  fi
  cp "${local_node_agent_tgz}" "${build_context}/agent.tgz"
elif [[ -n "${NODE_AGENT_BUNDLE_PATH:-}" ]]; then
  if [[ ! -f "${NODE_AGENT_BUNDLE_PATH}" ]]; then
    echo "NODE_AGENT_BUNDLE_PATH does not exist: ${NODE_AGENT_BUNDLE_PATH}" >&2
    exit 1
  fi
  cp "${NODE_AGENT_BUNDLE_PATH}" "${build_context}/agent.tgz"
else
  if [[ -z "${NODE_AGENT_BASE_URL:-}" ]]; then
    echo "Downloading a published agent bundle needs NODE_AGENT_BASE_URL." >&2
    echo "Set it, or build the agent from source with CAPD_NODE_AGENT_LOCAL_BUILD=true." >&2
    exit 1
  fi
  node_agent_url="${NODE_AGENT_BASE_URL}/agent_${effective_node_agent_version}_${NODE_AGENT_BUNDLE_ARCH}.tgz"
  echo "Downloading published agent bundle ${node_agent_url}"
  fetch_url "${node_agent_url}" "${build_context}/agent.tgz"
fi

if [[ -n "${SPIRE_AGENT_BUNDLE_PATH:-}" ]]; then
  if [[ ! -f "${SPIRE_AGENT_BUNDLE_PATH}" ]]; then
    echo "SPIRE_AGENT_BUNDLE_PATH does not exist: ${SPIRE_AGENT_BUNDLE_PATH}" >&2
    exit 1
  fi
  cp "${SPIRE_AGENT_BUNDLE_PATH}" "${build_context}/spire-agent.tgz"
elif [[ "${SPIRE_AGENT_SOURCE}" == "github" ]]; then
  # Default: assemble the bundle from the UPSTREAM PUBLIC releases.
  #
  # These are the same artifacts the internal bundle is built from -- the
  # published tarball contains the upstream `spire-agent` and
  # `tpm_attestor_agent` binaries unmodified, byte for byte. Fetching them here
  # means a developer outside the NVIDIA network can build this image, while
  # downstream keeps the internal path by setting SPIRE_AGENT_SOURCE=bundle.
  assemble_spire_bundle_from_upstream "${build_context}/spire-agent.tgz"
else
  if [[ -z "${SPIRE_BUNDLES_BASE_URL:-}" ]]; then
    echo "SPIRE_AGENT_SOURCE=bundle needs SPIRE_BUNDLES_BASE_URL." >&2
    echo "Set it, or use the default SPIRE_AGENT_SOURCE=github." >&2
    exit 1
  fi
  spire_bundle_name="spire_agent_v${SPIRE_AGENT_VERSION}_tpm_attestor_v${TPM_ATTESTOR_VERSION}_${SPIRE_BUNDLE_ARCH}.tgz"
  spire_bundle_url="${SPIRE_BUNDLES_BASE_URL}/${spire_bundle_name}"
  echo "Downloading published SPIRE agent bundle ${spire_bundle_url}"
  fetch_url "${spire_bundle_url}" "${build_context}/spire-agent.tgz"
fi

# Through the mirror when one is set: the host daemon has none of its own, and
# an anonymous docker.io pull from the shared CI egress returns 429 under load.
kindest_node_image="${NKE_E2E_DOCKERHUB_MIRROR:+${NKE_E2E_DOCKERHUB_MIRROR}/}kindest/node"
echo "Ensuring ${kindest_node_image}:${CAPD_KINDEST_NODE_TAG} is available for ${effective_platform}..."
# The mirror sometimes 404s a blob it has not cached yet; a second pull gets it.
for attempt in 1 2 3; do
  "${CONTAINER_CLI}" pull --platform "${effective_platform}" "${kindest_node_image}:${CAPD_KINDEST_NODE_TAG}" && break
  if ((attempt == 3)); then exit 1; fi
  echo "  pull attempt ${attempt} failed, retrying in 15s" >&2
  sleep 15
done

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
  --build-arg "NKE_E2E_DOCKERHUB_MIRROR=${NKE_E2E_DOCKERHUB_MIRROR}"
  --build-arg "KINDEST_NODE_IMAGE=${kindest_node_image}"
)

if [[ -n "${CAPD_KINDEST_NODE_TAG:-}" ]]; then
  build_args+=(--build-arg "KINDEST_NODE_TAG=${CAPD_KINDEST_NODE_TAG}")
fi

if [[ -n "${CAPD_KINDEST_NODE_PLATFORM}" ]]; then
  build_args+=(--platform "${CAPD_KINDEST_NODE_PLATFORM}")
fi

build_args+=("${build_context}")

"${CONTAINER_CLI}" "${build_args[@]}"
