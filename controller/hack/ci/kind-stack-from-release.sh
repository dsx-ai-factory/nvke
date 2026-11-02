#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Bring the controller stack up on a kind node from a release description.
#
# The release is a nvkr.dev/release-description/v1 file filled from three OCI
# layouts this script makes: the controller image built from this checkout,
# the profiles fixture image, and the CAPNICo image copied from its public
# registry. The
# layouts sit under a directory shaped like a completed NVKR Run, so
# `nvkectl render deploy-path --nvkr-run-dir` reads them in place. The
# executor then installs deploy/nico on the kind node, importing the images
# into its containerd, and the assertions check that the pods run the digests
# the release names.
#
# Needs: docker with buildx, kind, kubectl, helm, clusterctl, oras, python3,
# and nvkectl (NVKECTL, default cli/bin/nvkectl). Egress to the public chart
# repositories and image registries deploy/nico names.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
HERE="${REPO}/controller/hack/ci/kind-stack-from-release"
NVKECTL="${NVKECTL:-${REPO}/cli/bin/nvkectl}"
CLUSTER="${KIND_CLUSTER_NAME:-nvke-release-smoke}"
# WORK holds this run's layouts, release, site files and kubeconfig. It is
# created per run unless the caller names one, and never emptied: a caller's
# directory may hold another run's files.
WORK="${WORK:-$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/kind-stack-from-release.XXXXXX")}"
RUN="${WORK}/run"
KEEP_CLUSTER="${KEEP_CLUSTER:-false}"
CAPI_VERSION="${CAPI_VERSION:-$(make -C "${REPO}" --no-print-directory print-CAPI_VERSION)}"
DOCKERHUB_MIRROR="${NKE_E2E_DOCKERHUB_MIRROR:-}"
export KUBECONFIG="${WORK}/kubeconfig"

log() { printf '\n=== %s\n' "$*" >&2; }
die() { printf 'kind-stack-from-release: %s\n' "$*" >&2; exit 1; }

