#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# EXPERIMENT. Build the NVKE management plane with NVKR, then deploy it to a fresh
# kind cluster with AICR:
#
#   1. build     nvkr run local --definition definition.yaml  -> charts and images in a local registry
#   2. pin       copy data/ and fill release-derived fields (chart repository and versions)
#   3. recipe    aicr recipe + aicr bundle --deployer helm
#   4. deploy    kind cluster, stage the controller image, create Secrets, ./deploy.sh
#
# LOCAL LANE: a local Run is `unqualified-local-execution` with dry-run signing, so
# `nvkr release verify` cannot succeed. This script does NOT verify the Release; a
# production flow must verify first and refuse otherwise.
#
# Needs docker, kind, kubectl, helm, go, python3. Neither the NVKR nor the AICR
# checkout is modified; both are only built.
#
#   NVKR_DIR / AICR_DIR   sibling checkouts (default ../../../nvkr, ../../../../nvidia/aicr)
#   CLUSTER               kind cluster name (default nvke-aicr)
#   ATTEMPT               NVKR attempt name (default build-<epoch>)
#   SKIP_BUILD=1          reuse the charts already in the local registry
#   KEEP_CLUSTER=1        do not delete an existing cluster named $CLUSTER
set -Eeuo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NVKR_DIR="${NVKR_DIR:-${HERE}/../../../nvkr}"
AICR_DIR="${AICR_DIR:-${HERE}/../../../../nvidia/aicr}"
CLUSTER="${CLUSTER:-nvke-aicr}"
ATTEMPT="${ATTEMPT:-build-$(date +%s)}"
WORK="${HERE}/.work"
REGISTRY_NAME=nvke-aicr-registry
REGISTRY_PORT=5000
PREFIX="nvke"
CHART_PORT=8765
CHARTS=(postgresql temporal controller-chart nktp spire spire-crds)
NICO="${HERE}/../nico"
# Throwaway credential for the kind cluster only.
# Kept in .work so a rerun against a kept cluster matches the password Postgres initialised with.
[ -f "${WORK}/db-password" ] || { mkdir -p "${WORK}"; openssl rand -hex 12 >"${WORK}/db-password"; }
DB_PASSWORD="${DB_PASSWORD:-$(cat "${WORK}/db-password")}"

log() { printf '\n== %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }
for t in docker kind kubectl helm go python3; do command -v "$t" >/dev/null || die "missing $t"; done
mkdir -p "${WORK}"
NVKR_DIR="$(cd "${NVKR_DIR}" && pwd)"
AICR_DIR="$(cd "${AICR_DIR}" && pwd)"
cleanup() { [ -n "${CHART_PID:-}" ] && kill "${CHART_PID}" 2>/dev/null || true; }
trap cleanup EXIT

log "tools: build nvkr and aicr from their checkouts (no changes to either)"
(cd "${NVKR_DIR}" && go build -o "${WORK}/nvkr" ./cmd/nvkr)
(cd "${AICR_DIR}" && go build -o "${WORK}/aicr" ./cmd/aicr)

log "registry: loopback OCI registry on ${REGISTRY_PORT}"
if ! docker container inspect "${REGISTRY_NAME}" >/dev/null 2>&1; then
  docker run -d --name "${REGISTRY_NAME}" -p "127.0.0.1:${REGISTRY_PORT}:5000" registry:3 >/dev/null
fi
docker start "${REGISTRY_NAME}" >/dev/null 2>&1 || true
until curl -fs "http://127.0.0.1:${REGISTRY_PORT}/v2/" >/dev/null; do sleep 1; done

if [ -z "${SKIP_BUILD:-}" ]; then
  log "build: everything in definition.yaml, arm64"
  (cd "${HERE}" && "${WORK}/nvkr" run local --definition definition.yaml --environment environment-local.yaml \
    --attempts-root .nvkr/runs --attempt "${ATTEMPT}" --runner-source "${NVKR_DIR}" --jobs 4 --publish --ui=plain)
fi

log "pin: pull the charts from the Release's registry and serve them as a chart repository"
# Helm refuses a plain-HTTP OCI registry and AICR's --vendor-charts refuses loopback, so the
# charts are exposed as an HTTP chart repository. Each pull verifies the layer digest.
rm -rf "${WORK}/charts" "${WORK}/pinned"
mkdir -p "${WORK}/charts"
for c in "${CHARTS[@]}"; do
  helm pull "oci://127.0.0.1:${REGISTRY_PORT}/${PREFIX}/${c}" --plain-http --destination "${WORK}/charts" >/dev/null
done
helm repo index "${WORK}/charts"
(cd "${WORK}/charts" && exec python3 -m http.server "${CHART_PORT}" --bind 127.0.0.1 >/dev/null 2>&1) &
CHART_PID=$!
until curl -fs "http://127.0.0.1:${CHART_PORT}/index.yaml" >/dev/null; do sleep 1; done
cp -R "${HERE}/data" "${WORK}/pinned"
chart_version() { helm show chart "oci://127.0.0.1:${REGISTRY_PORT}/${PREFIX}/$1" --plain-http | sed -n 's/^version: //p'; }
sed_args=(-e "s#@REPOSITORY@#http://127.0.0.1:${CHART_PORT}#")
for c in "${CHARTS[@]}"; do sed_args+=(-e "s#@VERSION:${c}@#$(chart_version "${c}")#"); done
sed -i.bak "${sed_args[@]}" "${WORK}/pinned/registry.yaml"
rm -f "${WORK}/pinned/registry.yaml.bak"
# The SPIRE TPM attestor checksum is per architecture. The values mirror cli/internal/bootstrap/controllerdeploy;
# the node architecture is the Docker host's, which is what kind runs.
case "$(docker info --format '{{.Architecture}}')" in
  aarch64|arm64) tpm_checksum=b69d0153c8241383f24da138bc85429a620d9f962a2f10abb314aa0e39046d16 ;;
  x86_64|amd64)  tpm_checksum=46d0caad8c25a027dd11c93e18b58a8bc6fbd9f1fe2e36fa2a0dd440986de4dc ;;
  *) die "unsupported node architecture" ;;
