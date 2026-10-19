#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Builds the ovn-kubernetes chart and image for the nvke-cni-ovn-<stream> debs
# from the dsx-ai-factory/ovn-kubernetes source, pinned to a commit by the lock.
#
# Usage: bundle.sh --version 2.4.2 --dist DIR --lock-dir DIR --crane PATH
#                  [--arch amd64] [--write-lock]
#
# Env: OVN_KUBERNETES_REF is the tag, branch or commit --write-lock locks
#      (default v<version>, the release tag). OVN_KUBERNETES_REPO names another
#      GitHub repository. OVN_UBUNTU_BASE_IMAGE overrides the public Ubuntu
#      runtime base. The source owns the Go builder. OCI_BIN picks docker.
#
# The lock and archive handling is ../cni-common/bundle-lib.sh.

set -euo pipefail

CNI=cni-ovn
# shellcheck source-path=SCRIPTDIR
# shellcheck source=../cni-common/bundle-lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/../cni-common/bundle-lib.sh"

parse_bundle_args "$@"
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] ||
  die "--version must be a chart version such as 2.4.2, got ${VERSION}"

REPO="${OVN_KUBERNETES_REPO:-dsx-ai-factory/ovn-kubernetes}"
SOURCE_URL="https://github.com/${REPO}.git"
# Chart tags are named after the stream, and the image is tagged with the
# commit, so a lock bump is what changes the image the pods ask for.
IMAGE_REPO="localhost/nvke/ovnkube"
# The registry-less prefix values-nke.yaml hands the chart. The agent's
# registry override swaps the host and keeps the rest (ovnImageRepository).
IMAGE_PREFIX="${IMAGE_REPO%/*}"
# Base images are pinned by index digest, so a moved tag cannot change the build.
UBUNTU_BASE_IMAGE="${OVN_UBUNTU_BASE_IMAGE:-docker.io/library/ubuntu:24.04@sha256:534baea6a22c03a63003dbc8dbe78fe34bc0d7e595d9a9dc9834884ff530eb55}"

command -v git >/dev/null 2>&1 || die "git is required to fetch the ${REPO} source"

# --write-lock resolves the ref to a commit, the one place a moving ref is read.
# A build fetches the commit the lock names.
if [[ "$WRITE_LOCK" == true ]]; then
  REF="${OVN_KUBERNETES_REF:-v${VERSION}}"
else
  REF="$(lock_value source "$REPO")"
  [[ -n "$REF" ]] || die "${LOCK} has no source line for ${REPO}; run 'make lock' first"
  [[ "$REF" =~ ^[0-9a-f]{40}$ ]] || die "${LOCK} source commit for ${REPO} must be a full sha, got '${REF}'"
fi

# One shallow fetch of that ref. git checks every object against its hash, so the
# lock pins the commit and its tree: no checksum of a generated archive, whose
# bytes GitHub may re-encode, and no api.github.com call, which is rate-limited.
src_git="${WORK}/src.git"
git init -q --bare "$src_git"
retry "git fetch ${REF} from ${REPO}" git -C "$src_git" fetch -q --depth 1 "$SOURCE_URL" "$REF"
COMMIT="$(git -C "$src_git" rev-parse 'FETCH_HEAD^{commit}')"
TREE="$(git -C "$src_git" rev-parse 'FETCH_HEAD^{tree}')"
[[ "$COMMIT" =~ ^[0-9a-f]{40}$ ]] || die "${REPO} ${REF} did not resolve to a commit, got '${COMMIT}'"
if [[ "$WRITE_LOCK" == false ]]; then
  [[ "$COMMIT" == "$REF" ]] || die "${REPO} served ${COMMIT} for ${REF}"
  want_tree="$(lock_value tree "$REPO")"
  [[ -n "$want_tree" ]] || die "${LOCK} has no tree line for ${REPO}; run 'make lock' first"
  [[ "$TREE" == "$want_tree" ]] || die "${REPO} ${COMMIT} has tree ${TREE}, the lock pins ${want_tree}"
fi

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
SOURCE_LINES+=("source ${REPO} ${COMMIT}" "tree ${REPO} ${TREE}")
IMAGE_TAG="${COMMIT:0:12}"
IMAGE_REF="${IMAGE_REPO}:${IMAGE_TAG}"

