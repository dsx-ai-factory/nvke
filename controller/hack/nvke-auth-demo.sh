#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# The acceptance test for NVKE API authentication: a handful of curls, one table.
# See usage() below, or run with --help.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

usage() {
  cat <<'EOF'
nvke-auth-demo.sh [--mode enforcing|permissive]

Drives NVKE's API with four credentials and prints the status code each one got.
The three authentication states are documented on the auth/authn package and
in the authentication section of README.md.

Enforcing (the default deployment):

  no token                                 -> 401
  nke-clients/nke-client, correct audience -> 200
  nke-clients/nke-client, wrong audience   -> 401
  nke-outsiders/nke-outsider, correct aud  -> 403

Permissive (NKE_PERMISSIVE_MODE=true tilt up):

  all four return 200, and nvke_auth_requests_total{outcome="would_reject"}
  advances by 3 across the run, which is the only evidence permissive mode
  produces. That counter check is the extra case in a permissive run.

The mode is read from the live Deployment's AUTH_CONFIG_PATH and the
permissiveMode key of its controller config ConfigMap, so the same script covers both
deployments. Override it with --mode.

Scaffolding lives in deploy/k8s/nvke-auth-dev.yaml (namespaces, service accounts,
token-minter RBAC, the auth ConfigMap). The chart mounts the config and binds the
JWKS read permission.

Environment: KUBE_CONTEXT, NVKE_CONTROLLER_NAMESPACE, NVKE_AUTH_CONFIGMAP, NKE_DEPLOYMENT, NVKE_CONTROLLER_HTTP_PORT,
NKE_HTTP_URL, NKE_METRICS_PORT, NKE_METRICS_LOCAL_PORT, PROBE_PATH, TOKEN_TTL.
EOF
}

KUBE_CONTEXT="${KUBE_CONTEXT:-kind-controller-capi}"
# The NKE_CONTROLLER_ and CWE_ names (NAMESPACE, HTTP_PORT) are deprecated aliases for the NVKE_CONTROLLER_ names; #1399 removes them.
NVKE_CONTROLLER_NAMESPACE="${NVKE_CONTROLLER_NAMESPACE:-${NKE_CONTROLLER_NAMESPACE:-${CWE_NAMESPACE:-}}}"
# controllerConfig.name in the chart; an install that pins the old name sets this to nke-controller-config.
NVKE_CONTROLLER_CONFIGMAP="${NVKE_CONTROLLER_CONFIGMAP:-nvke-controller-config}"
NVKE_AUTH_CONFIGMAP="${NVKE_AUTH_CONFIGMAP:-nvke-auth-config}"
NKE_DEPLOYMENT="${NKE_DEPLOYMENT:-}"
NVKE_CONTROLLER_HTTP_PORT="${NVKE_CONTROLLER_HTTP_PORT:-${NKE_CONTROLLER_HTTP_PORT:-${CWE_HTTP_PORT:-8080}}}"
NKE_HTTP_URL="${NKE_HTTP_URL:-http://127.0.0.1:${NVKE_CONTROLLER_HTTP_PORT}}"
# Metrics are a separate listener (metricsListenAddress, chart default
# 0.0.0.0:9090) that neither the Service nor Tilt publishes, so this script
# forwards that port itself.
NKE_METRICS_PORT="${NKE_METRICS_PORT:-9090}"
NKE_METRICS_LOCAL_PORT="${NKE_METRICS_LOCAL_PORT:-19090}"
PROBE_PATH="${PROBE_PATH:-/api/clusters}"
TOKEN_TTL="${TOKEN_TTL:-10m}"

# Exported because nvke-token.sh runs as a child process and reads them from the
# environment. Without this it would use the kubeconfig's current context while
# this script targets KUBE_CONTEXT.
export KUBE_CONTEXT NVKE_CONTROLLER_NAMESPACE NVKE_AUTH_CONFIGMAP NKE_DEPLOYMENT

ADMITTED_NS="nke-clients"
ADMITTED_SA="nke-client"
OUTSIDER_NS="nke-outsiders"
OUTSIDER_SA="nke-outsider"
WRONG_AUDIENCE="https://nke.wrong.invalid"

die() { echo "FATAL: $*" >&2; exit 2; }

