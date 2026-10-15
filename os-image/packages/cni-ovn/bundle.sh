#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Builds the ovn-kubernetes chart and image for the nvke-cni-ovn-<stream> debs
# from the dsx-ai-factory/ovn-kubernetes source, pinned to a commit by the lock.
#
# Usage: bundle.sh --version 1.6.0 --dist DIR --lock-dir DIR --crane PATH
#                  [--arch amd64] [--write-lock]
#
# Env: OVN_KUBERNETES_TOKEN (else GITHUB_TOKEN) reads the source while the fork
#      is not public.
#      OVN_KUBERNETES_REF is the branch or commit --write-lock locks
#      (default release-<version>). OVN_BUILDER_IMAGE and OVN_UBUNTU_BASE_IMAGE
#      override the image build's Go and runtime bases. OCI_BIN picks docker.
#
# The lock and archive handling is ../cni-common/bundle-lib.sh.

set -euo pipefail

CNI=cni-ovn
# shellcheck source-path=SCRIPTDIR
# shellcheck source=../cni-common/bundle-lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/../cni-common/bundle-lib.sh"

parse_bundle_args "$@"
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.0$ ]] ||
  die "--version must be a bare chart version with patch 0, such as 1.6.0, got ${VERSION}"

REPO="${OVN_KUBERNETES_REPO:-dsx-ai-factory/ovn-kubernetes}"
API="https://api.github.com/repos/${REPO}"
# Chart tags are named after the stream, and the image is tagged with the
# commit, so a lock bump is what changes the image the pods ask for.
IMAGE_REPO="localhost/nvke/ovnkube"
# The registry-less prefix values-nke.yaml hands the chart. The agent's
# registry override swaps the host and keeps the rest (ovnImageRepository).
IMAGE_PREFIX="${IMAGE_REPO%/*}"
# Base images are pinned by index digest, so a moved tag cannot change the build.
BUILDER_IMAGE="${OVN_BUILDER_IMAGE:-quay.io/projectquay/golang:1.26@sha256:506a668eb4c763a964ffc4d4ad0125d45d75aac899392be7e5d39380f8452564}"
UBUNTU_BASE_IMAGE="${OVN_UBUNTU_BASE_IMAGE:-ubuntu:26.04@sha256:f144425ff09be612d6d9ad965196e9cdc23dae1f42110a8a11a3e9a8198759f7}"

# The fork is internal until it is published. A token never goes on a command
# line: fetch feeds CURL_CONFIG to curl as a config file. curl drops the header
# on the redirect to codeload, so it does not travel past api.github.com.
CURL_CONFIG=""
SOURCE_TOKEN="${OVN_KUBERNETES_TOKEN:-${GITHUB_TOKEN:-}}"
if [[ -n "$SOURCE_TOKEN" ]]; then
  CURL_CONFIG="header = \"Authorization: Bearer ${SOURCE_TOKEN}\""
fi

# --write-lock resolves the ref to a commit, the one place a moving ref is read.
# A build fetches the commit the lock names.
if [[ "$WRITE_LOCK" == true ]]; then
  ref="${OVN_KUBERNETES_REF:-release-${VERSION}}"
  fetch_sha="${WORK}/ref.sha"
  CURL_CONFIG="${CURL_CONFIG}"$'\n''header = "Accept: application/vnd.github.sha"' \
    fetch "${API}/commits/${ref}" "$fetch_sha"
  COMMIT="$(tr -d '[:space:]' <"$fetch_sha")"
else
  COMMIT="$(lock_value source "$REPO")"
  [[ -n "$COMMIT" ]] || die "${LOCK} has no source line for ${REPO}; run 'make lock' first"
fi
[[ "$COMMIT" =~ ^[0-9a-f]{40}$ ]] || die "${REPO} commit must be a full sha, got '${COMMIT}'"

