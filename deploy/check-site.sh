#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Reports whether this machine and a bootstrapped cluster are ready for the
# management plane stack. It changes nothing. Every finding is one line, PASS,
# WARN, SKIP or FAIL, and a FAIL names the fix. Any FAIL fails the run.
#
#   check-site.sh --sitespec FILE --stack-config FILE --kubeconfig PATH [--release FILE] [--nvkr-run-dir DIR]
#   check-site.sh --kubeconfig PATH --diagnostics
#
# --diagnostics writes what a failed step left in the cluster (nodes, pods,
# events, helm releases, controller logs) under NVKE_STATE_DIR/diagnostics
# and exits 0.
#
# What the stack needs and this check reports, the operator supplies: the
# control-plane taint is removed by nvkectl bootstrap site, the default
# StorageClass is the hardware's CSI driver, Cluster API is installed with
# the clusterctl init this check prints. docs/get-started/local-site.md
# lists the steps in order.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
# shellcheck source=lib/common.sh
. "${SCRIPT_DIR}/lib/common.sh"

SITESPEC=""
STACK_CONFIG=""
RELEASE=""
RUN_DIR=""
KUBECONFIG_PATH=""
DIAGNOSTICS=""
NVKECTL="${NVKECTL:-nvkectl}"
DEPLOY_PATH="${SCRIPT_DIR}/nico"
STATE_DIR="${NVKE_STATE_DIR:-${XDG_STATE_HOME:-${HOME}/.local/state}/nvke}"
CONTROLLER_NAMESPACE="${CONTROLLER_NAMESPACE:-nvke-controller}"
CAPI_VERSION="${CAPI_VERSION:-$(mk_var CAPI_VERSION)}"
SITE_NAME=""
NODE_ARCH=""
CAPI_INIT=""
HAVE_NVKECTL=""

EGRESS_URLS=(
  https://ghcr.io/v2/
  https://github.com/
  https://api.github.com/
  https://objects.githubusercontent.com/
  https://kubernetes-sigs.github.io/metrics-server/index.yaml
  https://open-telemetry.github.io/opentelemetry-helm-charts/index.yaml
  https://prometheus-community.github.io/helm-charts/index.yaml
  https://quay.io/v2/
  https://registry.k8s.io/v2/
)

usage() {
  cat >&2 <<'USAGE'
usage: check-site.sh --sitespec FILE --stack-config FILE --kubeconfig PATH [--release FILE] [--nvkr-run-dir DIR]
       check-site.sh --kubeconfig PATH --diagnostics

Reports, one line per finding, whether this machine and the cluster are
ready for the management plane stack. Changes nothing; exits non-zero on a
FAIL. --diagnostics instead writes nodes, pods, events, helm releases and
the controller logs under NVKE_STATE_DIR/diagnostics (default
$XDG_STATE_HOME/nvke/diagnostics).

  --sitespec FILE     the site spec the bootstrap ran with
  --stack-config FILE the stack config, the stack's input for this site; start
                      from deploy/examples/stack-config.json. The NVKECTL_*
                      credentials the install reads must be exported
  --kubeconfig PATH   the bootstrapped site cluster
  --release FILE      the NVKR release description the install will use
  --nvkr-run-dir DIR  with --release, the local NVKR Run the artifacts come from
  --diagnostics       write the cluster diagnostics and exit
USAGE
  exit 2
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --sitespec) SITESPEC="$2"; shift 2 ;;
    --stack-config) STACK_CONFIG="$2"; shift 2 ;;
    --kubeconfig) KUBECONFIG_PATH="$2"; shift 2 ;;
    --release) RELEASE="$2"; shift 2 ;;
    --nvkr-run-dir) RUN_DIR="$2"; shift 2 ;;
    --diagnostics) DIAGNOSTICS=1; shift ;;
    -h | --help) usage ;;
    *) die "unknown argument: $1" ;;
  esac
done
[[ -n "${KUBECONFIG_PATH}" ]] || usage

if [[ -n "${DIAGNOSTICS}" ]]; then
  (umask 077 && mkdir -p "${STATE_DIR}")
  diagnostics
  exit 0
