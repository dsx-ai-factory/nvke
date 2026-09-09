#!/usr/bin/env bash
# Build / publish the nke-stack umbrella chart.
#
# nke-stack composes the NKE management plane: the same component set the CAPD
# e2e deploys through controller/Tiltfile. It is a pure umbrella -- Chart.yaml
# plus values.yaml -- and every dependency resolves from a registry, the same
# shape nke-site-controller uses. generated/nke-stack/charts/ is build output
# and is not committed.
#
# Four dependencies come from public repositories. Two are first-party and have
# to be published before the umbrella can resolve:
#
#   controller             in-repo chart, not published before a controller/v*
#                          tag exists.
#   spire                  upstream chart pinned to a git commit rather than a
#                          release. It carries file://../spire-lib sibling deps,
#                          so the whole helm-charts-hardened repo must be on
#                          disk for `helm dependency build` -- a git-plugin
#                          sparse checkout strips that. Same reason
#                          controller/Tiltfile clones it.
#
# REGISTRY is a parameter, never baked into the committed chart: publish-deps
# pushes there, and package rewrites the two first-party repository fields to
# match before resolving.
#
# Usage:
#   REGISTRY=oci://nvcr.io/j7sbcjl3qgta ./build/nke-stack.sh publish-deps
#   REGISTRY=oci://nvcr.io/j7sbcjl3qgta ./build/nke-stack.sh package
#   REGISTRY=oci://nvcr.io/j7sbcjl3qgta ./build/nke-stack.sh all
#
# VERSION overrides the git-describe default.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHART_SRC="${REPO_ROOT}/generated/nke-stack"
# Builds mutate Chart.yaml -- the registry and the derived version are stamped in
# -- so they run against a copy. The committed chart keeps its canonical
# repository and its 0.0.0 placeholders, and `git status` stays clean after a
# build.
CHART_DIR="${REPO_ROOT}/.local/nke-stack-src"
CWE_CHART="${REPO_ROOT}/controller/deploy/charts/controller"
WORK_DIR="${REPO_ROOT}/.local/nke-stack-build"
OUT_DIR="${OUT_DIR:-${REPO_ROOT}/.local/nke-stack-dist}"

# Pinned in controller/Tiltfile as SPIRE_CHART_REPO_COMMIT (spire chart 0.29.0).
# Bump both together, only after re-testing.
SPIRE_REPO_URL="https://github.com/spiffe/helm-charts-hardened.git"
SPIRE_REPO_COMMIT="585203462b6bc7aa06d67b3a153bfff7ff287173"

log() { printf '\n=== %s\n' "$*" >&2; }

# Refresh the build copy from the committed chart. charts/ is wiped rather than
# reused: the publish path leaves .tgz archives and the local path leaves
# directories, and a charts/ carrying both forms of one dependency makes
# `helm package` ship every template twice.
stage_chart() {
  rm -rf "${CHART_DIR}"
  mkdir -p "${CHART_DIR}/charts"
  # `if`, not `[[ ... ]] && cp`: under `set -e` a false test on the LAST loop
  # iteration makes the loop return non-zero and kills the script silently.
  # .helmignore is optional -- the e2e stages only Chart.yaml and values.yaml --
  # so that is exactly what happened.
  for f in Chart.yaml values.yaml .helmignore; do
    if [[ -f "${CHART_SRC}/${f}" ]]; then
      cp "${CHART_SRC}/${f}" "${CHART_DIR}/${f}"
    fi
  done
  [[ -f "${CHART_DIR}/Chart.yaml" && -f "${CHART_DIR}/values.yaml" ]] || {
    echo "stage_chart: ${CHART_SRC} is missing Chart.yaml or values.yaml" >&2
    exit 1
  }
}

# git describe on the controller lineage, so the chart version and the
# controller image it deploys derive from one commit. The 0.0.0 in Chart.yaml
# is a placeholder for local lint/template only, matching the CWE chart.
resolve_version() {
  if [[ -n "${VERSION:-}" ]]; then printf '%s' "${VERSION#v}"; return; fi
  local d
  # No --dirty: an uncommitted tree is normal while iterating, and the suffix
  # would make the version fail to resolve against a published dependency.
  d="$(cd "${REPO_ROOT}" && git describe --match 'controller/v*' --tags --always 2>/dev/null || true)"
  d="${d#controller/}"
  printf '%s' "${d#v}"
}