# Both arches must pin one commit: the chart's image tag is the commit, and an arch
# whose lock differs would ship a chart that asks for an image the other arch lacks.
other_arch=amd64
[[ "$ARCH" == amd64 ]] && other_arch=arm64
other_lock="${LOCK_DIR}/${other_arch}/${VERSION}.lock"
if [[ -f "$other_lock" ]]; then
  other_commit="$(awk -v repo="$REPO" '$1 == "source" && $2 == repo { print $3 }' "$other_lock")"
  if [[ -n "$other_commit" && "$other_commit" != "$COMMIT" ]]; then
    if [[ "$WRITE_LOCK" == true ]]; then
      echo "${CNI}: note: ${other_arch} pins ${other_commit}; lock it too with OVN_KUBERNETES_REF=${COMMIT}" >&2
    else
      die "${ARCH} pins ${COMMIT} but ${other_arch} pins ${other_commit}; run make lock for both with one OVN_KUBERNETES_REF"
    fi
  fi
fi
SOURCE_LINES+=("source ${REPO} ${COMMIT}")
IMAGE_TAG="${COMMIT:0:12}"
IMAGE_REF="${IMAGE_REPO}:${IMAGE_TAG}"

# The chart is a directory of the source tree; the fork publishes no chart repo.
SRC_TGZ="ovn-kubernetes-src-${COMMIT}.tar.gz"
SRC_URL="${API}/tarball/${COMMIT}"
fetch "$SRC_URL" "${WORK}/${SRC_TGZ}"
verify_artifact "$SRC_TGZ" "${WORK}/${SRC_TGZ}" "$SRC_URL"
tar_paths_safe "${WORK}/${SRC_TGZ}"

mkdir -p "${WORK}/src"
tar -xzf "${WORK}/${SRC_TGZ}" --no-same-owner -C "${WORK}/src"
# GitHub names the top-level directory after the repo and commit; that is not
# part of the contract.
src="$(find "${WORK}/src" -mindepth 1 -maxdepth 1 -type d -print -quit)"
chart_src="${src}/charts/ovn-kubernetes"
[[ -f "${chart_src}/Chart.yaml" ]] || die "${SRC_TGZ} has no charts/ovn-kubernetes/Chart.yaml"
grep -qx "version: ${VERSION}" "${chart_src}/Chart.yaml" ||
  die "${SRC_TGZ} is not chart version ${VERSION}"
[[ -f "${src}/dist/images/Dockerfile.ubuntu" ]] || die "${SRC_TGZ} has no dist/images/Dockerfile.ubuntu"

mkdir -p "${STAGE}/charts"
cp -R "$chart_src" "${STAGE}/charts/ovn-kubernetes"

# NVKE's overrides, not the fork's; ovnHelmChartStaged in agent/internal/agent/cni.go
# keys on this file. OVS runs on the host (checkHostOpenVSwitchReady), so the
# ovs-node subchart is tagged off. Every enabled component runs the one
# ovnkube image, so each names it by prefix and tag. The chart's helper adds
# the prefix to a registry-less repository such as "ovnkube".
# Enabled by the chart's default tags. A new image key in the fork renders a
# registry pull; the helm check below catches it at build time.
cat >"${STAGE}/charts/ovn-kubernetes/values-nke.yaml" <<EOF
tags:
  ovs-node: false
global:
  imageRepositoryPrefix: ${IMAGE_PREFIX}
  useImageDigest: false
ovnkube-control-plane:
  ovnkubeControlPlane:
    image:
      tag: ${IMAGE_TAG}
ovnkube-single-node-zone:
  ovnkubeSingleNodeZone:
    image:
      tag: ${IMAGE_TAG}
  ovnkubeSingleNodeZoneNbdb:
    image:
      tag: ${IMAGE_TAG}
  ovnkubeSingleNodeZoneSbdb:
    image:
      tag: ${IMAGE_TAG}
  ovnkubeSingleNodeZoneNorthd:
    image:
      tag: ${IMAGE_TAG}
  ovnkubeSingleNodeZoneOvnHost:
    image:
      tag: ${IMAGE_TAG}
EOF