fi
[[ -n "${SITESPEC}${STACK_CONFIG}" ]] || usage
[[ -z "${SITESPEC}" || -n "${STACK_CONFIG}" ]] || die "--sitespec needs --stack-config: the stack's input is the stack config file"
[[ -z "${STACK_CONFIG}" || -n "${SITESPEC}" ]] || die "--stack-config needs --sitespec: the site name, VIP and trust domain come from the site spec"
[[ -z "${RUN_DIR}" || -n "${RELEASE}" ]] || die "--nvkr-run-dir holds a release's artifacts; pass --release"

# kread is a kubectl read whose failure is a finding: a denied, timed-out or
# flaky read must not pass as an empty answer. Its stderr lands in KREAD_ERR.
KREAD_ERR="$(mktemp "${TMPDIR:-/tmp}/check-site.XXXXXX")"
trap 'rm -f "${KREAD_ERR}"' EXIT
kread() { kubectl --kubeconfig "${KUBECONFIG_PATH}" "$@" 2>"${KREAD_ERR}"; }
kread_err() { tr -d '\n' <"${KREAD_ERR}" | cut -c1-200; }
# q quotes one word for a command the operator will paste.
q() { printf '%q' "$1"; }

# --- this machine ------------------------------------------------------------

for tool in kubectl helm clusterctl curl python3; do
  if command -v "${tool}" >/dev/null 2>&1; then
    check_pass "tool ${tool}: $(command -v "${tool}")"
  else
    check_fail "tool ${tool}: not found in PATH"
  fi
done
if command -v "${NVKECTL}" >/dev/null 2>&1; then
  check_pass "tool nvkectl: $(command -v "${NVKECTL}")"
  HAVE_NVKECTL=1
else
  check_fail "tool nvkectl: ${NVKECTL} not found; set NVKECTL or put it on PATH (make -C cli build). The site spec, the stack config and the release are not checked without it"
  HAVE_NVKECTL=""
fi
pin="$(mk_var HELM_VERSION)"
have="$(helm version --template '{{.Version}}' 2>/dev/null || true)"
if [[ -z "${pin}" || "${have}" == "${pin}" ]]; then
  check_pass "helm ${have:-unknown} matches the pin in make/tool-versions.mk"
else
  check_warn "helm ${have:-unknown}; the repository pins ${pin} in make/tool-versions.mk"
fi
for f in "${DEPLOY_PATH}/nvke-deploy.yaml" "${REPO_ROOT}/controller/deploy/charts/controller/Chart.yaml" "${REPO_ROOT}/nktp/deploy/charts/nktp/Chart.yaml"; do
  if [[ -f "${f}" ]]; then
    check_pass "checkout: ${f}"
  else
    check_fail "checkout: ${f} is missing; the executor installs the controller and NKTP charts from the tree"
  fi
done
if [[ -n "${CAPI_VERSION}" ]]; then
  check_pass "Cluster API: ${CAPI_VERSION} is the version to install"
else
  check_fail "Cluster API: no CAPI_VERSION; make/tool-versions.mk pins it, or set CAPI_VERSION"
fi
binary="$(clusterctl version -o short 2>/dev/null || true)"
[[ -z "${CAPI_VERSION}" || "${binary}" == "${CAPI_VERSION}" ]] ||
  check_warn "clusterctl ${binary:-unknown} does not match CAPI_VERSION ${CAPI_VERSION}; it decides the cert-manager version"

# --- the inputs --------------------------------------------------------------