require_registry() {
  : "${REGISTRY:?set REGISTRY, e.g. oci://nvcr.io/j7sbcjl3qgta}"
}

# Package and push the two charts the umbrella cannot resolve from anywhere else.
publish_deps() {
  require_registry
  local version staging clone
  version="$(resolve_version)"
  staging="${WORK_DIR}/deps"
  rm -rf "${staging}" && mkdir -p "${staging}"

  # --app-version drives image.tag in the CWE chart, so the composition points
  # at the controller image built from this same commit.
  log "publishing controller ${version}"
  helm package "${CWE_CHART}" --version "${version}" --app-version "v${version}" \
    --destination "${staging}" >/dev/null
  helm push "${staging}/controller-${version}.tgz" "${REGISTRY}"

  clone="${WORK_DIR}/helm-charts-hardened"
  if [[ ! -d "${clone}/.git" ]]; then
    log "cloning helm-charts-hardened"
    mkdir -p "${WORK_DIR}"
    git clone --quiet "${SPIRE_REPO_URL}" "${clone}"
  fi
  git -C "${clone}" fetch --quiet origin "${SPIRE_REPO_COMMIT}" 2>/dev/null || git -C "${clone}" fetch --quiet
  git -C "${clone}" checkout --quiet "${SPIRE_REPO_COMMIT}"

  log "publishing spire from the pinned clone ${SPIRE_REPO_COMMIT:0:12}"
  # Resolve the chart's own file://../spire-lib siblings before packaging.
  helm dependency build "${clone}/charts/spire" >/dev/null
  helm package "${clone}/charts/spire" --destination "${staging}" >/dev/null
  helm push "${staging}"/spire-*.tgz "${REGISTRY}"
}

# Build the chart entirely from this working tree, with no registry writes.
# The two first-party dependencies are vendored into charts/ directly; the four
# public ones still resolve from their upstream repositories.
#
# This is what the CAPD e2e uses. Pulling a published nke-stack would test
# whatever was packaged when it was published, so a pull request editing
# controller/deploy/charts/controller/templates would run the e2e
# green against the OLD templates -- the subcharts are baked into the package.
build_local() {
  stage_chart
  local version staging clone
  version="$(resolve_version)"
  [[ -n "${version}" ]] || {
    echo "build-local: could not resolve a version." >&2
    echo "  git describe found no repository under ${REPO_ROOT}." >&2
    echo "  Set VERSION= explicitly -- the e2e runner holds a tar of the tree, not a clone." >&2
    exit 1
  }
  staging="${WORK_DIR}/local"
  rm -rf "${staging}" && mkdir -p "${staging}" "${CHART_DIR}/charts"

  log "vendoring controller ${version} from the working tree"
  # --app-version drives image.tag, so the composition names the controller
  # image built from this same commit.
  helm package "${CWE_CHART}" --version "${version}" --app-version "v${version}" \
    --destination "${staging}" >/dev/null
  rm -rf "${CHART_DIR}/charts/controller"
  tar xzf "${staging}/controller-${version}.tgz" -C "${CHART_DIR}/charts"


  clone="${WORK_DIR}/helm-charts-hardened"
  if [[ ! -d "${clone}/.git" ]]; then
    log "cloning helm-charts-hardened"
    mkdir -p "${WORK_DIR}"
    git clone --quiet "${SPIRE_REPO_URL}" "${clone}"
  fi
  git -C "${clone}" fetch --quiet origin "${SPIRE_REPO_COMMIT}" 2>/dev/null || git -C "${clone}" fetch --quiet
  git -C "${clone}" checkout --quiet "${SPIRE_REPO_COMMIT}"
  log "vendoring spire from the pinned clone ${SPIRE_REPO_COMMIT:0:12}"
  helm dependency build "${clone}/charts/spire" >/dev/null
  rm -rf "${CHART_DIR}/charts/spire"
  cp -R "${clone}/charts/spire" "${CHART_DIR}/charts/spire"

  # Point the two first-party entries at the vendored copies. An empty
  # repository makes helm resolve them from charts/ rather than a registry.
  python3 "${REPO_ROOT}/build/nke-stack-retarget.py" \
    "${CHART_DIR}/Chart.yaml" "" "${version}"

  log "resolving the public dependencies"
  helm dependency update "${CHART_DIR}" >/dev/null

  mkdir -p "${OUT_DIR}"
  log "packaging nke-stack ${version} (local)"
  helm package "${CHART_DIR}" --version "${version}" --app-version "v${version}" \
    --destination "${OUT_DIR}" >/dev/null
  printf '%s\n' "${OUT_DIR}/nke-stack-${version}.tgz"
}

