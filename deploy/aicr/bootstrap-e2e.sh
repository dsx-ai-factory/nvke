#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# EXPERIMENT. Run the nvkectl bootstrap e2e (cli/e2e/bootstrap/run.sh) with AICR as the deploy step.
#
#   bootstrap-e2e.sh            build with NVKR, then run the e2e with this script as its deploy step
#   bootstrap-e2e.sh deploy     the deploy step itself: `nvkectl bootstrap site --deploy-command`
#                               runs it, as root, once the ephemeral kind cluster exists
#   bootstrap-e2e.sh prepare-ci prepare the deploy step for CI (no Docker, no NVKR): charts from upstream and
#                               from this checkout, and aicr at a pinned commit; use with AICR_FLAVOR=ci
#
# The e2e is unchanged. nvkectl still creates the kind cluster, side-loads the controller image, waits
# for the controller and builds the site cluster. Only its Postgres/Temporal/SPIRE/controller deploy
# is replaced: the charts come from the NVKR Release (definition.yaml) and AICR installs them from
# data/. Cluster API (clusterctl init) and the provisioned provider are prerequisites outside AICR,
# as before.
#
# Run it ON the Linux host the e2e needs (nerdctl + standalone containerd + buildkitd, /dev/vtpmx,
# openvswitch, root), e.g. the local-site Colima VM:
#   colima ssh -p local-site -- /path/to/nke/deploy/aicr/bootstrap-e2e.sh
#
# LOCAL LANE: the Release is not verified (`nvkr release verify` cannot succeed for a local Run), and
# the charts are re-served over HTTP, so nothing binds the bytes helm installs to the Release.
#
#   SKIP_BUILD=1     reuse the charts already in the local registry
#   CONTROLLER_IMAGE image the e2e deploys (default nke-controller:latest; `make -C cli images` builds it)
#   DOCKERHUB_MIRROR Docker Hub pull-through mirror, as the CI lane sets it (default mirror.gcr.io)
#   AICR_FLAVOR     nvkr (default): charts from the NVKR Release, recipe nvke-bootstrap
#                   ci: SPIRE and Temporal from upstream at the bootstrap manifest pins, Postgres and the
#                   controller from this checkout, recipe nvke-bootstrap-ci (what the CI legs use)
#   AICR_REF        aicr commit prepare-ci installs (default: the pin below)
#   STOCK_DEPLOY=1   leave nvkectl's built-in deploy in place: a baseline for the same host and images
#   CONTROLLER_CHART checkout (default) packages the controller chart from this checkout, so it matches
#                    the image under test; release uses the NVKR Release chart, which is built from the
#                    commit NVKR pins and must match the image
set -Eeuo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NKE="$(cd "${HERE}/../.." && pwd)"
NVKR_DIR="${NVKR_DIR:-${HERE}/../../../nvkr}"
AICR_DIR="${AICR_DIR:-${HERE}/../../../../nvidia/aicr}"
WORK="${AICR_WORK:-${HERE}/.work}"
REGISTRY_NAME=nvke-aicr-registry
REGISTRY_PORT=5000
PREFIX=nvke
CHART_PORT=8765
CHARTS=(postgresql temporal controller-chart spire spire-crds)
CONTAINERD_ADDRESS="${CONTAINERD_ADDRESS:-/run/containerd-nerdctl/containerd.sock}"
CONTROLLER_IMAGE="${CONTROLLER_IMAGE:-nke-controller:latest}"
CONTROLLER_CHART="${CONTROLLER_CHART:-checkout}"
AICR_FLAVOR="${AICR_FLAVOR:-nvkr}"
AICR_REF="${AICR_REF:-122c1bef03e23e3fc1127f6283f5d72481633956}"
DOCKERHUB_MIRROR="${DOCKERHUB_MIRROR:-mirror.gcr.io}"
PROVISIONED_IMAGE="${NVKECTL_CAPI_PROVISIONED_IMAGE:-cluster-api-provider-provisioned:latest}"
CWE_PORT="${NVKECTL_NVKE_CONTROLLER_PORT:-30080}"
SPIRE_TRUST_DOMAIN_E2E="${NVKECTL_E2E_SPIRE_TRUST_DOMAIN:-nvkectl-e2e.example.test}"

