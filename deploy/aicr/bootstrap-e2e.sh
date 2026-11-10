#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# EXPERIMENT. Run the nvkectl bootstrap e2e (cli/e2e/bootstrap/run.sh) with the controller stack deployed through
# AICR from the catalog template (aicr-catalog/): nvkectl renders the template, aicr bundles it, the bundle installs.
#
#   bootstrap-e2e.sh           prepare, then run the e2e
#   bootstrap-e2e.sh prepare   prepare only (what CI does in its own step): the charts this checkout builds, a chart
#                              repository serving them, aicr at a pinned commit, and a copy of the template whose
#                              unpublished chart locations point at that repository
#
# The charts Postgres (seed) and the controller are built from this checkout, because NVKE has no published location
# for them yet; Temporal and SPIRE come from their upstream repositories at the template's pinned versions. No Docker
# and no NVKR. Cluster API and the provisioned provider stay prerequisites outside AICR; nvkectl installs them.
#
# Run it ON the Linux host the e2e needs (nerdctl + standalone containerd + buildkitd, /dev/vtpmx, openvswitch,
# root), e.g. a Colima VM:  colima ssh -p <vm> -- /path/to/repo/deploy/aicr/bootstrap-e2e.sh
#
#   CONTROLLER_IMAGE image the e2e deploys (default nke-controller:latest; `make -C cli images` builds it)
#   DOCKERHUB_MIRROR Docker Hub pull-through mirror, as the CI lane sets it (default mirror.gcr.io)
#   AICR_REF         aicr commit installed (default: the pin below)
#   SUITE            provisioned (default) or nico: which e2e to run
set -Eeuo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERE}/../.." && pwd)"
WORK="${AICR_WORK:-${HERE}/.work}"
CHART_PORT=8765
CONTAINERD_ADDRESS="${CONTAINERD_ADDRESS:-/run/containerd-nerdctl/containerd.sock}"
CONTROLLER_IMAGE="${CONTROLLER_IMAGE:-nke-controller:latest}"
AICR_REF="${AICR_REF:-122c1bef03e23e3fc1127f6283f5d72481633956}"
DOCKERHUB_MIRROR="${DOCKERHUB_MIRROR:-mirror.gcr.io}"
PROVISIONED_IMAGE="${NVKECTL_CAPI_PROVISIONED_IMAGE:-cluster-api-provider-provisioned:latest}"
SPIRE_TRUST_DOMAIN_E2E="${NVKECTL_E2E_SPIRE_TRUST_DOMAIN:-nvkectl-e2e.example.test}"
UNPUBLISHED=oci://nvke.invalid/charts

