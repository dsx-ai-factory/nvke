#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Upgrade a pre-rename install, in a cluster, and assert it kept every object.
#
# The nvke rename moved the chart's defaults. Existing sites keep the names they
# already run by pinning values, and until now the only thing that checked those
# pins was `helm template` (controller/hack/chart-identity-assertions.py). A
# template cannot see the API server refuse an immutable spec.selector, cannot
# see an object get recreated under the same name, and cannot see a Deployment
# get replaced rather than rolled. This lane can, because it does the upgrade
# for real:
#
#   1. export the chart at CHART_UPGRADE_FROM_REF, whose DEFAULTS rendered the
#      legacy names
#   2. `helm install` it, under the release name a site actually runs
#   3. assert the result is a pre-rename install (so a bumped ref that already
#      moved cannot leave this lane green and empty)
#   4. `helm upgrade` to the chart in this checkout, with the pinned values from
#      hack/testdata/chart-legacy-pinned-values.yaml -- the file a site's own
#      values are modelled on
#   5. compare the cluster against itself
#
# Deliberately NOT here: a tenant cluster. #1496's "and its tenant clusters"
# needs a full CAPD bring-up and belongs in the backend-e2e lane; the failure
# this script hunts is an apply-time failure, which needs no running controller.
# The pods never become Ready -- the image is not pulled -- and nothing here
# waits for them.
#
# Needs: kind (unless KUBECONFIG names a cluster already), kubectl, helm, git,
# python3 with PyYAML. No registry credentials: the old chart comes out of this
# repository's own history.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
ASSERT="${REPO}/controller/hack/chart-upgrade-identity-assertions.py"
CHART="${REPO}/controller/deploy/charts/controller"
BEFORE_VALUES="${REPO}/controller/hack/testdata/chart-upgrade-before-values.yaml"
PINNED_VALUES="${REPO}/controller/hack/testdata/chart-legacy-pinned-values.yaml"
STUB_CRD="${REPO}/controller/hack/testdata/servicemonitor-stub-crd.yaml"

# The release name and namespace a site that predates the rename runs. Not
# configurable: the release name feeds app.kubernetes.io/instance, which is IN
# the Deployment's immutable selector, so installing under any other name would
# make the upgrade trivially succeed and prove nothing.
RELEASE="cloud-workflow-engine"
NAMESPACE="nke-system"

# The ref whose chart defaults render the legacy names. make/tool-versions.mk
# owns it so there is one place to look and one line to bump.
# MAKE is set when make invoked this script, so `make chart-upgrade-identity-check`
# re-enters the same make. A bare run on macOS needs gmake, which is why this is
# not hard-coded: the root Makefile refuses GNU Make 3.81.
FROM_REF="${CHART_UPGRADE_FROM_REF:-$("${MAKE:-make}" -C "${REPO}" --no-print-directory print-CHART_UPGRADE_FROM_REF)}"

CLUSTER="${KIND_CLUSTER_NAME:-nvke-chart-upgrade}"
WORK="${WORK:-$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/chart-upgrade-identity.XXXXXX")}"
KEEP_CLUSTER="${KEEP_CLUSTER:-false}"

log() { printf '\n=== %s\n' "$*" >&2; }
die() { printf 'chart-upgrade-identity: %s\n' "$*" >&2; exit 1; }

for tool in kubectl helm git python3; do
  command -v "${tool}" >/dev/null || die "${tool} is not on PATH"
done
python3 -c 'import yaml' 2>/dev/null || die "python3 needs PyYAML (pip install pyyaml)"

mkdir -p "${WORK}"