# Only a cluster this run created is deleted: a cluster of the same name
# that was there first is refused, not reused and not removed.
CREATED_CLUSTER=false
cleanup() {
  if [[ "${CREATED_CLUSTER}" == true && "${KEEP_CLUSTER}" != true ]]; then
    kind delete cluster --name "${CLUSTER}" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

# layout_digest prints the digest of the first manifest an OCI layout lists.
layout_digest() {
  python3 -c 'import json, sys; print(json.load(open(sys.argv[1]))["manifests"][0]["digest"])' "$1/index.json"
}

# build_layout builds a single-platform image straight into an OCI layout.
build_layout() {
  local name="$1" file="$2" context="$3" tar="${WORK}/${1}.tar"
  log "building ${name} into an OCI layout"
  docker buildx build --file "${file}" --platform linux/amd64 \
    --provenance=false --sbom=false --output "type=oci,dest=${tar}" "${context}"
  mkdir -p "${RUN}/packages/${name}"
  tar -xf "${tar}" -C "${RUN}/packages/${name}"
  rm -f "${tar}"
}

command -v "${NVKECTL}" >/dev/null || die "nvkectl not found at ${NVKECTL}; run make -C cli build"
for tool in docker kind kubectl helm clusterctl oras python3; do
  command -v "${tool}" >/dev/null || die "${tool} is not on PATH"
done
mkdir -p "${WORK}"
[[ -z "$(ls -A "${WORK}")" ]] || die "WORK ${WORK} is not empty; name an empty directory or let this script create one"
mkdir -p "${RUN}/packages" "${WORK}/site"

# 1. The release: three layouts and the description that names them.
build_layout cwe "${REPO}/controller/Dockerfile" "${REPO}"
build_layout controller-profiles "${REPO}/controller/test/fixtures/Dockerfile" "${REPO}/controller/test/fixtures"
capnico_image="$(awk '$1 == "image:" { print $2 }' "${REPO}/deploy/nico/capnico/release.yaml")"
[[ -n "${capnico_image}" ]] || die "deploy/nico/capnico/release.yaml names no image"
log "copying ${capnico_image} into an OCI layout"
oras cp --platform linux/amd64 --to-oci-layout "${capnico_image}" "${RUN}/packages/capnico-controller:smoke"
# The layouts hold the images now; the buildx cache would only take disk the
# kind node needs.
docker builder prune -af >/dev/null 2>&1 || true
cwe_digest="$(layout_digest "${RUN}/packages/cwe")"
profiles_digest="$(layout_digest "${RUN}/packages/controller-profiles")"
capnico_digest="$(layout_digest "${RUN}/packages/capnico-controller")"
sed -e "s|@CWE_DIGEST@|${cwe_digest}|g" -e "s|@PROFILES_DIGEST@|${profiles_digest}|g" \
  -e "s|@CAPNICO_DIGEST@|${capnico_digest}|g" "${HERE}/release.json.tmpl" >"${WORK}/release.json"
log "release: cwe ${cwe_digest}, controller-profiles ${profiles_digest}, capnico-controller ${capnico_digest}"

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
# placeholders: nothing in this lane reaches NICo or a private registry.
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
cp "${HERE}/stack-config.json.tmpl" "${WORK}/site/stack-config.json"
"${NVKECTL}" validate sitespec -f "${WORK}/site/sitespec.yaml"
"${NVKECTL}" inspect stack-config "${WORK}/site/stack-config.json" >/dev/null

# 4. The site runbook, in the order docs/get-started/local-site.md
# gives it. nvkectl bootstrap site untaints a real site's nodes; kind does
# not, so the lane does it here. kind ships a default StorageClass.
kubectl taint nodes --all node-role.kubernetes.io/control-plane- 2>/dev/null || true
log "checking the site"
NVKECTL="${NVKECTL}" bash "${REPO}/deploy/check-site.sh" --sitespec "${WORK}/site/sitespec.yaml" --stack-config "${WORK}/site/stack-config.json" \
  --kubeconfig "${KUBECONFIG}" --release "${WORK}/release.json" --nvkr-run-dir "${RUN}"
log "installing Cluster API ${CAPI_VERSION}"
clusterctl init --core "cluster-api:${CAPI_VERSION}" --bootstrap "kubeadm:${CAPI_VERSION}" --control-plane "kubeadm:${CAPI_VERSION}" --wait-providers
kubectl rollout status deployment/cert-manager-webhook -n cert-manager --timeout=5m

# 5. Render and install in one go: the executor renders deploy/nico for the
# site and against the release into a private scratch directory under the
# state directory, installs it, and removes it. CTR imports the layouts into
# the kind node's containerd. The state directory is this run's, so the
# scratch can be checked afterwards: nothing rendered may outlive the
# install, since the rendered tree holds the site's credentials.
STATE="${WORK}/state"
log "a failed render leaves nothing behind"
if probe="$(NVKECTL="${NVKECTL}" NVKE_STATE_DIR="${STATE}" bash "${REPO}/deploy/nvke-deploy.sh" install \
  --deploy-path "${REPO}/deploy/nico" --sitespec "${WORK}/site/sitespec.yaml" --stack-config "${WORK}/site/stack-config.json" \
  --release "${WORK}/release.json" --nvkr-run-dir "${WORK}/no-such-run" --kubeconfig "${KUBECONFIG}" 2>&1)"; then
  die "an install with a missing run directory exited 0"
fi
# The probe has to fail in the render itself, after the scratch exists;
# an earlier failure would prove nothing about the cleanup.
grep -q 'the render failed' <<<"${probe}" || die "the probe did not reach the render: ${probe}"
[[ -z "$(ls -A "${STATE}/renders" 2>/dev/null)" ]] || die "a failed render left scratch behind: $(ls -A "${STATE}/renders")"
log "rendering and installing deploy/nico"
NVKECTL="${NVKECTL}" NVKE_STATE_DIR="${STATE}" CTR="docker exec -i ${node} ctr" bash "${REPO}/deploy/nvke-deploy.sh" install \
  --deploy-path "${REPO}/deploy/nico" --sitespec "${WORK}/site/sitespec.yaml" --stack-config "${WORK}/site/stack-config.json" \
  --release "${WORK}/release.json" --nvkr-run-dir "${RUN}" --kubeconfig "${KUBECONFIG}" --timeout 15m \
  --values-for "nktp=${WORK}/site/nktp-values.yaml"
[[ -z "$(ls -A "${STATE}/renders" 2>/dev/null)" ]] || die "the install left its render behind: $(ls -A "${STATE}/renders")"
if leftovers="$(find "${STATE}" \( -name site-values.yaml -o -name site.env -o -name '*.yaml.tmpl' -o -name images.txt \) 2>/dev/null)" && [[ -n "${leftovers}" ]]; then
  die "rendered files outlived the install: ${leftovers}"
fi
# After the deploy path: the SPIRE exec-auth patch for the Cluster API
# controllers, which waits for the rollout once csi.spiffe.io is registered.
log "patching the Cluster API controllers with the SPIRE exec-auth plugin"
bash "${REPO}/controller/hack/apply-capi-spiffe-exec-auth.sh" --kubeconfig "${KUBECONFIG}"

# 6. Assert: the images are in containerd under the release's names, every
# pod is Ready, and the controller and CAPNICo run the release digests.
log "checking the result"
for ref in "nvkr.local/nvke/cwe@${cwe_digest}" "nvkr.local/nvke/controller-profiles@${profiles_digest}" "nvkr.local/nvke/capnico-controller@${capnico_digest}"; do
  docker exec -i "${node}" ctr -n k8s.io images ls -q "name==${ref}" | grep -qx "${ref}" || die "containerd lacks ${ref}"
done
# The stack's workloads first, for a named failure; then every pod in the
# stack's namespaces must be Running and Ready, nothing excluded.
for target in deployment/nvke-controller:nvke-controller deployment/controller-manager:capnico-system statefulset/spire-server:spire-server \
  statefulset/postgres-postgresql:postgres deployment/temporal-frontend:temporal deployment/temporal-history:temporal deployment/temporal-matching:temporal; do
  kubectl -n "${target#*:}" rollout status "${target%%:*}" --timeout=5m || die "${target} is not ready"
done
not_ready() {
  local ns
  for ns in nvke-system capnico-system nktp postgres spire-server spire-system temporal nvke-controller cert-manager capi-system; do
    kubectl -n "${ns}" get pods -o jsonpath='{range .items[?(@.status.phase!="Succeeded")]}{.metadata.namespace}/{.metadata.name}{"\t"}{.status.phase}{"\t"}{range .status.containerStatuses[*]}{.ready}{" "}{end}{"\t"}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' || die "cannot list pods in ${ns}"
  done | awk -F'\t' '$2 != "Running" || $3 ~ /false/ || $4 != "True"'
}
for _ in $(seq 1 30); do
  pending="$(not_ready)"
  [[ -n "${pending}" ]] || break
  sleep 10
done
[[ -z "${pending}" ]] || { printf '%s\n' "${pending}" >&2; die "pods that are not Running and Ready"; }
controller_ids="$(kubectl -n nvke-controller get pods -l app.kubernetes.io/name=nvke-controller \
  -o jsonpath='{range .items[*]}{range .status.initContainerStatuses[*]}{.imageID}{"\n"}{end}{range .status.containerStatuses[*]}{.imageID}{"\n"}{end}{end}')"
grep -q "${cwe_digest}" <<<"${controller_ids}" || die "the controller does not run the release's cwe digest: ${controller_ids}"
grep -q "${profiles_digest}" <<<"${controller_ids}" || die "the controller does not materialise the release's profiles digest: ${controller_ids}"
capnico_ids="$(kubectl -n capnico-system get pods -o jsonpath='{range .items[*]}{range .status.containerStatuses[*]}{.imageID}{"\n"}{end}{end}')"
grep -q "${capnico_digest}" <<<"${capnico_ids}" || die "CAPNICo does not run the release's digest: ${capnico_ids}"
log "PASS: the stack runs the release's digests on ${CLUSTER}"