# The chart is a directory of the source tree; the fork publishes no chart repo.
# git archive gives every file the commit's time, as a GitHub tarball does, so
# the staged tree does not depend on when it was fetched.
src="${WORK}/src"
mkdir -p "$src"
git -C "$src_git" archive --format=tar "$COMMIT" | tar -x --no-same-owner -C "$src"
chart_src="${src}/charts/ovn-kubernetes"
[[ -f "${chart_src}/Chart.yaml" ]] || die "${REPO} ${COMMIT} has no charts/ovn-kubernetes/Chart.yaml"
grep -qx "version: ${VERSION}" "${chart_src}/Chart.yaml" ||
  die "${REPO} ${COMMIT} is not chart version ${VERSION}"
dockerfile="${src}/dist/images/Dockerfile.ubuntu.nv"
[[ -f "$dockerfile" ]] || die "${REPO} ${COMMIT} has no dist/images/Dockerfile.ubuntu.nv"

mkdir -p "${STAGE}/charts"
cp -R "$chart_src" "${STAGE}/charts/ovn-kubernetes"

# NVKE's overrides, not the fork's; ovnHelmChartStaged in agent/internal/agent/cni.go
# keys on this file. OVS runs on the host (checkHostOpenVSwitchReady), so the
# ovs-node subchart is tagged off. Every enabled component runs the one
# ovnkube image, so each names it by prefix and tag. The chart's helper adds
# the prefix to a registry-less repository such as "ovnkube".
# Enabled by the chart's default tags. A new image key in the fork renders a
# registry pull; the helm check below catches it at build time.
# Pod hostPort is on (docs/features/hostport.md in the fork). It needs shared
# gateway mode; the agent turns it off on container nodes, which run local.
cat >"${STAGE}/charts/ovn-kubernetes/values-nke.yaml" <<EOF
tags:
  ovs-node: false
global:
  imageRepositoryPrefix: ${IMAGE_PREFIX}
  useImageDigest: false
  enablePodHostPort: true
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
  manifest="$(helm template ovn "${STAGE}/charts/ovn-kubernetes" \
    -f "${STAGE}/charts/ovn-kubernetes/values.yaml" -f "${STAGE}/charts/ovn-kubernetes/values-nke.yaml" \
    --set k8sAPIServer=https://127.0.0.1:6443 --set-string podNetwork=10.244.0.0/16/24 \
    --set serviceNetwork=10.96.0.0/16 --set global.corednsClusterIP=10.96.0.10)"
  rendered="$(awk '$1 == "image:" { print $2 }' <<<"$manifest" | sort -u)"
  [[ "$rendered" == "$IMAGE_REF" ]] ||
    die "the chart renders images other than ${IMAGE_REF}: ${rendered//$'\n'/ }"
  grep -A1 'name: OVNKUBE_POD_HOST_PORT_ENABLE' <<<"$manifest" | grep -q 'value: "true"' ||
    die "the chart does not enable pod hostPort; check global.enablePodHostPort"
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

# The readiness script's last check resolves google.com or cloudflare.com through
# CoreDNS, and the chart cannot change the names, so a node without internet DNS is
# never Ready and a template change then deletes every ovnkube-node pod at once.
# The API service name still proves CoreDNS answers and needs no egress. Remove once
# the fork exposes EXTERNAL_DNS_NAMES in the chart.
readiness="${src}/dist/images/ovnkube-node-readiness-check.sh"
public_names="dns_names=\${EXTERNAL_DNS_NAMES:-\"google.com cloudflare.com\"}"
grep -qxF "$public_names" "$readiness" ||
  die "${REPO} dist/images/ovnkube-node-readiness-check.sh no longer defaults to google.com cloudflare.com; re-check the probe patch"
sed -i "s|^${public_names}\$|dns_names=\${EXTERNAL_DNS_NAMES:-\"kubernetes.default.svc.cluster.local\"}|" "$readiness"

# Use the NVIDIA recipe with public package sources; its installers require noble.
build_image "$src" "$dockerfile" "$IMAGE_REF" \
  --build-arg "BASE_IMAGE=$(image_transport_ref "${UBUNTU_BASE_IMAGE}")" \
  --build-arg "OVS_SOURCE=doca" \
  --build-arg "OVN_SOURCE=uca" \
  --build-arg "K8S_VER=v${kubectl_minor}"
repack_archive
finish_bundle
