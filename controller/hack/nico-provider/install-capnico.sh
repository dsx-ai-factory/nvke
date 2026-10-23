#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

: "${CAPNICO_NAMESPACE:=capnico-system}"
: "${CAPNICO_RELEASE:=capi-provider-nico}"
: "${CAPNICO_IMAGE:=cluster-api-provider-nico:local}"
: "${CAPNICO_REBOOT_ANNOTATION:=nvke.nvidia.com/reboot}"
: "${CAPNICO_REPAIR_ANNOTATION:=nvke.nvidia.com/machine-health-issue}"
: "${CAPNICO_STAGED_CHART_DIR:?CAPNICO_STAGED_CHART_DIR is required}"
: "${KIND_CLUSTER_NAME:?KIND_CLUSTER_NAME is required}"
: "${KIND_BIN:=kind}"

adopt_namespace() {
  kubectl create namespace "${CAPNICO_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
  kubectl label namespace "${CAPNICO_NAMESPACE}" app.kubernetes.io/managed-by=Helm --overwrite
  kubectl annotate namespace "${CAPNICO_NAMESPACE}" \
    "meta.helm.sh/release-name=${CAPNICO_RELEASE}" \
    "meta.helm.sh/release-namespace=${CAPNICO_NAMESPACE}" --overwrite
}

# Install the published chart. Without CAPNICO_CHART_VERSION it installs the
# latest stable chart in the registry.
install_published_chart() {
  : "${CAPNICO_CHART_REF:=oci://ghcr.io/dsx-ai-factory/cluster-api-provider-nico/charts/capi-provider-nico}"
  if [[ -z "${CAPNICO_CHART_VERSION:-}" ]]; then
    CAPNICO_CHART_VERSION="$(helm show chart "${CAPNICO_CHART_REF}" | awk '/^version:/ { print $2; exit }')"
    if [[ -z "${CAPNICO_CHART_VERSION}" ]]; then
      echo "Cannot determine the latest CAPNICo chart version from ${CAPNICO_CHART_REF}. Set CAPNICO_CHART_VERSION." >&2
      exit 1
    fi
    echo "Installing the latest CAPNICo chart, ${CAPNICO_CHART_VERSION}."
  fi

  helm upgrade --install "${CAPNICO_RELEASE}" "${CAPNICO_CHART_REF}" \
    --version "${CAPNICO_CHART_VERSION}" \
    --namespace "${CAPNICO_NAMESPACE}" \
    --server-side=false \
    --reset-values \
    --wait --timeout 5m
}

install_source_chart() {
  source_dir="${CAPNICO_SOURCE_DIR}"
  chart_source="${source_dir}/chart"

  if [[ ! -f "${chart_source}/Chart.yaml" ]]; then
    echo "CAPNICo source '${source_dir}' is missing chart/Chart.yaml." >&2
    exit 1
  fi
  if [[ "${CAPNICO_IMAGE##*/}" != *:* ]]; then
    echo "CAPNICO_IMAGE must include a tag: '${CAPNICO_IMAGE}'." >&2
    exit 1
  fi

  if [[ "$(uname -s)" == Darwin && -z "${DOCKER_HOST:-}" ]]; then
    export DOCKER_HOST="${COLIMA_DOCKER_HOST:-unix://${HOME}/.colima/default/docker.sock}"
  fi

  echo "Building CAPNICo from ${source_dir}..."
  # The provider's Dockerfile names its Docker Hub base directly, with no ARG
  # to override, so BuildKit gets a rendered copy with official-image bases
  # (golang:…, alpine:…) pointed at NKE_E2E_DOCKERHUB_MIRROR when it is set.
  # Bases on other registries are left alone.
  rendered_dockerfile="$(mktemp)"
  if [[ -n "${NKE_E2E_DOCKERHUB_MIRROR:-}" ]]; then
    sed -E "s#^(FROM +(--platform=[^ ]+ +)?)(docker\.io/)?([a-z0-9._-]+)([:@])#\1${NKE_E2E_DOCKERHUB_MIRROR}/library/\4\5#" "${source_dir}/Dockerfile" >"${rendered_dockerfile}"
  else
    cp "${source_dir}/Dockerfile" "${rendered_dockerfile}"
  fi
  # Two attempts: a pull-through mirror can serve a manifest and then answer
  # "not found" for one of its blobs (mirror.gcr.io did, 2026-10-03), and
  # BuildKit does not retry that. The second attempt finds the blob cached.
  built=false
  for attempt in 1 2; do
    if DOCKER_BUILDKIT=1 docker build -t "${CAPNICO_IMAGE}" -f "${rendered_dockerfile}" "${source_dir}"; then
      built=true
      break
    fi
    echo "CAPNICo image build failed (attempt ${attempt}/2); retrying in 15s" >&2
    sleep 15
  done
  rm -f "${rendered_dockerfile}"
  if [[ "${built}" != true ]]; then
    echo "CAPNICo image build failed twice." >&2
    exit 1
  fi
  "${KIND_BIN}" load docker-image "${CAPNICO_IMAGE}" --name "${KIND_CLUSTER_NAME}"

  rm -rf "${CAPNICO_STAGED_CHART_DIR}"
  mkdir -p "$(dirname "${CAPNICO_STAGED_CHART_DIR}")"
  cp -R "${chart_source}" "${CAPNICO_STAGED_CHART_DIR}"

  helm upgrade --install "${CAPNICO_RELEASE}" "${CAPNICO_STAGED_CHART_DIR}" \
    --namespace "${CAPNICO_NAMESPACE}" \
    --server-side=false \
    --reset-values \
    --set-string "manager.image.repository=${CAPNICO_IMAGE%:*}" \
    --set-string "manager.image.tag=${CAPNICO_IMAGE##*:}" \
    --wait --timeout 5m
}

adopt_namespace
if [[ -n "${CAPNICO_SOURCE_DIR:-}" ]]; then
  install_source_chart
else
  install_published_chart
fi

# Keep the reboot and repair contracts identical in source and published modes.
kubectl -n "${CAPNICO_NAMESPACE}" patch deployment/controller-manager \
  --type=strategic \
  -p "{\"spec\":{\"template\":{\"spec\":{\"containers\":[{\"name\":\"manager\",\"args\":[\"--leader-elect\",\"--reboot-annotation=${CAPNICO_REBOOT_ANNOTATION}\",\"--repair-annotation=${CAPNICO_REPAIR_ANNOTATION}\"]}]}}}}"
kubectl -n "${CAPNICO_NAMESPACE}" rollout status deployment/controller-manager --timeout=180s