# Repoint the two first-party dependencies at REGISTRY, and pin
# the controller chart to the version publish-deps just pushed. The committed
# Chart.yaml names the canonical home and a released version; a dev build has
# neither, because chart-publish.yml only fires on a tag.
retarget() {
  require_registry
  python3 "${REPO_ROOT}/build/nke-stack-retarget.py" \
    "${CHART_DIR}/Chart.yaml" "${REGISTRY}" "$(resolve_version)"
}

package() {
  stage_chart
  require_registry
  local version
  version="$(resolve_version)"
  [[ -n "${version}" ]] || { echo "could not resolve a version; set VERSION=" >&2; exit 1; }

  # The chart is generated from controller/deploy/composition, so a stale
  # committed copy would silently build the wrong composition.
  ( cd "${REPO_ROOT}/controller" && go run ./cmd/composition-gen -check >&2 )
  retarget
  log "resolving dependencies from registries"
  helm dependency update "${CHART_DIR}" >/dev/null

  mkdir -p "${OUT_DIR}"
  log "packaging nke-stack ${version}"
  helm package "${CHART_DIR}" --version "${version}" --app-version "v${version}" \
    --destination "${OUT_DIR}" >/dev/null
  printf '%s\n' "${OUT_DIR}/nke-stack-${version}.tgz"
}

# Release namespace. Pinned to temporal: that chart's core templates have no
# namespaceOverride path. See generated/nke-stack/values.yaml.
NAMESPACE="${NAMESPACE:-temporal}"

# Create the release namespace plus every namespace a component is relocated
# into. Separate from install() because a caller may need namespaces to exist
# before it applies its own prerequisites into them.
# PodSecurity levels for the two spire namespaces.
#
# spire-agent and spiffe-csi-driver need hostNetwork, hostPID and privileged
# containers, which the restricted level rejects outright, so they live in
# spire-system while the server stays restricted. That split is the whole
# reason for the two-namespace layout.
#
# The labels have to be applied here because nothing else applies them. The
# upstream spire chart accepts global.spire.recommendations but its Namespace
# templates live in spire-lib, which this composition does not render -- so the
# levels it documents never actually arrive. It has gone unnoticed because kind
# does not enforce Pod Security by default; on a cluster that does, the two
# DaemonSets would be rejected. Matches the AICR bundle's spire-crds-post
# component, which exists solely to create spire-system privileged.
declare -a NS_POD_SECURITY=(
  "spire-system:privileged"
  "spire-server:restricted"
)

create_namespaces() {
  log "creating namespaces"
  local ns entry level
  for ns in "${NAMESPACE}" $(python3 "${REPO_ROOT}/build/nke-stack-namespaces.py" "${CHART_SRC}/values.yaml"); do
    kubectl create namespace "${ns}" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
    level=""
    for entry in "${NS_POD_SECURITY[@]}"; do
      if [[ "${entry%%:*}" == "${ns}" ]]; then level="${entry#*:}"; fi
    done
    if [[ -n "${level}" ]]; then
      kubectl label namespace "${ns}" \
        "pod-security.kubernetes.io/enforce=${level}" --overwrite >/dev/null
      printf '  %s (pod-security: %s)\n' "${ns}" "${level}" >&2
    else
      printf '  %s\n' "${ns}" >&2
    fi
  done
}

