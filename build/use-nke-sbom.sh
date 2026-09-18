#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Install a generated deploy path: walk its nke-deploy.yaml and apply the items
# in the order listed. The list IS the order -- see
# docs/site-bootstrap/deploy-repository.md, which this format follows.
#
# Replaces `build/nke-stack.sh install`. The difference that matters is where the
# phase list comes from: that script parsed the chart's values.yaml and built
# cumulative flags, emitting `--set <dep>.enabled=true` for everything up to the
# phase -- which beat the overlay, so a component an overlay disabled came back
# on. Here the phases arrive in release.yaml, generated from the same source as
# the ArgoCD Applications, and only ever as `enabled=false`. Nothing this script
# emits can turn a component on that an overlay turned off.
#
# Both inputs are local paths. The e2e SBOMs are publish: false and are consumed
# in place, so there is no registry, no oras pull and no tarball to extract.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ITEMS_PY="${SCRIPT_DIR}/nke-sbom-items.py"

SBOM=""
TARGET=""
CHART=""
UNTIL=""
DRY_RUN=""
KUBECONFIG_PATH=""
PHASE_TIMEOUT="${PHASE_TIMEOUT:-15m}"
REQUIRE_TIMEOUT="${REQUIRE_TIMEOUT:-180s}"
PASSTHROUGH=()

log() { printf '\n=== %s\n' "$*" >&2; }
die() { printf 'use-nke-sbom: %s\n' "$*" >&2; exit 1; }

