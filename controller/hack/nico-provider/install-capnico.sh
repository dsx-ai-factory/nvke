#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

: "${CAPNICO_NAMESPACE:=capnico-system}"
: "${CAPNICO_RELEASE:=capi-provider-nico}"
: "${CAPNICO_IMAGE:=cluster-api-provider-nico:local}"
: "${CAPNICO_REBOOT_ANNOTATION:=nke.nvidia.com/reboot}"
: "${CAPNICO_REPAIR_ANNOTATION:=nke.nvidia.com/machine-health-issue}"
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

install_released_chart() {
  : "${NGC_API_KEY:?NGC_API_KEY is required when CAPNICO_SOURCE_DIR is unset}"
  : "${CAPNICO_CHART_VERSION:=0.0.28}"
  : "${CAPNICO_HELM_REPO:=ngc-components-dev}"
  # The org in the chart repository URL is an NGC account identifier, so it is
  # supplied rather than checked in -- see hack/internal-config-check.sh. Give
  # the org and the URL is derived; give the whole URL and the org is not read
  # at all. Either way this is the released-chart path, which already requires
  # NGC_API_KEY above, so it is one more value and not a new kind of setup.
  if [[ -z "${CAPNICO_HELM_REPO_URL:-}" ]]; then
    : "${CAPNICO_HELM_ORG:?CAPNICO_HELM_ORG is required: set it to the NGC org hosting the dev charts, or set CAPNICO_HELM_REPO_URL to the full repository URL}"
    CAPNICO_HELM_REPO_URL="https://helm.ngc.nvidia.com/${CAPNICO_HELM_ORG}/components-dev"
  fi

  kubectl -n "${CAPNICO_NAMESPACE}" create secret docker-registry nvcr-creds \
    --docker-server=nvcr.io \
    --docker-username='$oauthtoken' \
    --docker-password="${NGC_API_KEY}" \
    --dry-run=client -o yaml | kubectl apply -f -

  helm repo add "${CAPNICO_HELM_REPO}" "${CAPNICO_HELM_REPO_URL}" \
    --username '$oauthtoken' --password "${NGC_API_KEY}" --force-update >/dev/null
  helm repo update "${CAPNICO_HELM_REPO}" >/dev/null

  helm upgrade --install "${CAPNICO_RELEASE}" \
    "${CAPNICO_HELM_REPO}/${CAPNICO_RELEASE}" \
    --version "${CAPNICO_CHART_VERSION}" \
    --namespace "${CAPNICO_NAMESPACE}" \
    --server-side=false \
    --reset-values \
    --set 'imagePullSecrets[0].name=nvcr-creds' \
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
  DOCKER_BUILDKIT=1 docker build -t "${CAPNICO_IMAGE}" "${source_dir}"
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
  install_released_chart
fi

# Keep the reboot and repair contracts identical in released-chart and source modes.
kubectl -n "${CAPNICO_NAMESPACE}" patch deployment/controller-manager \
  --type=strategic \
  -p "{\"spec\":{\"template\":{\"spec\":{\"containers\":[{\"name\":\"manager\",\"args\":[\"--leader-elect\",\"--reboot-annotation=${CAPNICO_REBOOT_ANNOTATION}\",\"--repair-annotation=${CAPNICO_REPAIR_ANNOTATION}\"]}]}}}}"
kubectl -n "${CAPNICO_NAMESPACE}" rollout status deployment/controller-manager --timeout=180s