# Both files are read first, so the pair this run uses is said before any
# finding, with the facts a wrong file would get wrong: the site name and
# the NICo endpoint and org.
site_valid=""
site_read=""
stack_valid=""
stack_problem=""
unset STACK_NICO_ENDPOINT STACK_NICO_ORG_ID
if [[ -n "${HAVE_NVKECTL}" ]]; then
  if "${NVKECTL}" validate sitespec -f "${SITESPEC}" >/dev/null 2>&1; then
    site_valid=1
    if site_vars="$("${NVKECTL}" inspect sitespec "${SITESPEC}" 2>/dev/null)"; then
      eval "${site_vars}"
      site_read=1
    fi
  fi
  # inspect stack-config checks the file and the NVKECTL_* credentials.
  if stack_vars="$("${NVKECTL}" inspect stack-config "${STACK_CONFIG}" 2>"${KREAD_ERR}")"; then
    eval "${stack_vars}"
    stack_valid=1
  else
    # The first problem; the command names every one of them. A command
    # that said nothing, killed or otherwise, is a finding too.
    stack_problem="$(grep -v -e '^Error: ' -e '^Trace ID: ' "${KREAD_ERR}" | head -n 1 || true)"
    [[ -n "${stack_problem}" ]] || stack_problem="nvkectl inspect stack-config exited non-zero without a reason"
  fi
fi
inputs="site spec $(abspath "${SITESPEC}")"
[[ -z "${SITE_NAME}" ]] || inputs="${inputs}, site ${SITE_NAME}"
inputs="${inputs}, stack config $(abspath "${STACK_CONFIG}")"
[[ -z "${STACK_NICO_ENDPOINT:-}" ]] || inputs="${inputs}, nico ${STACK_NICO_ENDPOINT} org ${STACK_NICO_ORG_ID:-}"
check_info "inputs: ${inputs}"

if [[ -z "${HAVE_NVKECTL}" ]]; then
  check_skip "site spec: ${SITESPEC} (no nvkectl)"
  check_skip "stack config: ${STACK_CONFIG} (no nvkectl)"
else
  if [[ -z "${site_valid}" ]]; then
    check_fail "site spec: ${SITESPEC} is not valid; run: $(q "${NVKECTL}") validate sitespec -f $(q "${SITESPEC}")"
  elif [[ -z "${site_read}" ]]; then
    check_fail "site spec: cannot read ${SITESPEC}; run: $(q "${NVKECTL}") inspect sitespec $(q "${SITESPEC}")"
  else
    check_pass "site spec: ${SITESPEC} is valid"
  fi
  if [[ -n "${stack_valid}" ]]; then
    check_pass "stack config: ${STACK_CONFIG}"
  else
    check_fail "stack config: ${STACK_CONFIG} is not ready: ${stack_problem}; run: $(q "${NVKECTL}") inspect stack-config $(q "${STACK_CONFIG}")"
  fi
fi
if [[ -n "${RELEASE}" && -z "${HAVE_NVKECTL}" ]]; then
  check_skip "release: ${RELEASE} (no nvkectl)"
elif [[ -n "${RELEASE}" ]]; then
  # The description says what the release holds, not that it was verified;
  # nvkr release verify decides that for the same reference.
  unset REFERENCE QUALIFICATION
  if release_vars="$("${NVKECTL}" inspect nvkr-release "${RELEASE}" --meta 2>/dev/null)" && eval "${release_vars}" && [[ -n "${REFERENCE:-}" ]]; then
    check_pass "release: ${REFERENCE} (declared ${QUALIFICATION:-?}); verify it with: nvkr release verify --reference ${REFERENCE}"
    [[ "${QUALIFICATION:-}" == "pipeline-attested" ]] || check_warn "release qualification is ${QUALIFICATION:-unknown}, not pipeline-attested, as the publisher declared it"
  else
    check_fail "release: ${RELEASE} does not parse; run: $(q "${NVKECTL}") inspect nvkr-release $(q "${RELEASE}") --meta"
  fi
fi
if [[ -n "${RUN_DIR}" ]]; then
  if [[ -d "${RUN_DIR}" ]]; then
    check_pass "run directory: ${RUN_DIR}"
  else
    check_fail "run directory: ${RUN_DIR} does not exist"
  fi
fi

# --- the cluster -------------------------------------------------------------

if ! kubectl --kubeconfig "${KUBECONFIG_PATH}" version >/dev/null 2>&1; then
  check_fail "cluster: API not reachable with ${KUBECONFIG_PATH}"
