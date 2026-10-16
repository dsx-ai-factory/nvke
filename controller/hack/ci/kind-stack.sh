#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Bring the controller stack up on a kind node from the stack artifacts
# `make local-stack-artifacts` builds.
#
# The artifacts are the controller image built from this checkout, the
# profiles fixture image, the CAPNICo image copied from its public registry,
# and the two NVKE charts, each as an OCI layout, with stack-artifacts.json
# naming them. The executor renders deploy/nico against the file, imports the
# layouts into the kind node's containerd, installs the result, and the
# assertions check that the pods run the digests the file names.
#
# Needs: docker with buildx, kind, kubectl, helm, clusterctl, oras, python3,
# GNU make 4, and nvkectl (NVKECTL, default cli/bin/nvkectl). Egress to the
# public chart repositories and image registries deploy/nico names.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
HERE="${REPO}/controller/hack/ci/kind-stack"
NVKECTL="${NVKECTL:-${REPO}/cli/bin/nvkectl}"
CLUSTER="${KIND_CLUSTER_NAME:-nvke-stack-smoke}"
# WORK holds this run's artifacts, site files and kubeconfig. It is created
# per run unless the caller names one, and never emptied: a caller's
# directory may hold another run's files.
WORK="${WORK:-$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/kind-stack.XXXXXX")}"
ARTIFACTS="${WORK}/artifacts"
KEEP_CLUSTER="${KEEP_CLUSTER:-false}"
CAPI_VERSION="${CAPI_VERSION:-$(make -C "${REPO}" --no-print-directory print-CAPI_VERSION)}"
DOCKERHUB_MIRROR="${NKE_E2E_DOCKERHUB_MIRROR:-}"
export KUBECONFIG="${WORK}/kubeconfig"

log() { printf '\n=== %s\n' "$*" >&2; }
die() { printf 'kind-stack: %s\n' "$*" >&2; exit 1; }