esac
sed -i.bak "s#@TPM_CHECKSUM@#${tpm_checksum}#" "${WORK}/pinned/components/spire/values.yaml"
rm -f "${WORK}/pinned/components/spire/values.yaml.bak"
sed -i.bak "s#@CLUSTER@#${CLUSTER}#" "${WORK}/pinned/components/nvke-base-config/manifests/nktp-cluster.yaml"
rm -f "${WORK}/pinned/components/nvke-base-config/manifests/nktp-cluster.yaml.bak"
# CAPNICo is a manifest, kept in the deploy path rather than copied: drop its Namespace (created below
# with the others) and let the image resolve to the one staged from the Release.
mkdir -p "${WORK}/pinned/components/capnico/manifests"
python3 -c '
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d and d.get("kind") != "Namespace"]
print(yaml.safe_dump_all(docs))' "${NICO}/capnico/capnico.yaml" >"${WORK}/pinned/components/capnico/manifests/capnico.yaml"

log "recipe + bundle: aicr, helm deployer"
rm -rf "${WORK}/bundle" "${WORK}/recipe.yaml"
"${WORK}/aicr" recipe --service nvke-kind --data "${WORK}/pinned" --output "${WORK}/recipe.yaml"
"${WORK}/aicr" bundle --recipe "${WORK}/recipe.yaml" --data "${WORK}/pinned" --deployer helm --output "${WORK}/bundle"

log "kind: fresh cluster ${CLUSTER}"
if kind get clusters 2>/dev/null | grep -qx "${CLUSTER}"; then
  [ -z "${KEEP_CLUSTER:-}" ] && kind delete cluster --name "${CLUSTER}"
fi
kind get clusters 2>/dev/null | grep -qx "${CLUSTER}" || kind create cluster --name "${CLUSTER}" --wait 120s
export KUBECONFIG="${WORK}/kubeconfig"
kind get kubeconfig --name "${CLUSTER}" >"${KUBECONFIG}"