else
  check_pass "cluster: API reachable with ${KUBECONFIG_PATH}"
  if ! nodes="$(kread get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.labels.kubernetes\.io/arch}{"\t"}{.spec.taints[?(@.key=="node-role.kubernetes.io/control-plane")].effect}{"\n"}{end}')"; then
    check_fail "cluster: cannot list nodes: $(kread_err)"
  else
    count="$(printf '%s\n' "${nodes}" | sed '/^$/d' | wc -l | tr -d ' ')"
    if [[ "${count}" -ge 1 && $((count % 2)) -eq 1 ]]; then
      check_pass "cluster: ${count} node(s), an odd number"
    else
      check_fail "cluster: ${count} node(s); expected an odd number, at least 1"
    fi
    # The stack installs for one architecture, read from the nodes: it
    # selects the release artifacts and the SPIRE TPM attestor checksum. The
    # executor makes the same read and passes the result to the render.
    unlabelled="$(printf '%s\n' "${nodes}" | awk -F'\t' 'NF && $2 == "" { print $1 }' | tr '\n' ' ')"
    archs="$(printf '%s\n' "${nodes}" | awk -F'\t' 'NF && $2 != "" { print $2 }' | sort -u | tr '\n' ' ')"
    archs="${archs% }"
    if [[ "${count}" -eq 0 ]]; then
      check_fail "cluster: no nodes"
    elif [[ -n "${unlabelled}" ]]; then
      check_fail "cluster: node ${unlabelled% } has no kubernetes.io/arch label"
    elif [[ "${archs}" == *" "* ]]; then
      check_fail "cluster: nodes report ${archs}; the stack installs for one architecture"
    else
      NODE_ARCH="${archs}"
      check_pass "cluster: nodes are ${NODE_ARCH}"
    fi
    # nvkectl bootstrap site removes the control-plane taint; nothing
    # schedules while it is on.
    tainted="$(printf '%s\n' "${nodes}" | awk -F'\t' '$3 ~ /(^| )NoSchedule( |$)/ { print $1 }' | tr '\n' ' ')"
    if [[ -z "${tainted}" ]]; then
      check_pass "cluster: no node carries the control-plane NoSchedule taint"
    else
      check_fail "cluster: ${tainted% } still carries the control-plane NoSchedule taint, which the bootstrap removes; run: kubectl taint nodes <node> node-role.kubernetes.io/control-plane:NoSchedule-"
    fi
  fi
  # Which provisioner is right is a property of the hardware, not of NVKE.
  if ! classes="$(kread get storageclass -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.annotations.storageclass\.kubernetes\.io/is-default-class}{"\n"}{end}')"; then
    check_fail "cluster: cannot list StorageClasses: $(kread_err)"
  else
    defaults="$(printf '%s\n' "${classes}" | awk -F'\t' '$2 == "true" { print $1 }')"
    count="$(printf '%s\n' "${defaults}" | sed '/^$/d' | wc -l | tr -d ' ')"
    if [[ "${count}" -eq 1 ]]; then
      check_pass "cluster: default StorageClass ${defaults}"
    elif [[ "${count}" -eq 0 ]]; then
      check_fail "cluster: no default StorageClass; Postgres and SPIRE need persistent volumes. Install the CSI driver for this hardware and annotate its class storageclass.kubernetes.io/is-default-class=true (a lab can use rancher local-path-provisioner)"
    else
      check_fail "cluster: ${count} default StorageClasses; the stack needs exactly one"
    fi
  fi
  # clusterctl init does not upgrade, so a Cluster API of another version is
  # a finding, not something to install over. Only NotFound means absent.
  CAPI_INIT="$(printf '%q ' clusterctl init --kubeconfig "${KUBECONFIG_PATH}" --core "cluster-api:${CAPI_VERSION}" --bootstrap "kubeadm:${CAPI_VERSION}" --control-plane "kubeadm:${CAPI_VERSION}" --wait-providers)"
  CAPI_INIT="${CAPI_INIT% }"
  if image="$(kread -n capi-system get deployment capi-controller-manager -o jsonpath='{.spec.template.spec.containers[0].image}')"; then
    # The tag, without a digest suffix or a registry port. An image pinned
    # by digest alone carries no version to compare.
    name="${image%@*}"; tag="${name##*/}"
    if [[ "${tag}" == *:* ]]; then tag="${tag##*:}"; else tag=""; fi
    if [[ -z "${tag}" ]]; then
      check_fail "Cluster API: ${image} is installed, pinned by digest; its version cannot be read, and the stack is tested with ${CAPI_VERSION}"
      CAPI_INIT=""
    elif [[ "${tag}" != "${CAPI_VERSION}" ]]; then
      check_fail "Cluster API: ${tag} is installed; the stack is tested with ${CAPI_VERSION}, and clusterctl init does not upgrade. Run: clusterctl upgrade apply --kubeconfig $(q "${KUBECONFIG_PATH}") --core cluster-api:${CAPI_VERSION} --bootstrap kubeadm:${CAPI_VERSION} --control-plane kubeadm:${CAPI_VERSION}"
      CAPI_INIT=""
    else
      # The kubeadm providers come with the same init; a core without them is
      # a partial install the stack cannot use.
      missing=""; read_err=""
      for d in capi-kubeadm-bootstrap-system/capi-kubeadm-bootstrap-controller-manager capi-kubeadm-control-plane-system/capi-kubeadm-control-plane-controller-manager; do
        if ! kread -n "${d%%/*}" get deployment "${d#*/}" -o name >/dev/null; then
          if grep -q 'NotFound' "${KREAD_ERR}"; then missing="${missing}${d} "; else read_err=1; check_fail "cluster: cannot read deployment ${d}: $(kread_err)"; fi
        fi
      done
      if [[ -n "${read_err}" ]]; then
        CAPI_INIT=""
      elif [[ -z "${missing}" ]]; then
        check_pass "Cluster API: ${CAPI_VERSION} already installed"
        CAPI_INIT=""
      else
        check_fail "Cluster API: ${CAPI_VERSION} core is installed without ${missing% }; run: ${CAPI_INIT}"
        CAPI_INIT=""
      fi
    fi
  elif grep -q 'NotFound' "${KREAD_ERR}"; then
    check_pass "Cluster API: not installed yet; run: ${CAPI_INIT}"
  else
    check_fail "cluster: cannot read deployment capi-system/capi-controller-manager: $(kread_err)"
    CAPI_INIT=""
  fi