# The embedded image is the only one the node can run without a registry, so a
# rendered image that is not it is a defect to catch here, not at cluster create.
if command -v helm >/dev/null 2>&1; then
  rendered="$(helm template ovn "${STAGE}/charts/ovn-kubernetes" \
    -f "${STAGE}/charts/ovn-kubernetes/values.yaml" -f "${STAGE}/charts/ovn-kubernetes/values-nke.yaml" \
    --set k8sAPIServer=https://127.0.0.1:6443 --set-string podNetwork=10.244.0.0/16/24 \
    --set serviceNetwork=10.96.0.0/16 --set global.corednsClusterIP=10.96.0.10 | awk '$1 == "image:" { print $2 }' | sort -u)"
  [[ "$rendered" == "$IMAGE_REF" ]] ||
    die "the chart renders images other than ${IMAGE_REF}: ${rendered//$'\n'/ }"
else
  echo "${CNI}: note: helm not found; skipped checking that the chart renders only ${IMAGE_REF}" >&2
fi

# nfpm's `type: tree` copies modes and mtimes from the source files.
find "${STAGE}/charts" -type d -exec chmod 0755 {} +
find "${STAGE}/charts" -type f -exec chmod 0644 {} +
find "${STAGE}/charts" -exec touch -d '2026-01-01 00:00:00Z' {} +

# The kubectl in the image tracks the Kubernetes the controller is built against.
kubectl_minor="$(awk '/^[[:space:]]*k8s.io\/kubernetes v/ { v = substr($2, 2); split(v, p, "."); print p[1] "." p[2]; exit }' \
  "${src}/go-controller/go.mod")"
[[ -n "$kubectl_minor" ]] || die "no k8s.io/kubernetes in ${REPO} go-controller/go.mod"

# The fork's Dockerfile.ubuntu leaves out three things the chart needs (Dockerfile.ubuntu.nv has all):
# - the node liveness and readiness scripts (and ovnkube-init.sh) the chart runs as probes from /root:
#   without them the liveness probe fails and kubelet kills ovnkube-controller every two minutes.
# - firewalld: ovnkube-cluster-manager looks up firewall-cmd at startup, and the chart only
#   turns firewalld off for the node.
# - a real iptables: the image copies wrappers that `chroot /host /sbin/iptables`, but the chart
#   mounts no host root at /host, so ovnkube-controller exits with "iptables ... exit status 127".
# Install both after the wrappers are copied, replacing them (update-alternatives will not
# overwrite a regular file). Remove once the fork's Dockerfile does this.
dockerfile="${WORK}/Dockerfile.ovnkube"
awk '/^LABEL io.k8s.display-name/ && !done {
  print "COPY --chmod=755 --from=ovnkube-builder /workspace/ovn-kubernetes/dist/images/ovnkube-init.sh /workspace/ovn-kubernetes/dist/images/ovnkube-node-readiness-check.sh /workspace/ovn-kubernetes/dist/images/ovnkube-node-liveness-check.sh /root/"
  print "RUN rm -f /usr/sbin/iptables /usr/sbin/iptables-save /usr/sbin/iptables-restore /usr/sbin/ip6tables /usr/sbin/ip6tables-save /usr/sbin/ip6tables-restore && \\"
  print "    apt-get update && apt-get install -y --no-install-recommends firewalld iptables && rm -rf /var/lib/apt/lists/*"
  done = 1 } { print }' "${src}/dist/images/Dockerfile.ubuntu" >"$dockerfile"
grep -q 'install -y --no-install-recommends firewalld iptables' "$dockerfile" ||
  die "${REPO} Dockerfile.ubuntu has no 'LABEL io.k8s.display-name' line to add the probe scripts, firewalld and iptables before"

# The build uses only public inputs: the Go and Ubuntu base images, the Ubuntu
# archive and pkgs.k8s.io. Not dist/images' ubuntu-nv-image (Artifactory).
build_image "$src" "$dockerfile" "$IMAGE_REF" \
  --build-arg "OVN_KUBERNETES_DIR=." \
  --build-arg "BUILDER_IMAGE=${BUILDER_IMAGE}" \
  --build-arg "KUBECTL_MINOR=${kubectl_minor}" \
  --build-arg "UBUNTU_BASE_IMAGE=${UBUNTU_BASE_IMAGE}"
repack_archive
finish_bundle