log() { printf '\n== %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------------------------------------------
# deploy: runs as root under nvkectl, with KUBECONFIG, NVKECTL_BOOTSTRAP_CLUSTER and NVKECTL_SITESPEC.
# ---------------------------------------------------------------------------------------------------
deploy() {
  : "${KUBECONFIG:?not set: run by nvkectl --deploy-command}"
  : "${NVKECTL_SITESPEC:?not set}" "${NVKECTL_BOOTSTRAP_CLUSTER:?not set}" "${CAPI_VERSION:?not set}"
  [ -x "${WORK}/aicr" ] && [ -d "${WORK}/charts" ] && [ -f "${WORK}/versions.env" ] ||
    die "run ${0} with no argument first: it builds the charts and aicr this step installs from"
  # Output reaches nvkectl only if this fails, so keep a copy.
  LOG=/tmp/nvke-aicr-bootstrap-deploy.log
  exec > >(tee "${LOG}") 2>&1
  T="$(mktemp -d)"
  chmod 700 "${T}"
  trap 'kill "${CHART_PID:-0}" 2>/dev/null || true; rm -rf "${T}"' EXIT

  log "charts: serve the Release charts as an HTTP chart repository"
  (cd "${WORK}/charts" && exec python3 -m http.server "${CHART_PORT}" --bind 127.0.0.1 >/dev/null 2>&1) &
  CHART_PID=$!
  until curl -fs "http://127.0.0.1:${CHART_PORT}/index.yaml" >/dev/null; do sleep 1; done

  log "pin: complete the bootstrap values from the site spec"
  cp -R "${HERE}/data" "${T}/pinned"
  # shellcheck disable=SC1091
  source "${WORK}/versions.env"
  sed_args=(-e "s#@REPOSITORY@#http://127.0.0.1:${CHART_PORT}#")
  for c in "${CHARTS[@]}"; do
    v="VERSION_${c//-/_}"
    sed_args+=(-e "s#@VERSION:${c}@#${!v}#")
  done
  sed -i "${sed_args[@]}" "${T}/pinned/registry.yaml"
  sed -i "s#@CWE_PORT@#${CWE_PORT}#" "${T}/pinned/components/nvke-bootstrap-config/manifests/nke-controller-external.yaml"
  case "$(uname -m)" in
    aarch64|arm64) tpm_checksum=b69d0153c8241383f24da138bc85429a620d9f962a2f10abb314aa0e39046d16 ;;
    x86_64|amd64)  tpm_checksum=46d0caad8c25a027dd11c93e18b58a8bc6fbd9f1fe2e36fa2a0dd440986de4dc ;;
    *) die "unsupported architecture $(uname -m)" ;;
  esac
  db_password="$(openssl rand -hex 12)"
  case "${AICR_FLAVOR}" in nvkr) service=nvke-bootstrap ;; ci) service=nvke-bootstrap-ci ;; *) die "AICR_FLAVOR must be nvkr or ci" ;; esac
  AICR_FLAVOR="${AICR_FLAVOR}" DB_PASSWORD="${db_password}" python3 - "${NVKECTL_SITESPEC}" "${NVKECTL_BOOTSTRAP_CLUSTER}" "${tpm_checksum}" "${NVKECTL_NVKE_CONTROLLER_IMAGE:-${CONTROLLER_IMAGE}}" \
    "${CWE_PORT}" "${T}/pinned" <<'PY'
import os, sys, yaml

spec_path, cluster_name, checksum, image, cwe_port, tree = sys.argv[1:]
spec = yaml.safe_load(open(spec_path))
cluster = spec.get("cluster") or {}
trust_domain = (spec.get("spire") or {}).get("trustDomain") or cluster_name + ".nke.local"
hashes = [n["tpmEkThumbprint"] for n in cluster.get("nodes") or [] if n.get("tpmEkThumbprint")]