usage() {
  cat >&2 <<'USAGE'
usage: use-nke-sbom.sh install --sbom DIR --target NAME [options] [-- helm args]

  --sbom DIR          generated SBOM directory, e.g. ./generated/nke-sbom-e2e
  --target NAME       deploy path inside it, e.g. capd-e2e
  --chart PATH        packaged .tgz or chart directory (required by a helm item)
  --until PHASE       stop after this phase, so a caller can interleave work
  --dry-run           print the argv each step would run, and exit
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
  for file in "${dir}"/*.yaml; do
    [[ -e "${file}" ]] || continue
    found=1
    kubectl_run apply --server-side -f "${file}"
  done
  [[ -n "${found}" ]] || die "manifest item ${dir} contains no .yaml files"
}

# apply_argocd_item creates the Application. Server-side apply of the CR rather
# than `argocd app create --upsert`: same object, no binary, no login.
apply_argocd_item() {
  local dir="$1"
  [[ -f "${dir}/application.yaml" ]] || die "argocd item ${dir} has no application.yaml"
  kubectl_run apply --server-side -f "${dir}/application.yaml"
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
that is the nke-base-config item, from controller/deploy/k8s/oidc-upstream-ca.yaml."
    fi
    log "waiting for ${kind}/${name} condition=${condition}"
    kubectl_run -n "${namespace}" wait --for="condition=${condition}" \
      "${kind}/${name}" --timeout="${REQUIRE_TIMEOUT}"
  done
}

# install_helm_item installs the umbrella one release per phase, or one release
# upgraded once per phase, according to release.yaml's perPhase.
install_helm_item() {
  local dir="$1"
  [[ -n "${CHART}" ]] || die "item ${dir} is type helm, which needs --chart"
  [[ -f "${dir}/release.yaml" ]] || die "helm item ${dir} has no release.yaml"

  local parsed
  local chart_name chart_version prefix namespace per_phase values
  IFS=$'\t' read -r chart_name chart_version prefix namespace per_phase values \
    < <(python3 "${ITEMS_PY}" release "${dir}/release.yaml")

  # The phase list comes from the CHART, where it is declared -- the phases:
  # block of its values.yaml, whose own comment says consumers drive the sequence
  # from it. Copying it into the deploy path would be a second place for it to be
  # wrong. `helm show values` reads a packaged .tgz, an unpacked directory and an
  # OCI ref alike, so this needs to know nothing about how the chart arrived.
  local chart_values
  chart_values="$(mktemp)"
  helm show values "${CHART}" >"${chart_values}"

  # A read loop rather than mapfile: macOS ships bash 3.2, and being able to
  # --dry-run this on a laptop is most of its value.
  local names=() enables=() requires=() i
  local phase_name phase_enable phase_require
  while IFS=$'\t' read -r phase_name phase_enable phase_require; do
    names+=("${phase_name}")
    enables+=("${phase_enable}")
    requires+=("${phase_require}")
  done < <(python3 "${ITEMS_PY}" phases "${chart_values}")
  rm -f "${chart_values}"

  [[ ${#names[@]} -gt 0 ]] || die "chart ${CHART} declares no phases"

  local values_args=() file
  for file in ${values//,/ }; do
    values_args+=(--values "${dir}/${file}")
  done

  for i in "${!names[@]}"; do
    local phase="${names[i]}"
    if [[ -z "${enables[i]}" ]]; then
      log "phase ${phase} enables nothing; skipping"
      continue
    fi

    wait_for_requires "${phase}" "${requires[i]}"

    # Only ever enabled=false, never enabled=true. Under perPhase a release owns
    # exactly its own slice, so everything else is off; otherwise one release
    # grows, so only the phases still ahead of it are off. Saying nothing about
    # the rest is what leaves an overlay's own disables intact.
    local disable="" j skip
    for j in "${!names[@]}"; do
      skip=""
      if [[ "${per_phase}" == "true" ]]; then
        if (( j == i )); then skip=1; fi
      elif (( j <= i )); then
        skip=1
      fi
      if [[ -z "${skip}" ]]; then
        disable+="${enables[j]//,/ } "
      fi
    done

    local flags=() component
    for component in ${disable}; do
      flags+=(--set "${component}.enabled=false")
    done

    local release="${prefix}"
    if [[ "${per_phase}" == "true" ]]; then
      release="${prefix}-${phase}"
    fi

    log "phase ${phase} -> release ${release}"
    run helm upgrade --install "${release}" "${CHART}" \
      ${KUBECONFIG_PATH:+--kubeconfig "${KUBECONFIG_PATH}"} \
      --namespace "${namespace}" \
      --wait --timeout "${PHASE_TIMEOUT}" \
      ${flags[@]+"${flags[@]}"} \
      ${values_args[@]+"${values_args[@]}"} \
      ${PASSTHROUGH[@]+"${PASSTHROUGH[@]}"}

    if [[ -n "${UNTIL}" && "${phase}" == "${UNTIL}" ]]; then
      log "stopping after phase ${phase}"
      return 0
    fi
  done
}

install() {
  [[ -n "${SBOM}" ]] || die "--sbom is required"
  [[ -n "${TARGET}" ]] || die "--target is required"

  local deploy_path="${SBOM}/${TARGET}"
  [[ -f "${deploy_path}/nke-deploy.yaml" ]] \
    || die "no nke-deploy.yaml in ${deploy_path}"

  local name type
  while IFS=$'\t' read -r name type; do
    [[ -n "${name}" ]] || continue
    local dir="${deploy_path}/${name}"
    [[ -d "${dir}" ]] || die "item ${name} has no directory at ${dir}"
    log "item ${name} (${type})"
    case "${type}" in
      manifest) apply_manifest_item "${dir}" ;;
      argocd)   apply_argocd_item "${dir}" ;;
      helm)     install_helm_item "${dir}" ;;
      template)
        die "item ${name} is type template, which needs the Go renderer.
Targets whose base-config carries .yaml.tmpl files cannot be installed by this
script; the e2e targets pass their credentials as --set instead."
        ;;
      *) die "item ${name} has unknown type ${type}" ;;
    esac
  done < <(python3 "${ITEMS_PY}" items "${deploy_path}/nke-deploy.yaml")
}

[[ $# -gt 0 ]] || usage
case "$1" in
  install) shift ;;
  *) usage ;;
esac

while [[ $# -gt 0 ]]; do
  case "$1" in
    --sbom) SBOM="$2"; shift 2 ;;
    --target) TARGET="$2"; shift 2 ;;
    --chart) CHART="$2"; shift 2 ;;
    --until) UNTIL="$2"; shift 2 ;;
    --kubeconfig) KUBECONFIG_PATH="$2"; shift 2 ;;
    --timeout) PHASE_TIMEOUT="$2"; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    --) shift; PASSTHROUGH=("$@"); break ;;
    -h | --help) usage ;;
    *) die "unknown argument: $1" ;;
  esac
done

install
