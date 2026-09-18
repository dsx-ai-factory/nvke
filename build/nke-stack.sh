#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
# Build the nke-stack umbrella chart from this working tree.
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
# Usage:
#   ./build/nke-stack.sh build-local     package the umbrella from this tree
#   ./build/nke-stack.sh spire-chart     print a materialised spire chart dir
#
# VERSION overrides the git-describe default.
#
# The registry half -- publish-deps, package, template, push, all -- is gone.
# Nothing invoked it: no workflow published the umbrella, and the only caller of
# this script is the CAPD e2e asking for build-local. spire-chart outlives the
# umbrella, because spire is pinned to a commit and cannot be helm-installed by
# coordinates whatever the rest of the stack looks like.
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



case "${1:-build-local}" in
  build-local)  build_local ;;
  spire-chart)  spire_chart ;;
  *) echo "usage: $0 [build-local|spire-chart]" >&2; exit 2 ;;
esac