install() {
  local chart
  if [[ -n "${CHART:-}" ]]; then
    chart="${CHART}"                       # a local .tgz or directory
  else
    require_registry
    chart="oci://${REGISTRY#oci://}/nke-stack"
  fi

  # helm --create-namespace makes only the release namespace, so each namespace
  # a component is relocated into has to exist first. Idempotent, so a re-run
  # after the caller already created them is a no-op.
  create_namespaces

  # Install phase by phase. Helm never waits mid-release, so one pass races:
  # spire's Certificate would hit cert-manager's webhook before it serves. Each
  # phase enables everything up to it and waits, reproducing the ordering
  # controller/Tiltfile gets from resource_deps. See phases: in values.yaml.
  #
  # Remaining arguments pass through: that is how the caller supplies what
  # controller/Tiltfile sets per deployment -- the postgres password, spire's
  # clusterName/trustDomain/jwtIssuer, the TPM attestor checksum and the OIDC
  # leaf's IP SANs -- plus, for the CAPD e2e, the locally built controller image.
  local phase flags reqs
  while IFS=$'\t' read -r phase flags reqs; do
    [[ -n "${phase}" ]] || continue
    # PHASES_UNTIL stops after the named phase, so a caller can interleave work
    # between phases -- applying manifests that need CRDs an earlier phase
    # installed, for instance. Phases are cumulative, so resuming is just
    # another install with a later (or no) stop point.
    if [[ -n "${stop_after_seen:-}" ]]; then
      break
    fi
    if [[ -n "${PHASES_UNTIL:-}" && "${phase}" == "${PHASES_UNTIL}" ]]; then
      stop_after_seen=1
    fi

    # Preconditions the chart declares but does not own (see requires: in
    # values.yaml). Checked before the phase so a missing prerequisite is a
    # named error here, not a --wait timeout on some unrelated workload ten
    # minutes later. helm --wait cannot do this: it only waits on workloads.
    for req in ${reqs}; do
      IFS=: read -r kind name ns cond <<<"${req}"
      if ! kubectl -n "${ns}" get "${kind}" "${name}" >/dev/null 2>&1; then
        echo "phase ${phase} requires ${kind}/${name} in namespace ${ns}, which does not exist." >&2
        echo "It is not part of this chart. Apply it first -- in this repo that is" >&2
        echo "controller/deploy/k8s/oidc-upstream-ca.yaml." >&2
        exit 1
      fi
      log "waiting for ${kind}/${name} condition=${cond}"
      kubectl -n "${ns}" wait --for="condition=${cond}" \
        "${kind}/${name}" --timeout="${REQUIRE_TIMEOUT:-180s}"
    done

    log "phase ${phase}"
    # shellcheck disable=SC2086  # flags is a deliberately word-split flag list
    helm upgrade --install nke-stack "${chart}" \
      ${VERSION:+--version "${VERSION#v}"} \
      --namespace "${NAMESPACE}" \
      --wait --timeout "${PHASE_TIMEOUT:-15m}" \
      ${flags} \
      "$@"
  done < <(python3 "${REPO_ROOT}/build/nke-stack-phases.py" \
             "${CHART_SRC}/Chart.yaml" "${CHART_SRC}/values.yaml")
}

case "${1:-package}" in
  publish-deps) publish_deps ;;
  package)      package ;;
  template)     tgz="$(package | tail -1)"; helm template nke-stack "${tgz}" --namespace temporal ;;
  push)         tgz="$(package | tail -1)"; log "pushing ${tgz} to ${REGISTRY}"; helm push "${tgz}" "${REGISTRY}" ;;
  all)          publish_deps; tgz="$(package | tail -1)"; log "pushing ${tgz}"; helm push "${tgz}" "${REGISTRY}" ;;
  build-local)  build_local ;;
  namespaces)   create_namespaces ;;
  install)      shift; install "$@" ;;
  *) echo "usage: $0 [publish-deps|build-local|package|template|push|all|install|namespaces]" >&2; exit 2 ;;
esac