MODE=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --mode)
      # Guard before the shift: this script runs without `set -e`, so a failing
      # `shift 2` would loop on the same argument forever.
      [[ $# -ge 2 && -n "${2:-}" ]] || die "--mode requires a value: enforcing or permissive"
      MODE="$2"
      shift 2
      ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

KUBECTL=(kubectl --context "${KUBE_CONTEXT}")


need() { command -v "$1" >/dev/null 2>&1 || die "$1 not found on PATH"; }
need kubectl
need curl
need yq

if [[ -z "${NVKE_CONTROLLER_NAMESPACE}" ]]; then
  # Same order nvkectl uses: the greenfield namespace if it holds the auth
  # ConfigMap, then the one a site installed before the nvke rename keeps, else
  # the legacy one. Only a confirmed absence of all of them falls back.
  NVKE_CONTROLLER_NAMESPACE="cloud-workflow-engine"
  for candidate in nvke-controller nke-controller; do
    found="$(
      "${KUBECTL[@]}" get configmap "${NVKE_AUTH_CONFIGMAP}" -n "${candidate}" \
        --ignore-not-found -o name 2>&1
    )" || die "cannot look up configmap ${candidate}/${NVKE_AUTH_CONFIGMAP}: ${found}"
    if grep -qx "configmap/${NVKE_AUTH_CONFIGMAP}" <<<"${found}"; then
      NVKE_CONTROLLER_NAMESPACE="${candidate}"
      break
    fi
  done
  export NVKE_CONTROLLER_NAMESPACE
fi

"${KUBECTL[@]}" get ns "${NVKE_CONTROLLER_NAMESPACE}" >/dev/null 2>&1 \
  || die "namespace ${NVKE_CONTROLLER_NAMESPACE} not found in context ${KUBE_CONTEXT}"

# --- scaffolding preflight ------------------------------------------------

missing=()
"${KUBECTL[@]}" get sa "${ADMITTED_SA}" -n "${ADMITTED_NS}" >/dev/null 2>&1 \
  || missing+=("serviceaccount ${ADMITTED_NS}/${ADMITTED_SA}")
"${KUBECTL[@]}" get sa "${OUTSIDER_SA}" -n "${OUTSIDER_NS}" >/dev/null 2>&1 \
  || missing+=("serviceaccount ${OUTSIDER_NS}/${OUTSIDER_SA}")
"${KUBECTL[@]}" get configmap "${NVKE_AUTH_CONFIGMAP}" -n "${NVKE_CONTROLLER_NAMESPACE}" >/dev/null 2>&1 \
  || missing+=("configmap ${NVKE_CONTROLLER_NAMESPACE}/${NVKE_AUTH_CONFIGMAP}")
if [[ ${#missing[@]} -gt 0 ]]; then
  printf 'FATAL: missing scaffolding:\n' >&2
  printf '  - %s\n' "${missing[@]}" >&2
  echo "  apply deploy/k8s/nvke-auth-dev.yaml, or run tilt up" >&2
  exit 2
fi

# nvke-token.sh picks nvke-controller, nke-controller or cloud-workflow-engine when unset.
NKE_DEPLOYMENT="$("${SCRIPT_DIR}/nvke-token.sh" --deployment)"
[[ -n "${NKE_DEPLOYMENT}" ]] || die "could not resolve NVKE's Deployment in ${NVKE_CONTROLLER_NAMESPACE}"

# The audience comes from the live deployment, via nvke-token.sh.
AUDIENCE="$("${SCRIPT_DIR}/nvke-token.sh" --audience)"
[[ -n "${AUDIENCE}" ]] || die "could not read the audience from configmap ${NVKE_CONTROLLER_NAMESPACE}/${NVKE_AUTH_CONFIGMAP}"

# --- mode detection -------------------------------------------------------

if [[ -z "${MODE}" ]]; then
  # By container name, not index: a sidecar can sort first, and its environment
  # would report the wrong mode.
  container_env() {
    "${KUBECTL[@]}" get deploy "${NKE_DEPLOYMENT}" -n "${NVKE_CONTROLLER_NAMESPACE}" \
      -o jsonpath="{.spec.template.spec.containers[?(@.name=='${NKE_DEPLOYMENT}')].env[?(@.name=='$1')].value}" 2>/dev/null
  }
  auth_path="$(container_env AUTH_CONFIG_PATH)"
  permissive="$("${KUBECTL[@]}" get configmap "${NVKE_CONTROLLER_CONFIGMAP}" -n "${NVKE_CONTROLLER_NAMESPACE}" \
    -o jsonpath='{.data.config\.yaml}' 2>/dev/null | yq '.permissiveMode')"
  if [[ -z "${auth_path}" ]]; then
    MODE="disabled"
  elif [[ "${permissive}" == "true" ]]; then
    MODE="permissive"
  else
    MODE="enforcing"
  fi
fi

case "${MODE}" in
  enforcing|permissive) ;;
  disabled)
    echo "FATAL: AUTH_CONFIG_PATH is not set on deploy/${NKE_DEPLOYMENT}." >&2
    echo "  Authentication is disabled, so there is nothing to prove." >&2
    echo "  Redeploy with NKE_AUTH_ENABLED=true, or pass --mode explicitly." >&2
    exit 2
    ;;
  *) die "unknown mode: ${MODE}" ;;
esac

# --- transport ------------------------------------------------------------

PF_PIDS=()
cleanup() {
  for pid in "${PF_PIDS[@]:-}"; do
    [[ -n "${pid}" ]] && kill "${pid}" >/dev/null 2>&1
  done
}
trap cleanup EXIT

wait_for_port() {
  local url="$1" tries=0
  while (( tries < 40 )); do
    curl -s -o /dev/null --max-time 2 "${url}" && return 0
    tries=$(( tries + 1 ))
    sleep 0.25
  done
  return 1
}

# Reuse Tilt's forward on 8080 when it is already up (Tiltfile port_forwards).
if ! curl -s -o /dev/null --max-time 2 "${NKE_HTTP_URL}/api/version"; then
  echo "no listener on ${NKE_HTTP_URL}, starting a port-forward"
  "${KUBECTL[@]}" -n "${NVKE_CONTROLLER_NAMESPACE}" port-forward \
    "deploy/${NKE_DEPLOYMENT}" "${NVKE_CONTROLLER_HTTP_PORT}:8080" >/dev/null 2>&1 &
  PF_PIDS+=("$!")
  wait_for_port "${NKE_HTTP_URL}/api/version" \
    || die "could not reach NVKE on ${NKE_HTTP_URL}"
fi

METRICS_URL="http://127.0.0.1:${NKE_METRICS_LOCAL_PORT}/metrics"
metrics_available=0
"${KUBECTL[@]}" -n "${NVKE_CONTROLLER_NAMESPACE}" port-forward \
  "deploy/${NKE_DEPLOYMENT}" "${NKE_METRICS_LOCAL_PORT}:${NKE_METRICS_PORT}" >/dev/null 2>&1 &
PF_PIDS+=("$!")
if wait_for_port "${METRICS_URL}"; then
  metrics_available=1
else
  echo "WARNING: no metrics listener on ${METRICS_URL}"
fi

# Sum nvke_auth_requests_total across every label set matching outcome=<arg>.
auth_counter() {
  local outcome="$1"
  (( metrics_available )) || { echo ""; return; }
  curl -s --max-time 5 "${METRICS_URL}" \
    | awk -v want="outcome=\"${outcome}\"" '
        # $(NF-1) would be wrong too; the value is the field right after "}".
        /^nvke_auth_requests_total\{/ && index($0, want) {
          split(substr($0, index($0, "}") + 1), f, " ")
          total += f[1]; found = 1
        }
        END { if (found) printf "%d\n", total; else print "" }
      '
}

# --- token minting --------------------------------------------------------

# Callers check for an empty result rather than this function calling `die`:
# inside `$( )`, exit only leaves the subshell and the failure would be lost.
mint() {
  local ns="$1" sa="$2" aud="$3"
  "${KUBECTL[@]}" create token "${sa}" -n "${ns}" \
    --audience="${aud}" --duration="${TOKEN_TTL}" 2>/dev/null
}

TOKEN_ADMITTED="$(mint "${ADMITTED_NS}" "${ADMITTED_SA}" "${AUDIENCE}")"
[[ -n "${TOKEN_ADMITTED}" ]] \
  || die "kubectl create token ${ADMITTED_SA} -n ${ADMITTED_NS} --audience=${AUDIENCE} failed"
TOKEN_WRONG_AUD="$(mint "${ADMITTED_NS}" "${ADMITTED_SA}" "${WRONG_AUDIENCE}")"
[[ -n "${TOKEN_WRONG_AUD}" ]] || die "could not mint the wrong-audience token"
TOKEN_OUTSIDER="$(mint "${OUTSIDER_NS}" "${OUTSIDER_SA}" "${AUDIENCE}")"
[[ -n "${TOKEN_OUTSIDER}" ]] || die "could not mint the unadmitted token"

# --- the cases ------------------------------------------------------------

# Do not fold these two curl lines into one with an array of headers: an empty
# array breaks under `set -u` on bash 3.2, which is what macOS ships.
probe() {
  local token="$1"
  if [[ -z "${token}" ]]; then
    curl -s -o /dev/null -w '%{http_code}' --max-time 10 "${NKE_HTTP_URL}${PROBE_PATH}"
  else
    curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
      -H "Authorization: Bearer ${token}" "${NKE_HTTP_URL}${PROBE_PATH}"
  fi
}

if [[ "${MODE}" == "permissive" ]]; then
  EXPECT_NO_TOKEN=200; EXPECT_WRONG_AUD=200; EXPECT_OUTSIDER=200
else
  EXPECT_NO_TOKEN=401; EXPECT_WRONG_AUD=401; EXPECT_OUTSIDER=403
fi
# Not in either arm: the admitted caller is served in both modes.
EXPECT_ADMITTED=200

WOULD_REJECT_BEFORE="$(auth_counter would_reject)"

# One preformatted row per case; ACTUAL keeps the raw codes for the all-200
# diagnostic at the end.
ROW_FORMAT='%-34s %10s %10s  %s\n'
declare -a ROWS=() ACTUAL=()
failures=0

check() {
  local name="$1" expected="$2" actual="$3" result
  if [[ "${actual}" == "${expected}" ]]; then result="PASS"; else result="FAIL"; failures=$(( failures + 1 )); fi
  # shellcheck disable=SC2059 # ROW_FORMAT is a constant format string
  ROWS+=("$(printf "${ROW_FORMAT}" "${name}" "${expected}" "${actual}" "${result}")")
  ACTUAL+=("${actual}")
}

check "1 no token"                       "${EXPECT_NO_TOKEN}"  "$(probe "")"
check "2 ${ADMITTED_NS}/${ADMITTED_SA}"  "${EXPECT_ADMITTED}"  "$(probe "${TOKEN_ADMITTED}")"
check "3 wrong audience"                 "${EXPECT_WRONG_AUD}" "$(probe "${TOKEN_WRONG_AUD}")"
check "4 ${OUTSIDER_NS}/${OUTSIDER_SA}"  "${EXPECT_OUTSIDER}"  "$(probe "${TOKEN_OUTSIDER}")"

if [[ "${MODE}" == "permissive" ]]; then
  WOULD_REJECT_AFTER="$(auth_counter would_reject)"
  before="${WOULD_REJECT_BEFORE:-0}"
  after="${WOULD_REJECT_AFTER:-}"
  if [[ -z "${after}" ]]; then
    check "5 would_reject +3" "3" "no-metric"
  else
    check "5 would_reject +3" "3" "$(( after - before ))"
  fi
fi

# --- report ---------------------------------------------------------------

echo
echo "NVKE API authentication demo"
echo "  context   ${KUBE_CONTEXT}"
echo "  target    ${NKE_HTTP_URL}${PROBE_PATH}"
echo "  mode      ${MODE}"
echo "  audience  ${AUDIENCE}"
echo
# shellcheck disable=SC2059 # ROW_FORMAT is a constant format string
printf "${ROW_FORMAT}" "CASE" "EXPECTED" "ACTUAL" "RESULT"
# shellcheck disable=SC2059
printf "${ROW_FORMAT}" "----------------------------------" "----------" "----------" "------"
printf '%s\n' "${ROWS[@]}"
echo

if (( failures > 0 )); then
  echo "${failures} of ${#ROWS[@]} checks failed"
  if [[ "$(printf '%s\n' "${ACTUAL[@]}" | sort -u | tr -d '\n')" == "200" ]]; then
    echo "Every case returned 200: the authentication middleware is not in the chain."
  fi
  exit 1
fi

echo "all ${#ROWS[@]} checks passed"