fi

# --- egress and disk ---------------------------------------------------------

for url in "${EGRESS_URLS[@]}"; do
  if curl -sSI --max-time 10 -o /dev/null "${url}" 2>/dev/null; then
    check_pass "egress: ${url}"
  else
    check_warn "egress: ${url} not reachable within 10s; the charts and images that come from it will not either"
  fi
done
(umask 077 && mkdir -p "${STATE_DIR}") || true
avail_kb="$(df -Pk "${STATE_DIR}" 2>/dev/null | awk 'NR == 2 { print $4 }' || true)"
if [[ -n "${avail_kb}" && $((avail_kb / 1024 / 1024)) -lt 10 ]]; then
  check_warn "disk: $((avail_kb / 1024 / 1024)) GB free under ${STATE_DIR}; the render and diagnostics want at least 10 GB"
fi

[[ "${CHECK_FAILURES}" -eq 0 ]] || die "${CHECK_FAILURES} finding(s) to fix before the stack installs"
printf 'ready. Next:\n'
[[ -z "${CAPI_INIT:-}" ]] || printf '  %s\n' "${CAPI_INIT}"
install_cmd=("${SCRIPT_DIR}/nvke-deploy.sh" install --deploy-path "${DEPLOY_PATH}" --sitespec "${SITESPEC}" --stack-config "${STACK_CONFIG}" --kubeconfig "${KUBECONFIG_PATH}")
[[ -z "${RELEASE}" ]] || install_cmd+=(--release "${RELEASE}")
[[ -z "${RUN_DIR}" ]] || install_cmd+=(--nvkr-run-dir "${RUN_DIR}")
install_line="$(printf '%q ' "${install_cmd[@]}")"
printf '  %s\n' "${install_line% }"
printf '  # with your own telemetry backend, add: --values-for nktp=<file>\n'
