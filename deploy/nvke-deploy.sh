#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Install a deploy path: walk its nvke-deploy.yaml and apply the items in the
# order listed. The list IS the order -- see
# docs/site-bootstrap/deploy-repository.md, which this format follows.
#
# This is the executor every deploy path under deploy/ and controller/deploy/
# is written for. deploy/nico is the customer path; the e2e lanes install
# theirs with the same script. The helpers it needs are in lib/ beside it.
#
# A helm item is one chart installed as one release; the item order does the
# sequencing. There are no enable flags and no phases, so nothing this script
# emits can override what an item's values decided.
#
# Charts come from the deploy path or the item's repository. A deploy path
# rendered against an NVKR release with `nvkectl render deploy-path` carries its
# charts as archives, its images by digest, and an images.txt of images to
# import from a local Run's OCI layouts; this script applies it the same way.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ITEMS_PY="${SCRIPT_DIR}/lib/nvke-deploy-items.py"
RENDER_PY="${SCRIPT_DIR}/lib/render-template.py"

DRY_RUN=""
KUBECONFIG_PATH=""
PHASE_TIMEOUT="${PHASE_TIMEOUT:-15m}"
REQUIRE_TIMEOUT="${REQUIRE_TIMEOUT:-180s}"
PASSTHROUGH=()
CONFIG=""
SITESPEC=""
STACK_CONFIG=""
RELEASE=""
RUN_DIR=""
# ARCH is the --arch override; NODE_ARCH is what the cluster's nodes report.
ARCH=""
NODE_ARCH=""
NVKECTL="${NVKECTL:-nvkectl}"
# The state directory holds the render scratch: a rendered deploy path
# carries the site's credentials, so it lives under a directory only this
# user can read, never a shared temp directory, and is removed on exit.
STATE_DIR="${NVKE_STATE_DIR:-${XDG_STATE_HOME:-${HOME}/.local/state}/nvke}"
SCRATCH=""
VALUES_FOR=()
DEPLOY_PATH=""
# CTR imports images into the node's containerd for a deploy path rendered
# from a local NVKR Run (images.txt names each image's OCI layout). It may
# carry arguments: CTR="sudo ctr" when the install does not run as root, or
# CTR="docker exec -i kind-control-plane ctr" when the cluster is a kind node.
CTR="${CTR:-ctr}"

log() { printf '\n=== %s\n' "$*" >&2; }
die() { printf 'nvke-deploy: %s\n' "$*" >&2; exit 1; }

usage() {
  cat >&2 <<'USAGE'
usage: nvke-deploy.sh install --deploy-path DIR [options] [-- helm args]

  With --sitespec and --stack-config, or --release, the deploy path is first
  rendered for the site and against the release (nvkectl does the rendering) into a private
  scratch directory under NVKE_STATE_DIR (default $XDG_STATE_HOME/nvke),
  installed from there, and the scratch directory is removed when the
  install ends, so no rendered credential is left behind.

  --deploy-path DIR   the deploy path to install: a directory holding
                      nvke-deploy.yaml and one directory per item, e.g.
                      deploy/nico or controller/deploy/capd-e2e
  --sitespec FILE     render for this site first (needs nvkectl: NVKECTL or PATH);
                      the site name, VIP and trust domain come from it
  --stack-config FILE with --sitespec: the stack's input for this site, the NICo
                      API and the optional images and mirror; start from
                      deploy/examples/stack-config.json. The NVKECTL_*
                      credentials must be exported; see deploy/README.md
  --arch linux/ARCH   render for this architecture instead of the one the
                      cluster's nodes report (linux/amd64 or linux/arm64)
  --release FILE      render against this NVKR release description first;
                      with --sitespec and --stack-config
  --nvkr-run-dir DIR  with --release, for a Run that was not published (a
                      local build, the CI lane): read charts and images from
                      this completed NVKR Run's OCI layouts instead of a
                      registry; images.txt then names them and they are
                      imported into the node's containerd (CTR names the
                      import command)
  --config PATH       BootstrapConfig, as JSON. Required by a template item;
                      its fields are the context those templates render against.
  --values-for ITEM=PATH
                      an extra values file for one item, applied after the
                      item's own values. This is how per-run values reach a
                      single component: with one release per chart the keys are
                      no longer prefixed, so a value meant for the controller
                      cannot be broadcast to every item. Repeatable.
  --dry-run           print the argv each step would run, and exit. The
                      render still runs
  --kubeconfig PATH   passed to every kubectl call
  --                  everything after is forwarded to helm verbatim
