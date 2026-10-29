#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

: "${CAPLL_NAMESPACE:=capll-system}"
: "${CAPLL_FAKE_NAMESPACE:=fake-ll-api}"
: "${CAPLL_RELEASE:=capi-provider-launchlayer}"
: "${CAPLL_IMAGE:=cluster-api-provider-launchlayer:local}"
: "${CAPLL_FAKE_IMAGE:=fake-ll-api-server:local}"
: "${CAPLL_STAGED_CHART_DIR:?CAPLL_STAGED_CHART_DIR is required}"
: "${CAPLL_SOURCE_DIR:?CAPLL_SOURCE_DIR is required}"
: "${KIND_CLUSTER_NAME:?KIND_CLUSTER_NAME is required}"
: "${KIND_BIN:=kind}"

chart_source="${CAPLL_SOURCE_DIR}/chart"
if [[ ! -f "${chart_source}/Chart.yaml" ]]; then
  echo "CAPLL source '${CAPLL_SOURCE_DIR}' is missing chart/Chart.yaml." >&2
  exit 1
fi

if [[ "$(uname -s)" == Darwin && -z "${DOCKER_HOST:-}" ]]; then
  export DOCKER_HOST="${COLIMA_DOCKER_HOST:-unix://${HOME}/.colima/default/docker.sock}"
fi

adopt_namespace() {
  kubectl create namespace "$1" --dry-run=client -o yaml | kubectl apply -f -
}

build_and_load() {
  local image="$1" dockerfile="$2"
  echo "Building ${image} from ${CAPLL_SOURCE_DIR}/${dockerfile}..."
  built=false
  for attempt in 1 2; do
    if DOCKER_BUILDKIT=1 docker build -t "${image}" -f "${CAPLL_SOURCE_DIR}/${dockerfile}" "${CAPLL_SOURCE_DIR}"; then
      built=true
      break
    fi
    echo "${image} build failed (attempt ${attempt}/2); retrying in 15s" >&2
    sleep 15
  done
  if [[ "${built}" != true ]]; then
    echo "${image} build failed twice." >&2
    exit 1
  fi
  "${KIND_BIN}" load docker-image "${image}" --name "${KIND_CLUSTER_NAME}"
}

adopt_namespace "${CAPLL_NAMESPACE}"

build_and_load "${CAPLL_IMAGE}" "Dockerfile"

# The manager's --identity-secret must exist before the chart's deployment can
# reach ready, and `helm upgrade --install --wait` below blocks on exactly
# that readiness -- so the secret is created first, not after.
kubectl -n "${CAPLL_NAMESPACE}" create secret generic launchlayer-credentials \
  --from-literal=clientID=fake-client \
  --from-literal=clientSecret=fake-secret \
  --from-literal=scopes=example-scope \
  --from-literal=issuerURL="http://fake-ll-vpc.${CAPLL_FAKE_NAMESPACE}.svc.cluster.local:8090" \
  --dry-run=client -o yaml | kubectl apply -f -

rm -rf "${CAPLL_STAGED_CHART_DIR}"
mkdir -p "$(dirname "${CAPLL_STAGED_CHART_DIR}")"
cp -R "${chart_source}" "${CAPLL_STAGED_CHART_DIR}"

helm upgrade --install "${CAPLL_RELEASE}" "${CAPLL_STAGED_CHART_DIR}" \
  --namespace "${CAPLL_NAMESPACE}" \
  --reset-values \
  -f "${CAPLL_SOURCE_DIR}/hack/tilt/values.yaml" \
  --set-string "manager.image.repository=${CAPLL_IMAGE%:*}" \
  --set-string "manager.image.tag=${CAPLL_IMAGE##*:}" \
  --wait --timeout 5m

build_and_load "${CAPLL_FAKE_IMAGE}" "Dockerfile.fake"

# hack/tilt/fake-ll-api.yaml hardcodes the bare "fake-ll-api-server" image
# name (no tag), which Kubernetes would otherwise read as ":latest" and try
# to pull from a registry instead of using the image just loaded into kind.
sed "s#image: fake-ll-api-server#image: ${CAPLL_FAKE_IMAGE}#" "${CAPLL_SOURCE_DIR}/hack/tilt/fake-ll-api.yaml" | kubectl apply -f -
kubectl apply -f "${CAPLL_SOURCE_DIR}/hack/tilt/compute-rbac.yaml"

kubectl -n "${CAPLL_FAKE_NAMESPACE}" rollout status deployment/fake-ll-vpc --timeout=180s
kubectl -n "${CAPLL_FAKE_NAMESPACE}" rollout status deployment/fake-ll-compute --timeout=180s
kubectl -n "${CAPLL_FAKE_NAMESPACE}" rollout status deployment/fake-ll-sg --timeout=180s