log "stage: controller image into the node (no registry credential needed)"
# The chart pins ghcr.io/.../nvke-controller:<tag>@sha256:<digest>; the digest is the cwe output's.
image_ref="$(helm template x "${WORK}/charts/"controller-chart-*.tgz | sed -n 's/^ *image: "\(ghcr.io[^"]*\)".*/\1/p' | head -1)"
[ -n "${image_ref}" ] || die "controller chart names no ghcr image"
docker network connect kind "${REGISTRY_NAME}" 2>/dev/null || true
digest="${image_ref#*@}"
docker exec "${CLUSTER}-control-plane" ctr -n k8s.io images pull --plain-http \
  "${REGISTRY_NAME}:5000/${PREFIX}/cwe@${digest}" >/dev/null
docker exec "${CLUSTER}-control-plane" ctr -n k8s.io images tag --force \
  "${REGISTRY_NAME}:5000/${PREFIX}/cwe@${digest}" "${image_ref%@*}" >/dev/null
# The kubelet resolves name:tag@digest through the digest reference.
docker exec "${CLUSTER}-control-plane" ctr -n k8s.io images tag --force \
  "${REGISTRY_NAME}:5000/${PREFIX}/cwe@${digest}" "${image_ref%%:*}@${digest}" >/dev/null

log "stage: CAPNICo image from the Release"
capnico_ref="$(sed -n 's/^image: //p' "${NICO}/capnico/release.yaml")"
capnico_tag="$(curl -s "http://127.0.0.1:${REGISTRY_PORT}/v2/${PREFIX}/capnico-controller/tags/list" | python3 -c 'import json,sys;print(next(t for t in json.load(sys.stdin)["tags"] if t.startswith("sha256-")))')"
capnico_digest="sha256:${capnico_tag#sha256-}"
docker exec "${CLUSTER}-control-plane" ctr -n k8s.io images pull --plain-http \
  "${REGISTRY_NAME}:5000/${PREFIX}/capnico-controller@${capnico_digest}" >/dev/null
docker exec "${CLUSTER}-control-plane" ctr -n k8s.io images tag --force \
  "${REGISTRY_NAME}:5000/${PREFIX}/capnico-controller@${capnico_digest}" "${capnico_ref}" >/dev/null

log "secrets: created out of band, never in the data tree"
# The namespaces keep their Pod Security labels, which Helm's --create-namespace would not set.
kubectl apply -f "${NICO}/nvke-base-config/00-namespaces.yaml" >/dev/null
url="postgresql://postgres:${DB_PASSWORD}@postgres-postgresql.postgres.svc.cluster.local:5432/nke-controller?sslmode=disable"
for ns in postgres temporal nke-controller; do
  kubectl -n "${ns}" create secret generic nvke-database \
    --from-literal=password="${DB_PASSWORD}" --from-literal=postgres-password="${DB_PASSWORD}" \
    --from-literal=database-connection-string="${url}" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
done

log "prerequisite: Cluster API CRDs (the controller exits without them)"
# Outside AICR's scope: the old path ran `clusterctl init`, which also installs cert-manager and
# CAPI's controllers. kind only needs the CRDs, so apply those and drop their conversion webhooks.
CAPI_VERSION="${CAPI_VERSION:-v1.13.6}"
curl -fsSL "https://github.com/kubernetes-sigs/cluster-api/releases/download/${CAPI_VERSION}/core-components.yaml" |
  python3 -c '
import sys, yaml
docs = [d for d in yaml.safe_load_all(sys.stdin) if d and d.get("kind") == "CustomResourceDefinition"]
for d in docs:
    d["spec"].pop("conversion", None)
    d["metadata"].get("annotations", {}).pop("cert-manager.io/inject-ca-from", None)
print(yaml.safe_dump_all(docs))' |
  kubectl apply --server-side -f - >/dev/null

log "deploy: aicr bundle"
(cd "${WORK}/bundle" && chmod +x deploy.sh && ./deploy.sh)

log "ready: fail unless every component is up (the chart installs do not wait)"
kubectl -n cert-manager wait --for=condition=Available deployment --all --timeout=300s
kubectl -n capnico-system wait --for=condition=Available deployment --all --timeout=300s
kubectl -n spire-server wait --for=condition=Ready certificate --all --timeout=300s
kubectl -n spire-server wait --for=condition=Available deployment --all --timeout=300s
kubectl -n spire-server rollout status statefulset/spire-server --timeout=300s
kubectl -n spire-system rollout status daemonset --timeout=300s
kubectl -n nktp wait --for=condition=Available deployment --all --timeout=300s
kubectl -n nktp rollout status daemonset --timeout=300s
kubectl -n nktp rollout status statefulset --timeout=300s
kubectl -n postgres wait --for=condition=Ready pod -l app.kubernetes.io/instance=postgres --timeout=300s
kubectl -n temporal wait --for=condition=Complete job --all --timeout=300s
kubectl -n temporal wait --for=condition=Available deployment --all --timeout=300s
kubectl -n nke-controller wait --for=condition=Available deployment/nke-controller --timeout=300s
kubectl get pods -A