USAGE
  exit 2
}

# run executes a command, or prints it when --dry-run is set. Every helm and
# kubectl call goes through here, which is what makes --dry-run a complete
# description of the install rather than a summary of it.
run() {
  if [[ -n "${DRY_RUN}" ]]; then
    printf '%q ' "$@"
    printf '\n'
    return 0
  fi
  "$@"
}

kubectl_run() {
  if [[ -n "${KUBECONFIG_PATH}" ]]; then
    run kubectl --kubeconfig "${KUBECONFIG_PATH}" "$@"
  else
    run kubectl "$@"
  fi
}

# apply_manifest_item applies every .yaml in the item directory, lexicographically.
# The generator names them 00-, 10-, 40- precisely so that order is the intended
# one: namespaces exist before anything lands in them.
apply_manifest_item() {
  local dir="$1" file found=""
  if [[ -f "${dir}/kustomization.yaml" ]]; then
    # Rendered by nvkectl render deploy-path: the committed YAML with its image
    # rewritten to the release's repository and digest.
    log "kustomization: $(awk '$1 == "newName:" { print $2 }' "${dir}/kustomization.yaml" | tr -d '"')@$(awk '$1 == "digest:" { print $2 }' "${dir}/kustomization.yaml" | tr -d '"')"
    if [[ -n "${DRY_RUN}" ]]; then
      run kubectl kustomize "${dir}"
      kubectl_run apply --server-side -f -
    else
      kubectl kustomize "${dir}" | kubectl_run apply --server-side -f -
    fi
    return 0
  fi
  for file in "${dir}"/*.yaml; do
    [[ -e "${file}" ]] || continue
    # release.yaml is the item's metadata, as it is for a helm item, not a manifest.
    [[ "${file##*/}" == release.yaml ]] && continue
    found=1
    kubectl_run apply --server-side -f "${file}"
  done
  [[ -n "${found}" ]] || die "manifest item ${dir} contains no .yaml files"
}

# render_template_item renders the item and applies what comes out.
#
# The output goes to a temp directory rather than over the source: the tree may
# be a read-only extraction of a published artifact, and what is rendered holds
# credentials. Removed as soon as it is applied.
render_template_item() {
  local dir="$1" item="$2" out
  [[ -n "${CONFIG}" ]] || die "item ${item} is type template, which needs --config <BootstrapConfig json>.
Its .yaml.tmpl files render against that context; see
docs/site-bootstrap/deploy-repository.md for the field names."
  out="$(mktemp -d "${TMPDIR:-/tmp}/nvke-deploy.XXXXXX")"
  python3 "${RENDER_PY}" "${CONFIG}" "${dir}" "${out}" >/dev/null
  apply_manifest_item "${out}"
  rm -rf "${out}"
}

# wait_for_requires blocks on the preconditions a phase declares but the chart
# does not own. helm --wait cannot do this: it waits on workloads, and a
# Certificate is not one, so it would return before the CA is issued.
wait_for_requires() {
  local phase="$1" specs="$2" spec kind name namespace condition
  for spec in ${specs}; do
    IFS=: read -r kind name namespace condition <<<"${spec}"
    if [[ -z "${DRY_RUN}" ]] && ! kubectl ${KUBECONFIG_PATH:+--kubeconfig "${KUBECONFIG_PATH}"} \
        -n "${namespace}" get "${kind}" "${name}" >/dev/null 2>&1; then
      die "phase ${phase} requires ${kind}/${name} in namespace ${namespace}, which does not exist.
It is not part of this chart -- an earlier item supplies it. In this repository
that is the nvke-base-config item, from controller/deploy/k8s/oidc-upstream-ca.yaml."
    fi
    log "waiting for ${kind}/${name} condition=${condition}"
    kubectl_run -n "${namespace}" wait --for="condition=${condition}" \
      "${kind}/${name}" --timeout="${REQUIRE_TIMEOUT}"
  done
}

