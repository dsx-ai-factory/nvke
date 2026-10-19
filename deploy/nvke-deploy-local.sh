#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# The install for the local site: nvke-deploy.sh with the lab's steps around it.
#
#   deploy/nvke-deploy-local.sh install [more nvke-deploy.sh arguments]
#
# Before the install, the lab StorageClass (Rancher's local-path-provisioner,
# at the version make/tool-versions.mk pins); a real site brings the CSI
# driver for its storage. Then `deploy/nvke-deploy.sh install --deploy-path
# deploy/nico` with the site's inputs read from the environment, so the
# command line carries none of them. After it, the SPIRE exec-auth patch for
# the Cluster API controllers, which every site applies after the deploy
# path. Anything else on the command line is passed to nvke-deploy.sh, such
# as --dry-run or --values-for nktp=<file>.
#
# The inputs, which hack/local-site.sh setup writes into the workspace's
# env.sh:
#   NVKE_SITESPEC          the site spec the bootstrap ran with
#   NVKE_STACK_CONFIG      the stack config, which names the two images the
#                          setup staged into the node
#   NVKE_KUBECONFIG        the bootstrapped cluster's kubeconfig
# and the NVKECTL_* credentials nvke-deploy.sh reads. Each path defaults to
# the file of that name under NVKE_LOCAL_SITE_DIR (default ~/nvke-local-site).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
WORKSPACE="${NVKE_LOCAL_SITE_DIR:-${HOME}/nvke-local-site}"
SITESPEC="${NVKE_SITESPEC:-${WORKSPACE}/sitespec.yaml}"
STACK_CONFIG="${NVKE_STACK_CONFIG:-${WORKSPACE}/stack-config.json}"
KUBECONFIG_PATH="${NVKE_KUBECONFIG:-${WORKSPACE}/local-site.kubeconfig}"
DRY_RUN=""

log() { printf '\n=== %s\n' "$*" >&2; }
die() { printf 'nvke-deploy-local: %s\n' "$*" >&2; exit 1; }

usage() {
  sed -n '4,/^set -euo/p' "${BASH_SOURCE[0]}" | sed '$d' | sed 's/^# \{0,1\}//' >&2
  exit 2
}

[[ "${1:-}" == install ]] || usage
shift
# Only the executor's own flags: everything after -- goes to helm.
for arg in "$@"; do
  [[ "${arg}" == -- ]] && break
  [[ "${arg}" == --dry-run ]] && DRY_RUN=1
done
for f in "${SITESPEC}" "${STACK_CONFIG}" "${KUBECONFIG_PATH}"; do
  [[ -f "${f}" ]] || die "${f} is missing; run make local-site-setup and nvkectl bootstrap site first, or set NVKE_SITESPEC, NVKE_STACK_CONFIG and NVKE_KUBECONFIG"
done
log "inputs: site spec ${SITESPEC}, stack config ${STACK_CONFIG}, kubeconfig ${KUBECONFIG_PATH}"

# The lab StorageClass. A site with real storage uses it instead.
. "${REPO_ROOT}/hack/read-tool-version.sh"
version="$(read_tool_version LOCAL_PATH_PROVISIONER_VERSION)" || die "make/tool-versions.mk pins no LOCAL_PATH_PROVISIONER_VERSION"
if [[ -n "${DRY_RUN}" ]]; then
  log "dry run: the lab StorageClass local-path-provisioner ${version} would be installed and made the default"
else
  if kubectl --kubeconfig "${KUBECONFIG_PATH}" get storageclass local-path >/dev/null 2>&1; then
    log "lab StorageClass local-path is installed"
  else
    log "installing the lab StorageClass (local-path-provisioner ${version})"
    kubectl --kubeconfig "${KUBECONFIG_PATH}" apply -f "https://raw.githubusercontent.com/rancher/local-path-provisioner/${version}/deploy/local-path-storage.yaml"
  fi
  # The default, on every run: an earlier run may have applied the class and
  # stopped before this.
  kubectl --kubeconfig "${KUBECONFIG_PATH}" annotate storageclass local-path storageclass.kubernetes.io/is-default-class=true --overwrite
  # cert-manager's webhook, which clusterctl init installs, answers before
  # the spire item creates its Certificate.
  kubectl --kubeconfig "${KUBECONFIG_PATH}" -n cert-manager rollout status deployment/cert-manager-webhook --timeout=5m
fi

# The install itself, as a site runs it.
bash "${SCRIPT_DIR}/nvke-deploy.sh" install --deploy-path "${REPO_ROOT}/deploy/nico" \
  --sitespec "${SITESPEC}" --stack-config "${STACK_CONFIG}" --kubeconfig "${KUBECONFIG_PATH}" "$@"

# After the deploy path: the SPIRE exec-auth patch for the Cluster API
# controllers, which waits for the rollout once csi.spiffe.io is registered.
if [[ -n "${DRY_RUN}" ]]; then
  log "dry run: controller/hack/apply-capi-spiffe-exec-auth.sh --kubeconfig ${KUBECONFIG_PATH} would run"
else
  log "patching the Cluster API controllers with the SPIRE exec-auth plugin"
  bash "${REPO_ROOT}/controller/hack/apply-capi-spiffe-exec-auth.sh" --kubeconfig "${KUBECONFIG_PATH}"
fi