# Only a cluster this run created is deleted: a cluster of the same name
# that was there first is refused, not reused and not removed.
CREATED_CLUSTER=false
cleanup() {
  if [[ "${CREATED_CLUSTER}" == true && "${KEEP_CLUSTER}" != true ]]; then
    kind delete cluster --name "${CLUSTER}" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

command -v "${NVKECTL}" >/dev/null || die "nvkectl not found at ${NVKECTL}; run make -C cli build"
for tool in docker kind kubectl helm clusterctl oras python3; do
  command -v "${tool}" >/dev/null || die "${tool} is not on PATH"
done
mkdir -p "${WORK}"
[[ -z "$(ls -A "${WORK}")" ]] || die "WORK ${WORK} is not empty; name an empty directory or let this script create one"
mkdir -p "${WORK}/site"

# 1. The artifacts: the five layouts and the file that names them, written
# by the make target an operator runs. OUT is not pre-created: the target
# replaces it.
log "building the stack artifacts into ${ARTIFACTS}"
make -C "${REPO}" --no-print-directory local-stack-artifacts OUT="${ARTIFACTS}" CONTAINER_CLI=docker ARCH=amd64
# The layouts hold the images now; the buildx cache would only take disk the
# kind node needs.
docker builder prune -af >/dev/null 2>&1 || true
STACK_ARTIFACTS="${ARTIFACTS}/stack-artifacts.json"
# artifact_ref NAME prints the reference of an artifact as the file names it.
artifact_ref() {
  "${NVKECTL}" inspect stack-artifacts "${STACK_ARTIFACTS}" --artifacts | awk -F'\t' -v n="$1" '$1 == n { print $3 }'
}
cwe_ref="$(artifact_ref cwe)"; profiles_ref="$(artifact_ref controller-profiles)"; capnico_ref="$(artifact_ref capnico-controller)"
[[ -n "${cwe_ref}" && -n "${profiles_ref}" && -n "${capnico_ref}" ]] || die "the stack artifacts lack an image: $("${NVKECTL}" inspect stack-artifacts "${STACK_ARTIFACTS}" --artifacts)"
log "artifacts: ${cwe_ref}, ${profiles_ref}, ${capnico_ref}"

# 2. The kind node. Images from Docker Hub go through a mirror when one is
# named, as the other kind lanes do.
log "creating kind cluster ${CLUSTER}"
if kind get clusters 2>/dev/null | grep -qx "${CLUSTER}"; then
  die "a kind cluster named ${CLUSTER} already exists; delete it or set KIND_CLUSTER_NAME"
fi
CREATED_CLUSTER=true
kind create cluster --name "${CLUSTER}" --kubeconfig "${KUBECONFIG}" --wait 5m
node="${CLUSTER}-control-plane"
if [[ -n "${DOCKERHUB_MIRROR}" ]]; then
  docker exec -i "${node}" sh -c 'mkdir -p /etc/containerd/certs.d/docker.io && cat >/etc/containerd/certs.d/docker.io/hosts.toml' <<HOSTS
server = "https://registry-1.docker.io"
[host."https://${DOCKERHUB_MIRROR}"]
  capabilities = ["pull", "resolve"]
HOSTS
fi
vip="$(docker inspect -f '{{with index .NetworkSettings.Networks "kind"}}{{.IPAddress}}{{end}}' "${node}")"
[[ -n "${vip}" ]] || die "cannot read the kind node's address"

# 3. The site spec, the stack config and the credentials. The credentials are
# placeholders: nothing in this lane reaches NICo or a private registry. The
# stack config is the local site's: nico-sim's constants.
export NVKECTL_NICO_TOKEN=smoke-nico-token NVKECTL_NODE_USER=nvke NVKECTL_NODE_PASSWORD=smoke-tenant-password
NVKECTL_DATABASE_PASSWORD="$(openssl rand -hex 16)"
export NVKECTL_DATABASE_PASSWORD
# nktp sized for one kind node: nktp's own kind values, plus a ceiling of one
# replica on the autoscaled collectors. The whole stack shares this node, so a
# scale-up on a startup CPU burst would leave a replica Pending.
if grep -qE '^ *maxReplicas: 1$' "${REPO}/nktp/hack/e2e-values.yaml"; then
  cp "${REPO}/nktp/hack/e2e-values.yaml" "${WORK}/site/nktp-values.yaml"
else
  sed '/^      minReplicas: 1$/a\
      maxReplicas: 1' "${REPO}/nktp/hack/e2e-values.yaml" >"${WORK}/site/nktp-values.yaml"
fi
grep -qE '^ *maxReplicas: 1$' "${WORK}/site/nktp-values.yaml" || die "the nktp kind values changed shape; check nktp/hack/e2e-values.yaml"
sed -e "s|@CLUSTER@|${CLUSTER}|g" -e "s|@VIP@|${vip}|g" \
  "${HERE}/sitespec.yaml.tmpl" >"${WORK}/site/sitespec.yaml"
STACK_CONFIG="${REPO}/deploy/examples/local-site/stack-config.json"
"${NVKECTL}" validate sitespec -f "${WORK}/site/sitespec.yaml"
"${NVKECTL}" inspect stack-config "${STACK_CONFIG}" >/dev/null

# 4. The site runbook, in the order docs/operate/install-management-plane-services.md
# gives it. nvkectl bootstrap site untaints a real site's nodes; kind does
# not, so the lane does it here. kind ships a default StorageClass.
kubectl taint nodes --all node-role.kubernetes.io/control-plane- 2>/dev/null || true
log "checking the site"
NVKECTL="${NVKECTL}" CTR="docker exec -i ${node} ctr" bash "${REPO}/deploy/check-site.sh" --sitespec "${WORK}/site/sitespec.yaml" --stack-config "${STACK_CONFIG}" \
  --stack-artifacts "${STACK_ARTIFACTS}" --kubeconfig "${KUBECONFIG}"
log "installing Cluster API ${CAPI_VERSION}"
clusterctl init --core "cluster-api:${CAPI_VERSION}" --bootstrap "kubeadm:${CAPI_VERSION}" --control-plane "kubeadm:${CAPI_VERSION}" --wait-providers
kubectl rollout status deployment/cert-manager-webhook -n cert-manager --timeout=5m

# 5. Render and install in one go: the executor renders deploy/nico for the
# site and against the artifacts into a private scratch directory under the
# state directory, installs it, removes the scratch and records what it
# installed. CTR imports the layouts into the kind node's containerd. The
# state directory is this run's, so both can be checked afterwards: nothing
# rendered may outlive the install, since the rendered tree holds the site's
# credentials, and exactly one record must.
STATE="${WORK}/state"
log "a failed render leaves nothing behind"
# A file lacking the controller image: the render reaches its selection,
# after the scratch exists, and refuses. An earlier failure would prove
# nothing about the cleanup.
python3 -c '
import json, sys
doc = json.load(open(sys.argv[1]))
doc["artifacts"] = [a for a in doc["artifacts"] if a["name"] != "cwe"]
for a in doc["artifacts"]:
    a["layout"] = sys.argv[3] + "/" + a["layout"]
json.dump(doc, open(sys.argv[2], "w"))
' "${STACK_ARTIFACTS}" "${WORK}/no-cwe.json" "${ARTIFACTS}"
if probe="$(NVKECTL="${NVKECTL}" NVKE_STATE_DIR="${STATE}" bash "${REPO}/deploy/nvke-deploy.sh" install \
  --deploy-path "${REPO}/deploy/nico" --sitespec "${WORK}/site/sitespec.yaml" --stack-config "${STACK_CONFIG}" \
  --stack-artifacts "${WORK}/no-cwe.json" --kubeconfig "${KUBECONFIG}" 2>&1)"; then
  die "an install without the controller image exited 0"
fi
grep -q 'the render failed' <<<"${probe}" || die "the probe did not reach the render: ${probe}"
grep -q 'controller: image cwe is not in the stack artifacts' <<<"${probe}" || die "the probe did not fail on the missing image: ${probe}"
[[ -z "$(ls -A "${STATE}/renders" 2>/dev/null)" ]] || die "a failed render left scratch behind: $(ls -A "${STATE}/renders")"
[[ ! -e "${STATE}/installs" || -z "$(ls -A "${STATE}/installs")" ]] || die "a failed render wrote an install record"
log "rendering and installing deploy/nico"
NVKECTL="${NVKECTL}" NVKE_STATE_DIR="${STATE}" CTR="docker exec -i ${node} ctr" bash "${REPO}/deploy/nvke-deploy.sh" install \
  --deploy-path "${REPO}/deploy/nico" --sitespec "${WORK}/site/sitespec.yaml" --stack-config "${STACK_CONFIG}" \
  --stack-artifacts "${STACK_ARTIFACTS}" --kubeconfig "${KUBECONFIG}" --timeout 15m \
  --values-for "nktp=${WORK}/site/nktp-values.yaml"
[[ -z "$(ls -A "${STATE}/renders" 2>/dev/null)" ]] || die "the install left its render behind: $(ls -A "${STATE}/renders")"
if leftovers="$(find "${STATE}" -path "${STATE}/installs" -prune -o \( -name site-values.yaml -o -name site.env -o -name '*.yaml.tmpl' -o -name images.txt \) -print 2>/dev/null)" && [[ -n "${leftovers}" ]]; then
  die "rendered files outlived the install: ${leftovers}"
fi
records="$(find "${STATE}/installs" -mindepth 1 -maxdepth 1 -type d | wc -l | tr -d ' ')"
[[ "${records}" == 1 ]] || die "expected one install record, found ${records}: $(ls "${STATE}/installs" 2>/dev/null)"
record="$(find "${STATE}/installs" -mindepth 1 -maxdepth 1 -type d)"
[[ "$(ls -A "${record}" | sort | tr '\n' ' ')" == "images.txt render.log stack-artifacts.json status " ]] || die "the record holds $(ls -A "${record}" | tr '\n' ' '), not the four files"
grep -q '^status=ok$' "${record}/status" || die "the record's status is not ok: $(cat "${record}/status")"
"${NVKECTL}" inspect stack-artifacts "${record}/stack-artifacts.json" --meta >/dev/null || die "the recorded stack-artifacts.json does not load"
# After the deploy path: the SPIRE exec-auth patch for the Cluster API
# controllers, which waits for the rollout once csi.spiffe.io is registered.
log "patching the Cluster API controllers with the SPIRE exec-auth plugin"
bash "${REPO}/controller/hack/apply-capi-spiffe-exec-auth.sh" --kubeconfig "${KUBECONFIG}"

# 6. Assert: the images are in containerd under the file's names, every pod
# is Ready, and the controller and CAPNICo run the file's digests.
log "checking the result"
for ref in "${cwe_ref}" "${profiles_ref}" "${capnico_ref}"; do
  docker exec -i "${node}" ctr -n k8s.io images ls -q "name==${ref}" | grep -qx "${ref}" || die "containerd lacks ${ref}"
done
# The stack's workloads first, for a named failure; then every pod in the
# stack's namespaces must be Running and Ready, nothing excluded.
for target in deployment/nke-controller:nke-controller deployment/controller-manager:capnico-system statefulset/spire-server:spire-server \
  statefulset/postgres-postgresql:postgres deployment/temporal-frontend:temporal deployment/temporal-history:temporal deployment/temporal-matching:temporal; do
  kubectl -n "${target#*:}" rollout status "${target%%:*}" --timeout=5m || die "${target} is not ready"
done
not_ready() {
  local ns
  for ns in nvke-system capnico-system nktp postgres spire-server spire-system temporal nke-controller cert-manager capi-system; do
    kubectl -n "${ns}" get pods -o jsonpath='{range .items[?(@.status.phase!="Succeeded")]}{.metadata.namespace}/{.metadata.name}{"\t"}{.status.phase}{"\t"}{range .status.containerStatuses[*]}{.ready}{" "}{end}{"\t"}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' || die "cannot list pods in ${ns}"
  done | awk -F'\t' '$2 != "Running" || $3 ~ /false/ || $4 != "True"'
}
for _ in $(seq 1 30); do
  pending="$(not_ready)"
  [[ -n "${pending}" ]] || break
  sleep 10
done
[[ -z "${pending}" ]] || { printf '%s\n' "${pending}" >&2; die "pods that are not Running and Ready"; }
controller_ids="$(kubectl -n nke-controller get pods -l app.kubernetes.io/name=nke-controller \
  -o jsonpath='{range .items[*]}{range .status.initContainerStatuses[*]}{.imageID}{"\n"}{end}{range .status.containerStatuses[*]}{.imageID}{"\n"}{end}{end}')"
grep -q "${cwe_ref##*@}" <<<"${controller_ids}" || die "the controller does not run the cwe digest: ${controller_ids}"
grep -q "${profiles_ref##*@}" <<<"${controller_ids}" || die "the controller does not materialise the profiles digest: ${controller_ids}"
capnico_ids="$(kubectl -n capnico-system get pods -o jsonpath='{range .items[*]}{range .status.containerStatuses[*]}{.imageID}{"\n"}{end}{end}')"
grep -q "${capnico_ref##*@}" <<<"${capnico_ids}" || die "CAPNICo does not run the file's digest: ${capnico_ids}"
log "PASS: the stack runs the artifacts' digests on ${CLUSTER}"