log() { printf '\n== %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

prepare() {
  for t in go helm python3 curl; do command -v "$t" >/dev/null || die "missing $t"; done
  mkdir -p "${WORK}"
  rm -rf "${WORK}/charts" "${WORK}/catalog-template"
  mkdir -p "${WORK}/charts"

  log "charts: Postgres and the controller packaged from this checkout"
  for dir in postgresql controller; do
    helm package "${REPO_ROOT}/controller/deploy/charts/${dir}" --destination "${WORK}/charts" >/dev/null
  done
  helm repo index "${WORK}/charts"

  log "catalog template: unpublished chart locations point at the local chart repository"
  cp -R "${REPO_ROOT}/aicr-catalog" "${WORK}/catalog-template"
  for f in "${WORK}/catalog-template/registry.yaml" "${WORK}/catalog-template/overlays/"*.yaml; do
    sed -e "s#${UNPUBLISHED}#http://127.0.0.1:${CHART_PORT}#g" "${f}" >"${f}.new" && mv "${f}.new" "${f}"
  done
  # The template pins the versions of the charts it expects at those locations; they must be this checkout's.
  python3 - "${WORK}/catalog-template" "${REPO_ROOT}/controller/deploy/charts" <<'PY'
import sys, yaml
tree, charts = sys.argv[1:]
seed = yaml.safe_load(open(f"{tree}/overlays/nvke-seed.yaml"))["spec"]["componentRefs"]
reg = {c["name"]: c["helm"] for c in yaml.safe_load(open(f"{tree}/registry.yaml"))["components"]}
want = {
    "postgresql": next(r["version"] for r in seed if r["name"] == "postgres"),
    "controller": reg["nke-controller"]["defaultVersion"],
}
for chart, version in want.items():
    have = str(yaml.safe_load(open(f"{charts}/{chart}/Chart.yaml"))["version"])
    if have != version:
        sys.exit(f"the template expects {chart} chart {version}, this checkout builds {have}")
PY

  log "charts: serve them on 127.0.0.1:${CHART_PORT}"
  if [ -f "${WORK}/charts.pid" ]; then kill "$(cat "${WORK}/charts.pid")" 2>/dev/null || true; fi
  (cd "${WORK}/charts" && exec nohup python3 -m http.server "${CHART_PORT}" --bind 127.0.0.1 >/dev/null 2>&1) &
  echo $! >"${WORK}/charts.pid"
  until curl -fs "http://127.0.0.1:${CHART_PORT}/index.yaml" >/dev/null; do sleep 1; done

  log "aicr: ${AICR_REF}"
  GOBIN="${WORK}" GOFLAGS=-mod=mod go install "github.com/NVIDIA/aicr/cmd/aicr@${AICR_REF}"
  "${WORK}/aicr" --version
}

case "${1:-}" in
  prepare) prepare; exit 0 ;;
  "") ;;
  *) die "unknown mode ${1}" ;;
esac

# Tools installed without root come first: helm, kind, kubectl, clusterctl and jq in ${WORK}/vmbin, and Go in
# ${WORK}/vmgo (not under vmbin: a directory named go there would shadow the go binary for make).
[ ! -d "${WORK}/vmbin" ] || PATH="${WORK}/vmbin:${WORK}/vmgo/bin:${PATH}"
for t in go python3 helm kubectl kind nerdctl clusterctl jq sudo; do command -v "$t" >/dev/null || die "missing $t"; done
[ "$(uname -s)" = Linux ] || die "the e2e needs a Linux host: run this inside a Colima VM"
mkdir -p "${WORK}"
cleanup() {
  for pid in "${WATCH_PID:-}" "$(cat "${WORK}/charts.pid" 2>/dev/null || true)"; do
    [ -z "${pid}" ] || kill "${pid}" 2>/dev/null || true
  done
}
trap cleanup EXIT

# While the site cluster provisions, the stalled pod is in the tenant cluster: every CAPTURE_INTERVAL seconds
# write its pods, a describe, the nodes and the kube-system events to ${WORK}/tenant-capture.
capture_tenant() {
  local out="${WORK}/tenant-capture" boot ts
  mkdir -p "${out}"
  while :; do
    # The directory is root's, so the glob runs as root too.
    boot="$(sudo bash -c "compgen -G '/tmp/nvkectl-bootstrap-kubeconfig-nvkectl-bootstrap-*/config'" | tail -1 || true)"
    # shellcheck disable=SC2024 # the redirect is meant to be the user's: the root-owned kubeconfig is only read
    if [ -n "${boot}" ] && sudo env "PATH=${PATH}" kubectl --kubeconfig "${boot}" --request-timeout=20s -n nvke-system \
      get secret nke-e2e-kubeconfig -o jsonpath='{.data.value}' >"${out}/.secret" 2>/dev/null && [ -s "${out}/.secret" ]; then
      base64 -d "${out}/.secret" >"${out}/tenant.kubeconfig"
      ts="$(date +%H%M%S)"
      k=(kubectl --kubeconfig "${out}/tenant.kubeconfig" --request-timeout=20s)
      "${k[@]}" -n kube-system get pods -l dns-role=cluster-dns -o wide >"${out}/${ts}-1-pods.txt" 2>&1 || true
      "${k[@]}" -n kube-system describe pod -l dns-role=cluster-dns >"${out}/${ts}-2-describe.txt" 2>&1 || true
      "${k[@]}" get nodes -o wide >"${out}/${ts}-3-nodes.txt" 2>&1 || true
      "${k[@]}" -n kube-system get events --sort-by=.lastTimestamp >"${out}/${ts}-4-events.txt" 2>&1 || true
    fi
    sleep "${CAPTURE_INTERVAL:-120}"
  done
}