def merge(base, over):
    for k, v in over.items():
        base[k] = merge(base.get(k) or {}, v) if isinstance(v, dict) and isinstance(base.get(k), (dict, type(None))) else v
    return base


def split_image(ref):
    # name[:tag][@digest], as splitImageRef in controllerdeploy.
    digest = ""
    if "@" in ref:
        ref, digest = ref.split("@", 1)
    tag = ""
    colon = ref.rfind(":")
    if colon > ref.rfind("/"):
        ref, tag = ref[:colon], ref[colon + 1:]
    return ref, tag, digest


spire_path = f"{tree}/components/spire/bootstrap-values.yaml"
spire = yaml.safe_load(open(spire_path))
server = {
    "adminIDs": [f"spiffe://{trust_domain}/nvke-service/nke-controller"],
    "controllerManager": {"className": "spire-mgmt-spire"},
    "service": {"port": 8081},
}
if hashes:
    # An allowlist of the nodes' endorsement keys; no thumbprints means no TPM attestor.
    server["nodeAttestor"] = {"tpmDirect": {"enabled": True, "checksum": checksum, "hashes": hashes}}
ca_cert, ca_key = os.environ.get("NVKECTL_SPIRE_CA_CERT_FILE"), os.environ.get("NVKECTL_SPIRE_CA_KEY_FILE")
if ca_cert and ca_key:
    cert = open(ca_cert).read()
    server["upstreamAuthority"] = {"disk": {"enabled": True, "secret": {"data": {
        "certificate": cert, "key": open(ca_key).read(), "bundle": cert}}}}
merge(spire, {
    "global": {"spire": {"clusterName": cluster_name, "trustDomain": trust_domain}},
    "spire-server": server,
    "spire-agent": {"server": {"port": 8081}},
})
yaml.safe_dump(spire, open(spire_path, "w"), sort_keys=False)
os.chmod(spire_path, 0o600)

flavor, password = os.environ["AICR_FLAVOR"], os.environ["DB_PASSWORD"]
ctl_path = f"{tree}/components/nke-controller/{'ci' if flavor == 'ci' else 'nvkr'}-values.yaml"
ctl = yaml.safe_load(open(ctl_path))
repo, tag, digest = split_image(image)
if flavor == "ci":
    # What the built-in deploy sets per run: the database password, and the chart wiring around it.
    merge(ctl, {
        "secrets": {"createDbSecret": True},
        "appDb": {"postgresPassword": password},
        "profiles": {"path": "/profiles"},
        "cni": {"installAfterInit": True},
        "allowControlPlaneWithoutVIP": False,
        "supportedInfraProviders": "infra",
    })
    for name, extra in (
        ("postgres", {"auth": {"password": password}}),
        ("temporal", {"server": {"config": {"persistence": {
            "default": {"sql": {"password": password}}, "visibility": {"sql": {"password": password}}}}}}),
    ):
        path = f"{tree}/components/{name}/ci-values.yaml"
        values = merge(yaml.safe_load(open(path)), extra)
        yaml.safe_dump(values, open(path, "w"), sort_keys=False)
        os.chmod(path, 0o600)
merge(ctl, {
    "image": {"repository": repo, "tag": tag, "digest": digest, "pullPolicy": "IfNotPresent"},
    "spiffe": {"enabled": True, "trustDomain": trust_domain, "spire": {
        "className": "spire-mgmt-spire", "serverAddress": "spire-server.spire-server.svc.cluster.local", "serverPort": "8081"}},
    "service": {"port": int(cwe_port)},
    "capi": {"enabled": True},
})
port = cluster.get("nodeAgentPort")
if port:
    ctl["env"] = [e for e in ctl.get("env") or [] if e.get("name") != "NODE_AGENT_PORT"] + [{"name": "NODE_AGENT_PORT", "value": str(port)}]
