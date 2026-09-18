#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
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
# Materialise the upstream spire chart and print its directory.
#
# spire is pinned to a COMMIT, not a release: global.spire.jwtIssuer has no
# upstream release yet, and oci://ghcr.io/dsx-ai-factory/nke/charts/spire:0.29.0
# does not exist. So nothing can `helm install` it by coordinates -- it has to be
# cloned and have its file://../spire-lib siblings resolved first.
#
# Printed on stdout so a caller can hand it straight to
# `use-nke-sbom.sh --chart-override spire=...`; all logging goes to stderr.
spire_chart() {
  local clone="${WORK_DIR}/helm-charts-hardened"
  if [[ ! -d "${clone}/.git" ]]; then
    log "cloning helm-charts-hardened"
    mkdir -p "${WORK_DIR}"
    git clone --quiet "${SPIRE_REPO_URL}" "${clone}"
  fi
  git -C "${clone}" fetch --quiet origin "${SPIRE_REPO_COMMIT}" 2>/dev/null || git -C "${clone}" fetch --quiet
  git -C "${clone}" checkout --quiet "${SPIRE_REPO_COMMIT}"
  helm dependency build "${clone}/charts/spire" >/dev/null
  printf '%s\n' "${clone}/charts/spire"
}

publish_deps() {
  require_registry
  local version staging
  version="$(resolve_version)"
  staging="${WORK_DIR}/deps"
  rm -rf "${staging}" && mkdir -p "${staging}"

  # --app-version drives image.tag in the CWE chart, so the composition points
  # at the controller image built from this same commit.
  log "publishing controller ${version}"
  helm package "${CWE_CHART}" --version "${version}" --app-version "v${version}" \
    --destination "${staging}" >/dev/null
  helm push "${staging}/nke-controller-${version}.tgz" "${REGISTRY}"

  log "publishing spire from the pinned clone ${SPIRE_REPO_COMMIT:0:12}"
  helm package "$(spire_chart)" --destination "${staging}" >/dev/null
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
  local version staging
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
  rm -rf "${CHART_DIR}/charts/nke-controller"
  tar xzf "${staging}/nke-controller-${version}.tgz" -C "${CHART_DIR}/charts"


  log "vendoring spire from the pinned clone ${SPIRE_REPO_COMMIT:0:12}"
  rm -rf "${CHART_DIR}/charts/spire"
  cp -R "$(spire_chart)" "${CHART_DIR}/charts/spire"

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

case "${1:-package}" in
  publish-deps) publish_deps ;;
  package)      package ;;
  template)     tgz="$(package | tail -1)"; helm template nke-stack "${tgz}" --namespace temporal ;;
  push)         tgz="$(package | tail -1)"; log "pushing ${tgz} to ${REGISTRY}"; helm push "${tgz}" "${REGISTRY}" ;;
  all)          publish_deps; tgz="$(package | tail -1)"; log "pushing ${tgz}"; helm push "${tgz}" "${REGISTRY}" ;;
  build-local)  build_local ;;
  spire-chart)  spire_chart ;;
  *) echo "usage: $0 [publish-deps|build-local|spire-chart|package|template|push|all]" >&2; exit 2 ;;
esac