log "tools: nvkectl built from this checkout"
make -C "${REPO_ROOT}/cli" build >/dev/null

log "images: the controller and provider the e2e side-loads must be in the standalone containerd"
for img in "${CONTROLLER_IMAGE}" "${PROVISIONED_IMAGE}"; do
  [ -n "$(sudo nerdctl --address "${CONTAINERD_ADDRESS}" images -q "${img}")" ] ||
    die "${img} is not in ${CONTAINERD_ADDRESS}: run 'sudo -E make -C cli images' with CONTAINERD_ADDRESS, BUILDKIT_HOST and CONTAINERD_NAMESPACE set"
done

prepare

# The e2e downloads a 52 MB SPIRE archive with curl --max-time 30, which this VM's route to GitHub often
# cannot meet. A curl first on PATH lifts the limit for the run without touching the e2e.
mkdir -p "${WORK}/shim"
# shellcheck disable=SC2016 # CURL_MAX_TIME expands when the shim runs
printf '#!/bin/sh\nexec %s "$@" --max-time "${CURL_MAX_TIME:-900}"\n' "$(command -v curl)" >"${WORK}/shim/curl"
chmod +x "${WORK}/shim/curl"

log "e2e: cli/e2e/bootstrap (${SUITE:-provisioned}), deploying through AICR"
CAPI_VERSION="$(make --no-print-directory -C "${REPO_ROOT}" print-CAPI_VERSION)"
[ -n "${CAPI_VERSION}" ] || die "no CAPI_VERSION from make/versions.mk"
cd "${REPO_ROOT}/cli"
capture_tenant &
WATCH_PID=$!
catalog="${WORK}/catalog-template"
SUITE="${SUITE:-provisioned}"
case "${SUITE}" in provisioned) runner=run.sh ;; nico) runner=run-nico.sh ;; *) die "SUITE must be provisioned or nico" ;; esac
sudo -E env "PATH=${WORK}/shim:${PATH}" \
  NVKECTL_CATALOG_TEMPLATE="${catalog}" \
  NVKECTL_AICR="${WORK}/aicr" \
  NVKECTL_NVKE_CONTROLLER_IMAGE="${CONTROLLER_IMAGE}" \
  NVKECTL_CAPI_PROVISIONED_IMAGE="${PROVISIONED_IMAGE}" \
  NVKECTL_E2E_SPIRE_TRUST_DOMAIN="${SPIRE_TRUST_DOMAIN_E2E}" \
  NKE_E2E_DOCKERHUB_MIRROR="${DOCKERHUB_MIRROR}" \
  NVKECTL_BOOTSTRAP_DOCKERHUB_MIRROR="${DOCKERHUB_MIRROR}" \
  NVKECTL_NVKE_CONTROLLER_HELM_TIMEOUT="${NVKECTL_NVKE_CONTROLLER_HELM_TIMEOUT:-20m}" \
  NVKECTL_NVKE_CONTROLLER_READY_TIMEOUT="${NVKECTL_NVKE_CONTROLLER_READY_TIMEOUT:-15m}" \
  CAPI_VERSION="${CAPI_VERSION}" \
  CONTAINERD_ADDRESS="${CONTAINERD_ADDRESS}" \
  BUILDKIT_HOST="${BUILDKIT_HOST:-unix:///run/buildkit-nerdctl/buildkitd.sock}" \
  CONTAINERD_NAMESPACE="${CONTAINERD_NAMESPACE:-default}" \
  OCI_BIN=nerdctl \
  "./e2e/bootstrap/${runner}"