yaml.safe_dump(ctl, open(ctl_path, "w"), sort_keys=False)
os.chmod(ctl_path, 0o600)
PY

  log "secrets: namespaces, and the database Secret created out of band"
  # The checkout controller chart creates and owns nvke-system, as it does under the built-in deploy; Helm refuses
  # to adopt a namespace that already exists. The NVKR chart does not, so that flavor creates it.
  namespaces=(postgres temporal spire-server spire-system nke-controller)
  [ "${AICR_FLAVOR}" = ci ] || namespaces+=(nvke-system)
  for ns in "${namespaces[@]}"; do
    kubectl create namespace "${ns}" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  done
  url="postgresql://postgres:${db_password}@postgres-postgresql.postgres.svc.cluster.local:5432/nke-controller?sslmode=disable"
  # The NVKR-chart flavor reads the password from this Secret; the ci flavor takes it in the values, as the built-in deploy does.
  if [ "${AICR_FLAVOR}" = nvkr ]; then
    for ns in postgres temporal nke-controller; do
      kubectl -n "${ns}" create secret generic nvke-database \
        --from-literal=password="${db_password}" --from-literal=postgres-password="${db_password}" \
        --from-literal=database-connection-string="${url}" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
    done
  fi

  log "prerequisite: Cluster API ${CAPI_VERSION} and the provisioned provider (outside AICR)"
  if ! kubectl -n capi-system get deployment capi-controller-manager >/dev/null 2>&1; then
    clusterctl init --kubeconfig "${KUBECONFIG}" --core "cluster-api:${CAPI_VERSION}" \
      --bootstrap "kubeadm:${CAPI_VERSION}" --control-plane "kubeadm:${CAPI_VERSION}" --wait-providers
  fi
  default_image=docker.io/library/cluster-api-provider-provisioned:latest
  manifest="${NKE}/cli/internal/bootstrap/controllerdeploy/manifests/provisioned-provider.yaml"
  grep -qF "${default_image}" "${manifest}" || die "provisioned-provider.yaml no longer names ${default_image}"
  sed "s#${default_image}#${PROVISIONED_IMAGE}#g" "${manifest}" | kubectl apply --server-side --force-conflicts -f - >/dev/null
  kubectl -n provisioned-system rollout status deployment/provisioned-controller-manager --timeout=300s

  log "recipe + bundle: aicr, helm deployer"
  "${WORK}/aicr" recipe --service "${service}" --data "${T}/pinned" --output "${T}/recipe.yaml"
  "${WORK}/aicr" bundle --recipe "${T}/recipe.yaml" --data "${T}/pinned" --deployer helm --output "${T}/bundle"

  log "deploy: aicr bundle"
  (cd "${T}/bundle" && chmod +x deploy.sh && ./deploy.sh)
}