# values_for prints the --values-for paths for an item, newline separated.
values_for() {
  local entry
  for entry in ${VALUES_FOR[@]+"${VALUES_FOR[@]}"}; do
    if [[ "${entry%%=*}" == "$1" ]]; then
      printf '%s\n' "${entry#*=}"
    fi
  done
}

# build_chart_dependencies downloads the archives a path chart's Chart.lock pins
# into its charts/ directory, which a checkout does not carry. helm dependency
# build refuses to run until every repository in Chart.yaml has been added, so
# they are added to a throwaway Helm config, leaving the caller's repository
# list alone -- the same steps as `make nktp-chart-deps`. OCI dependencies need
# no repository entry. A chart without a Chart.lock has nothing to build.
build_chart_dependencies() {
  local chart="$1" helm_home url n=0
  [[ -f "${chart}/Chart.lock" ]] || return 0
  helm_home="$(mktemp -d "${TMPDIR:-/tmp}/nvke-deploy.XXXXXX")"
  local helm_env=(env HELM_REPOSITORY_CONFIG="${helm_home}/repositories.yaml"
    HELM_REPOSITORY_CACHE="${helm_home}/cache")
  log "building chart dependencies of ${chart}"
  while IFS= read -r url; do
    [[ "${url}" == oci://* ]] && continue
    n=$((n + 1))
    run "${helm_env[@]}" helm repo add "dependency-${n}" "${url}"
  done < <(awk '$1 == "repository:" { print $2 }' "${chart}/Chart.yaml" | sort -u)
  run "${helm_env[@]}" helm dependency build "${chart}" --skip-refresh
  rm -rf "${helm_home}"
}

# helm_install retries helm upgrade --install up to 3 times when the chart download gets a 5xx.
helm_install() {
  local attempt err rc
  err="$(mktemp "${TMPDIR:-/tmp}/nvke-deploy.XXXXXX")"
  for attempt in 1 2 3; do
    rc=0
    run helm upgrade --install "$@" 2>"${err}" || rc=$?
    cat "${err}" >&2
    if (( rc == 0 || attempt == 3 )) || ! grep -qE 'failed to fetch .*: 5[0-9][0-9]([^0-9]|$)' "${err}"; then
      rm -f "${err}"
      return "${rc}"
    fi
    log "chart download got a server error; retrying (attempt $((attempt + 1)) of 3)"
    sleep 5
  done
}

# install_single installs one chart as one release. No enable flags and no phase
# loop: a release contains only its own chart, so there is nothing to turn off.
install_helm_item() {
  local dir="$1" item="$2"
  [[ -f "${dir}/release.yaml" ]] || die "helm item ${dir} has no release.yaml"

  # Captured before it is read, not read from a process substitution: the exit
  # status of a process substitution is never checked, so a parser failure
  # would leave every field empty and surface as a bad helm invocation.
  local fields chart_name version repository path release namespace values requires setfile
  if ! fields="$(python3 "${ITEMS_PY}" release "${dir}/release.yaml")"; then
    die "item ${item}: cannot parse ${dir}/release.yaml"
  fi
  IFS=$'\x1f' read -r _ chart_name version repository path release namespace values requires _ _ _ setfile \
    <<<"${fields}"

  # A path inside the deploy path first: a chart directory from the checkout,
  # or the archive a render put under charts/. A rendered deploy path writes a
  # committed chart.path absolute, so it installs from anywhere. Then the
  # repository the chart is published to.
  local ref=()
  if [[ -n "${path}" ]]; then
    if [[ "${path}" == /* ]]; then ref=("${path}"); else ref=("${DEPLOY_PATH}/${path}"); fi
    [[ "${path}" == *.tgz ]] || build_chart_dependencies "${ref[0]}"
  elif [[ "${repository}" == oci://* ]]; then
    ref=("${repository%/}/${chart_name}" --version "${version}")
  elif [[ -n "${repository}" ]]; then
    # A classic index.yaml repository names the chart separately.
    ref=("${chart_name}" --repo "${repository}" --version "${version}")
  else
    die "item ${item} names no chart source; give release.yaml a chart.repository or chart.path"
  fi

  wait_for_requires "${item}" "${requires}"

  # The item's own values first, then whatever the run supplies for it. Helm
  # merges -f in order, so the caller's file wins on any key it names.
  local values_args=() file
  for file in ${values//,/ }; do
    values_args+=(--values "${dir}/${file}")
  done
  while IFS= read -r file; do
    [[ -n "${file}" ]] || continue
    [[ -f "${file}" ]] || die "--values-for ${item}: no such file: ${file}"
    values_args+=(--values "${file}")
  done < <(values_for "${item}")
  # chart.setFile entries are files the operator holds; helm reads them here,
  # so the deploy path carries a path, not the content.
  local entry
  for entry in ${setfile//,/ }; do
    [[ -f "${entry#*=}" ]] || die "item ${item}: setFile ${entry%%=*}: no such file: ${entry#*=}"
    values_args+=(--set-file "${entry}")
  done

  log "item ${item} -> release ${release} (${chart_name}${version:+ ${version}}) in ${namespace}"
  helm_install "${release}" "${ref[@]}" \
    ${KUBECONFIG_PATH:+--kubeconfig "${KUBECONFIG_PATH}"} \
    --namespace "${namespace}" \
    --wait --timeout "${PHASE_TIMEOUT}" \
    ${values_args[@]+"${values_args[@]}"} \
    ${PASSTHROUGH[@]+"${PASSTHROUGH[@]}"}
}

# import_images imports every image images.txt names with a layout into the
# node's containerd, namespace k8s.io, under the reference's repository and
# digest. The repository need not exist anywhere: imagePullPolicy
# IfNotPresent resolves the name from the local store. A line without a
# layout is an image the nodes pull from the registry the reference names.
import_images() {
  local list="${DEPLOY_PATH}/images.txt" ref layout repository ctr line lines=()
  [[ -f "${list}" ]] || return 0
  read -ra ctr <<<"${CTR}"
  # The list is read up front: CTR may be `docker exec -i`, which would
  # otherwise consume the rest of a loop's stdin.
  mapfile -t lines <"${list}"
  for line in ${lines[@]+"${lines[@]}"}; do
    IFS=$'\t' read -r ref layout <<<"${line}"
    [[ -n "${ref}" && -n "${layout}" ]] || continue
    [[ -d "${layout}" ]] || die "image ${ref}: its OCI layout ${layout} is missing"
    repository="${ref%%@*}"
    log "image ${ref}: imported from ${layout}"
    # --local: containerd's own import, which names every manifest of the
    # layout <repository>@<digest>. The transfer import keeps an annotated
    # name and skips that alias, and the workload asks for the alias.
    if [[ -n "${DRY_RUN}" ]]; then
      run tar -c -C "${layout}" .
      run "${ctr[@]}" -n k8s.io images import --local --digests --base-name "${repository}" -
      run "${ctr[@]}" -n k8s.io images ls -q "name==${ref}"
      continue
    fi
    tar -c -C "${layout}" . | "${ctr[@]}" -n k8s.io images import --local --digests --base-name "${repository}" - ||
      die "cannot import ${ref} into containerd from ${layout}"
    "${ctr[@]}" -n k8s.io images ls -q "name==${ref}" </dev/null | grep -qx "${ref}" ||
      die "image ${ref} is not in containerd after the import; the workload cannot resolve it"
  done
}

# kubectl_read is a kubectl read that runs under --dry-run too: the facts
# the render needs come from the cluster, and a dry run prints them rather
# than the call. stderr is returned with the output.
kubectl_read() {
  kubectl ${KUBECONFIG_PATH:+--kubeconfig "${KUBECONFIG_PATH}"} "$@" 2>&1
}

# read_node_arch sets NODE_ARCH to the one architecture the cluster's nodes
# report, as the render's --arch, linux/<arch>. The stack installs for one
# architecture: it selects the release artifacts and the SPIRE TPM attestor
# checksum. No nodes, a node without the label, or two values stop the
# install, as check-site.sh reports them. An unreachable API stops it too,
# except under --dry-run, where the render falls back to this machine's
# architecture and says so.
read_node_arch() {
  local nodes unlabelled archs
  if ! nodes="$(kubectl_read get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.labels.kubernetes\.io/arch}{"\n"}{end}')"; then
    if [[ -n "${DRY_RUN}" ]]; then
      log "cannot list nodes; the dry run renders for this machine's architecture"
      return 0
    fi
    die "cluster: cannot list nodes: $(printf '%s' "${nodes}" | tr -d '\n' | cut -c1-200)"
  fi
  [[ -n "$(printf '%s\n' "${nodes}" | sed '/^$/d')" ]] || die "cluster: no nodes"
  unlabelled="$(printf '%s\n' "${nodes}" | awk -F'\t' 'NF && $2 == "" { print $1 }' | tr '\n' ' ')"
  [[ -z "${unlabelled}" ]] || die "cluster: node ${unlabelled% } has no kubernetes.io/arch label"
  archs="$(printf '%s\n' "${nodes}" | awk -F'\t' 'NF && $2 != "" { print $2 }' | sort -u | tr '\n' ' ')"
  archs="${archs% }"
  [[ "${archs}" != *" "* ]] || die "cluster: nodes report ${archs}; the stack installs for one architecture"
  NODE_ARCH="linux/${archs}"
}

# read_inputs loads the facts the site spec and the stack config carry and
# logs the pair this run uses, with its absolute paths, before anything is
# rendered: the site name, the NICo endpoint and org. An input that does not
# load is left to the render, which names every problem.
read_inputs() {
  local vars inputs
  unset SITE_NAME STACK_NICO_ENDPOINT STACK_NICO_ORG_ID
  if vars="$("${NVKECTL}" inspect sitespec "${SITESPEC}" 2>/dev/null)"; then eval "${vars}"; fi
  if vars="$("${NVKECTL}" inspect stack-config "${STACK_CONFIG}" 2>/dev/null)"; then eval "${vars}"; fi
  inputs="site spec $(abspath "${SITESPEC}")"
  [[ -z "${SITE_NAME:-}" ]] || inputs="${inputs}, site ${SITE_NAME}"
  inputs="${inputs}, stack config $(abspath "${STACK_CONFIG}")"
  [[ -z "${STACK_NICO_ENDPOINT:-}" ]] || inputs="${inputs}, nico ${STACK_NICO_ENDPOINT} org ${STACK_NICO_ORG_ID:-}"
  log "inputs: ${inputs}"
}

# abspath prints path made absolute against the working directory.
abspath() {
  local dir
  if [[ "$1" == /* ]]; then
    printf '%s' "$1"
  elif dir="$(cd "$(dirname "$1")" 2>/dev/null && pwd)"; then
    printf '%s/%s' "${dir}" "$(basename "$1")"
  else
    printf '%s/%s' "${PWD}" "$1"
  fi
}

# render_deploy_path renders DEPLOY_PATH for the site and the release and
# points DEPLOY_PATH at the result, a private scratch directory under the
# state directory (never a shared temp directory, which the render refuses)
# that is removed when this script exits.
render_deploy_path() {
  command -v "${NVKECTL}" >/dev/null 2>&1 || die "nvkectl is needed to render; set NVKECTL or put it on PATH"
  local args=(render deploy-path --deploy-path "${DEPLOY_PATH}" --ephemeral)
  if [[ -n "${SITESPEC}" ]]; then
    read_inputs
    [[ -n "${ARCH}" ]] || read_node_arch
    args+=(--sitespec "${SITESPEC}" --stack-config "${STACK_CONFIG}")
  fi
  [[ -z "${ARCH:-${NODE_ARCH:-}}" ]] || args+=(--arch "${ARCH:-${NODE_ARCH}}")
  [[ -z "${RELEASE}" ]] || args+=(--release "${RELEASE}")
  [[ -z "${RUN_DIR}" ]] || args+=(--nvkr-run-dir "${RUN_DIR}")
  local base="${STATE_DIR}/renders"
  (umask 077 && mkdir -p "${base}") || die "cannot create ${base}; NVKE_STATE_DIR names the state directory"
  SCRATCH="$(mktemp -d "${base}/install.XXXXXX")"
  # Removed however this script ends: a die, a failed render, a signal.
  trap 'rm -rf "${SCRATCH}"' EXIT
  log "rendering ${DEPLOY_PATH}"
  "${NVKECTL}" "${args[@]}" --out "${SCRATCH}/rendered" || die "the render failed"
  DEPLOY_PATH="${SCRATCH}/rendered"
}

install() {
  [[ -n "${DEPLOY_PATH}" ]] || die "--deploy-path is required"
  [[ -f "${DEPLOY_PATH}/nvke-deploy.yaml" ]] \
    || die "no nvke-deploy.yaml in ${DEPLOY_PATH}"

  # Same shape as the release read in install_helm_item: a failed parser must
  # stop the install, not leave it with no items to apply.
  local items name type
  if ! items="$(python3 "${ITEMS_PY}" items "${DEPLOY_PATH}/nvke-deploy.yaml")"; then
    die "cannot parse ${DEPLOY_PATH}/nvke-deploy.yaml"
  fi
  import_images
  while IFS=$'\t' read -r name type; do
    [[ -n "${name}" ]] || continue
    local dir="${DEPLOY_PATH}/${name}"
    [[ -d "${dir}" ]] || die "item ${name} has no directory at ${dir}"
    log "item ${name} (${type})"
    case "${type}" in
      manifest) apply_manifest_item "${dir}" ;;
      helm)     install_helm_item "${dir}" "${name}" ;;
      template) render_template_item "${dir}" "${name}" ;;
      *) die "item ${name} has unknown type ${type}" ;;
    esac
  done <<<"${items}"
}

[[ $# -gt 0 ]] || usage
case "$1" in
  install) shift ;;
  *) usage ;;
esac

while [[ $# -gt 0 ]]; do
  case "$1" in
    --deploy-path) DEPLOY_PATH="${2%/}"; shift 2 ;;
    --sitespec) SITESPEC="$2"; shift 2 ;;
    --stack-config) STACK_CONFIG="$2"; shift 2 ;;
    --arch) ARCH="$2"; shift 2 ;;
    --release) RELEASE="$2"; shift 2 ;;
    --nvkr-run-dir) RUN_DIR="$2"; shift 2 ;;
    --config) CONFIG="$2"; shift 2 ;;
    --values-for) VALUES_FOR+=("$2"); shift 2 ;;
    --kubeconfig) KUBECONFIG_PATH="$2"; shift 2 ;;
    --timeout) PHASE_TIMEOUT="$2"; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    --) shift; PASSTHROUGH=("$@"); break ;;
    -h | --help) usage ;;
    *) die "unknown argument: $1" ;;
  esac
done

[[ -z "${SITESPEC}" || -n "${STACK_CONFIG}" ]] || die "--sitespec needs --stack-config: the stack's input is the stack config file"
[[ -z "${STACK_CONFIG}" || -n "${SITESPEC}" ]] || die "--stack-config needs --sitespec: the site name, VIP and trust domain come from the site spec"
[[ -z "${RUN_DIR}" || -n "${RELEASE}" ]] || die "--nvkr-run-dir holds a release's artifacts; pass --release"
[[ -z "${RELEASE}" || -n "${SITESPEC}" ]] || die "--release needs --sitespec and --stack-config"
case "${ARCH}" in
  "" | linux/amd64 | linux/arm64) ;;
  *) die "--arch ${ARCH} is not linux/amd64 or linux/arm64" ;;
esac
if [[ -n "${SITESPEC}${RELEASE}" ]]; then
  [[ -n "${DEPLOY_PATH}" ]] || die "--deploy-path is required"
  render_deploy_path
fi
install