# Only a cluster this run created is deleted. A cluster that was already there
# -- the caller's, or another run's -- is used and left alone.
CREATED_CLUSTER=false
cleanup() {
  if [[ "${CREATED_CLUSTER}" == true && "${KEEP_CLUSTER}" != true ]]; then
    kind delete cluster --name "${CLUSTER}" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

if [[ -z "${KUBECONFIG:-}" ]]; then
  command -v kind >/dev/null || die "kind is not on PATH and KUBECONFIG is unset; one or the other is needed"
  export KUBECONFIG="${WORK}/kubeconfig"
  log "creating kind cluster ${CLUSTER}"
  kind create cluster --name "${CLUSTER}" --wait 120s
  CREATED_CLUSTER=true
fi
kubectl cluster-info >/dev/null || die "the cluster in KUBECONFIG is not reachable"

# The chart renders a ServiceMonitor, whose name is one of the identities under
# test. Without the CRD the apply is rejected and the lane never gets to the
# upgrade.
log "installing the ServiceMonitor stub CRD"
kubectl apply -f "${STUB_CRD}"
kubectl wait --for=condition=Established --timeout=60s \
  crd/servicemonitors.monitoring.coreos.com

log "exporting the chart at ${FROM_REF}"
OLD_TREE="${WORK}/from-ref"
mkdir -p "${OLD_TREE}"
# `git archive <ref> -- <path>` rather than a checkout or a worktree: it touches
# no index and no HEAD, so this is safe to run from a dirty tree and from a
# session that is already in a worktree of its own.
git -C "${REPO}" archive "${FROM_REF}^{commit}" -- controller/deploy/charts/controller \
  | tar -x -C "${OLD_TREE}" \
  || die "cannot export controller/deploy/charts/controller at ${FROM_REF}; is the tag fetched? (git fetch --tags)"
OLD_CHART="${OLD_TREE}/controller/deploy/charts/controller"
[[ -f "${OLD_CHART}/Chart.yaml" ]] || die "${FROM_REF} has no chart at controller/deploy/charts/controller"

# --wait is deliberately absent, here and on the upgrade. Nothing in this lane
# pulls the controller image, so no pod will ever be Ready; waiting would just
# spend the timeout and then fail for a reason that is not the one under test.
log "installing the pre-rename chart as release ${RELEASE} in ${NAMESPACE}"
helm install "${RELEASE}" "${OLD_CHART}" \
  --namespace "${NAMESPACE}" --create-namespace \
  --values "${BEFORE_VALUES}"

# The Deployment's ReplicaSet is created by the deployment controller, not by
# Helm, and the "did it roll" comparison is meaningless until it exists.
log "waiting for the Deployment to be observed"
kubectl -n "${NAMESPACE}" wait --for=jsonpath='{.status.observedGeneration}'=1 \
  --timeout=120s "deployment/${RELEASE}"

log "snapshotting the install"
python3 "${ASSERT}" snapshot \
  --release "${RELEASE}" --namespace "${NAMESPACE}" --out "${WORK}/before.json"

log "checking the baseline really is a pre-rename install"
python3 "${ASSERT}" check-baseline --snapshot "${WORK}/before.json"

# This is the step a site performs, and the one that fails on a renamed object:
# the chart from this checkout, with the values the site pins.
log "upgrading to the chart in this checkout, with the pinned legacy values"
UPGRADE_RC=0
helm upgrade "${RELEASE}" "${CHART}" \
  --namespace "${NAMESPACE}" \
  --values "${PINNED_VALUES}" || UPGRADE_RC=$?

# The snapshot and the comparison run even when the upgrade failed. A partial
# upgrade leaves some objects applied and some not, and which ones is exactly
# what a reader needs; exiting here would throw that away and report only
# "helm upgrade failed".
if (( UPGRADE_RC != 0 )); then
  printf '\n!!! helm upgrade exited %d -- snapshotting anyway, the comparison below says what moved\n' \
    "${UPGRADE_RC}" >&2
fi

log "snapshotting the upgrade"
python3 "${ASSERT}" snapshot \
  --release "${RELEASE}" --namespace "${NAMESPACE}" \
  --out "${WORK}/after.json" --include-from "${WORK}/before.json"

log "comparing"
COMPARE_RC=0
python3 "${ASSERT}" compare --before "${WORK}/before.json" --after "${WORK}/after.json" || COMPARE_RC=$?

if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
  {
    echo "### Chart upgrade identity"
    echo
    echo "Upgraded \`${RELEASE}\` in \`${NAMESPACE}\` from \`${FROM_REF}\` to this checkout."
    echo "\`helm upgrade\` exited ${UPGRADE_RC}; the comparison exited ${COMPARE_RC}."
  } >> "${GITHUB_STEP_SUMMARY}"
fi

# Snapshots are the artifact worth keeping: they are what a reader diffs when
# this fails on a change they did not expect to touch the chart's identities.
if [[ -n "${GITHUB_ENV:-}" ]]; then
  echo "CHART_UPGRADE_WORK=${WORK}" >> "${GITHUB_ENV}"
fi

(( UPGRADE_RC == 0 )) || die "helm upgrade failed (exit ${UPGRADE_RC}); see the comparison above"
(( COMPARE_RC == 0 )) || exit "${COMPARE_RC}"

log "the upgrade preserved the install"