# ---------------------------------------------------------------------------------------------------
# prepare-ci: everything deploy installs from, without Docker or NVKR. Chart pins and checksums come from
# the bootstrap manifest the built-in deploy uses, so the two cannot disagree.
# ---------------------------------------------------------------------------------------------------
prepare_ci() {
  for t in go helm python3 curl sha256sum; do command -v "$t" >/dev/null || die "missing $t"; done
  rm -rf "${WORK}/charts" "${WORK}/versions.env"
  mkdir -p "${WORK}/charts"
  log "charts: upstream charts at the bootstrap manifest pins, checksum verified"
  python3 - "${NKE}/cli/internal/bootstrap/manifest/bootstrap-manifest.yaml" <<'PY' >"${WORK}/upstream.txt"
import sys, yaml
components = yaml.safe_load(open(sys.argv[1]))["components"]
for name in ("temporal", "spire", "spire-crds"):
    chart = components[name]["chart"]
    print(name, chart["repo"], chart["name"], chart["version"], chart["sha256"])
PY
  while read -r component repo chart version sha; do
    helm pull "${chart}" --repo "${repo}" --version "${version}" --destination "${WORK}/charts" >/dev/null
    printf '%s  %s\n' "${sha}" "${WORK}/charts/${chart}-${version}.tgz" | sha256sum -c - >/dev/null ||
      die "${chart} ${version} does not match the checksum in the bootstrap manifest"
    printf 'VERSION_%s=%s\n' "${component//-/_}" "${version}" >>"${WORK}/versions.env"
  done <"${WORK}/upstream.txt"
  rm -f "${WORK}/upstream.txt"
  log "charts: Postgres and the controller packaged from this checkout"
  for pair in postgresql:postgresql controller:controller-chart; do
    dir="${NKE}/controller/deploy/charts/${pair%%:*}"
    helm package "${dir}" --destination "${WORK}/charts" >/dev/null
    name="${pair##*:}"
    printf 'VERSION_%s=%s\n' "${name//-/_}" "$(sed -n 's/^version: "\{0,1\}\([^"]*\)"\{0,1\}$/\1/p' "${dir}/Chart.yaml")" >>"${WORK}/versions.env"
  done
  helm repo index "${WORK}/charts"
  log "aicr: ${AICR_REF}"
  GOBIN="${WORK}" GOFLAGS=-mod=mod go install "github.com/NVIDIA/aicr/cmd/aicr@${AICR_REF}"
  "${WORK}/aicr" --version
}

case "${1:-}" in
  deploy) deploy; exit 0 ;;
  prepare-ci) prepare_ci; exit 0 ;;
  "") ;;
  *) die "unknown mode ${1}" ;;
esac

# ---------------------------------------------------------------------------------------------------
# default: build with NVKR as the user, then run the e2e as root with deploy as its deploy step.
# ---------------------------------------------------------------------------------------------------
# Tools installed without root come first: helm, kind, kubectl, clusterctl and jq in ${WORK}/vmbin, and Go in
# ${WORK}/vmgo (not under vmbin: a directory named go there would shadow the go binary for make).
[ ! -d "${WORK}/vmbin" ] || PATH="${WORK}/vmbin:${WORK}/vmgo/bin:${PATH}"
tools=(go python3 helm kubectl kind nerdctl clusterctl jq sudo)
[ "${AICR_FLAVOR}" = ci ] || tools+=(docker)
for t in "${tools[@]}"; do command -v "$t" >/dev/null || die "missing $t"; done
[ "$(uname -s)" = Linux ] || die "the e2e needs a Linux host: run this inside a Colima VM"
mkdir -p "${WORK}"
cleanup() { for pid in "${CHART_PID:-}" "${WATCH_PID:-}"; do [ -z "${pid}" ] || kill "${pid}" 2>/dev/null || true; done; }
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
make -C "${NKE}/cli" build >/dev/null

log "images: the controller and provider the e2e side-loads must be in the standalone containerd"
for img in "${CONTROLLER_IMAGE}" "${PROVISIONED_IMAGE}"; do
  [ -n "$(sudo nerdctl --address "${CONTAINERD_ADDRESS}" images -q "${img}")" ] ||
    die "${img} is not in ${CONTAINERD_ADDRESS}: run 'sudo -E make -C cli images' with CONTAINERD_ADDRESS, BUILDKIT_HOST and CONTAINERD_NAMESPACE set"
done

if [ "${AICR_FLAVOR}" = ci ]; then
  prepare_ci
else
NVKR_DIR="$(cd "${NVKR_DIR}" && pwd)"
AICR_DIR="$(cd "${AICR_DIR}" && pwd)"
log "tools: nvkr and aicr built from their checkouts (no changes to either)"
(cd "${NVKR_DIR}" && go build -o "${WORK}/nvkr" ./cmd/nvkr)
(cd "${AICR_DIR}" && go build -o "${WORK}/aicr" ./cmd/aicr)
log "registry: loopback OCI registry on ${REGISTRY_PORT}"
if ! docker container inspect "${REGISTRY_NAME}" >/dev/null 2>&1; then
  docker run -d --name "${REGISTRY_NAME}" -p "127.0.0.1:${REGISTRY_PORT}:5000" registry:3 >/dev/null
fi
docker start "${REGISTRY_NAME}" >/dev/null 2>&1 || true
until curl -fs "http://127.0.0.1:${REGISTRY_PORT}/v2/" >/dev/null; do sleep 1; done

if [ -z "${SKIP_BUILD:-}" ]; then
  log "build: everything in definition.yaml, for this host's architecture"
  case "$(uname -m)" in aarch64) platform=linux/arm64 ;; x86_64) platform=linux/amd64 ;; *) die "unsupported architecture" ;; esac
  [ "${platform}" = linux/arm64 ] || die "definition.yaml builds linux/arm64 only; edit it for ${platform}"
  (cd "${HERE}" && "${WORK}/nvkr" run local --definition definition.yaml --environment environment-local.yaml \
    --attempts-root .nvkr/runs --attempt "build-$(date +%s)" --runner-source "${NVKR_DIR}" --jobs 4 --publish --ui=plain)
fi

log "pin: pull the Release's charts"
rm -rf "${WORK}/charts" "${WORK}/versions.env"
mkdir -p "${WORK}/charts"
for c in "${CHARTS[@]}"; do
  [ "${c}" = controller-chart ] && [ "${CONTROLLER_CHART}" = checkout ] && continue
  helm pull "oci://127.0.0.1:${REGISTRY_PORT}/${PREFIX}/${c}" --plain-http --destination "${WORK}/charts" >/dev/null
  chart_version="$(helm show chart "oci://127.0.0.1:${REGISTRY_PORT}/${PREFIX}/${c}" --plain-http | sed -n 's/^version: //p')"
  [ -n "${chart_version}" ] || die "no chart version for ${c} in the Release registry"
  printf 'VERSION_%s=%s\n' "${c//-/_}" "${chart_version}" >>"${WORK}/versions.env"
done
if [ "${CONTROLLER_CHART}" = checkout ]; then
  chart_dir="${NKE}/controller/deploy/charts/controller"
  helm package "${chart_dir}" --destination "${WORK}/charts" >/dev/null
  printf 'VERSION_controller_chart=%s\n' "$(sed -n 's/^version: "\{0,1\}\([^"]*\)"\{0,1\}$/\1/p' "${chart_dir}/Chart.yaml")" >>"${WORK}/versions.env"
fi
helm repo index "${WORK}/charts"
fi

# The e2e downloads a 52 MB SPIRE archive with curl --max-time 30, which this VM's route to GitHub often
# cannot meet. A curl first on PATH lifts the limit for the run without touching the e2e.
mkdir -p "${WORK}/shim"
# shellcheck disable=SC2016 # CURL_MAX_TIME expands when the shim runs
printf '#!/bin/sh\nexec %s "$@" --max-time "${CURL_MAX_TIME:-900}"\n' "$(command -v curl)" >"${WORK}/shim/curl"
chmod +x "${WORK}/shim/curl"

log "e2e: cli/e2e/bootstrap/run.sh, deploying with AICR"
CAPI_VERSION="$(make --no-print-directory -C "${NKE}" print-CAPI_VERSION)"
[ -n "${CAPI_VERSION}" ] || die "no CAPI_VERSION from make/versions.mk"
cd "${NKE}/cli"
capture_tenant &
WATCH_PID=$!
deploy_command="${HERE}/bootstrap-e2e.sh deploy"
[ -z "${STOCK_DEPLOY:-}" ] || deploy_command=
sudo -E env "PATH=${WORK}/shim:${PATH}" \
  NVKECTL_BOOTSTRAP_DEPLOY_COMMAND="${deploy_command}" \
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
  AICR_FLAVOR="${AICR_FLAVOR}" \
  OCI_BIN=nerdctl \
  ./e2e/bootstrap/run.sh
