#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Print a bearer token for the NVKE API, or nothing when NVKE is not
# authenticating.
#
#   TOKEN="$(hack/nvke-token.sh)"
#   curl ${TOKEN:+-H "Authorization: Bearer ${TOKEN}"} "${API_URL}/clusters"
#
# Empty output means authentication is off and a bare request is correct. Real
# failures exit non-zero and say why on stderr.
#
# Environment:
#   KUBE_CONTEXT               kube context (default: kubeconfig current-context)
#   NVKE_CONTROLLER_NAMESPACE  NVKE's namespace (default nvke-controller, then
#                              nke-controller, whichever holds NVKE_AUTH_CONFIGMAP
#                              or the Deployment of the same name; else
#                              cloud-workflow-engine)
#   NVKE_AUTH_CONFIGMAP         authentication ConfigMap (default nvke-auth-config;
#                              a site that pinned apiAuth.configMap.name sets
#                              this to nke-auth-config)
#   NKE_DEPLOYMENT             NVKE's Deployment (default nvke-controller, then
#                              nke-controller, whichever exists in
#                              NVKE_CONTROLLER_NAMESPACE, else cloud-workflow-engine)
#   NKE_TOKEN_NAMESPACE        service account namespace (default nke-clients)
#   NKE_TOKEN_SERVICE_ACCOUNT  service account (default nke-client)
#   NKE_TOKEN_TTL              requested lifetime (default 30m)
#   NKE_AUDIENCE               skip discovery and mint for this audience
#   NKE_AUTH_TOKEN             skip everything and echo this

set -euo pipefail

KUBE_CONTEXT="${KUBE_CONTEXT:-}"
# NKE_CONTROLLER_NAMESPACE and CWE_NAMESPACE are deprecated aliases for NVKE_CONTROLLER_NAMESPACE; #1399 removes them.
NVKE_CONTROLLER_NAMESPACE="${NVKE_CONTROLLER_NAMESPACE:-${NKE_CONTROLLER_NAMESPACE:-${CWE_NAMESPACE:-}}}"
NKE_DEPLOYMENT="${NKE_DEPLOYMENT:-}"
NVKE_AUTH_CONFIGMAP="${NVKE_AUTH_CONFIGMAP:-nvke-auth-config}"
NKE_TOKEN_NAMESPACE="${NKE_TOKEN_NAMESPACE:-nke-clients}"
NKE_TOKEN_SERVICE_ACCOUNT="${NKE_TOKEN_SERVICE_ACCOUNT:-nke-client}"
NKE_TOKEN_TTL="${NKE_TOKEN_TTL:-30m}"

# An explicit token wins, including an invalid one. Not for --deployment or
# --audience, which would otherwise print the token as a name.
if [[ -n "${NKE_AUTH_TOKEN:-}" && -z "${1:-}" ]]; then
  printf '%s' "${NKE_AUTH_TOKEN}"
  exit 0
fi

die() { echo "nvke-token.sh: $*" >&2; exit 1; }

command -v kubectl >/dev/null 2>&1 || die "kubectl not found on PATH"
KUBECTL=(kubectl)
[[ -n "${KUBE_CONTEXT}" ]] && KUBECTL+=(--context "${KUBE_CONTEXT}")

if [[ -z "${NVKE_CONTROLLER_NAMESPACE}" ]]; then
  # Same order nvkectl uses: the greenfield namespace if it holds the auth
  # ConfigMap, then the one a site installed before the nvke rename keeps, else
  # the legacy one. A greenfield site with authentication off has no ConfigMap,
  # so its Deployment also selects the greenfield namespace. Only a confirmed
  # absence of every candidate falls back.
  NVKE_CONTROLLER_NAMESPACE="cloud-workflow-engine"
  for candidate in nvke-controller nke-controller; do
    found="$(
      "${KUBECTL[@]}" get "configmap/${NVKE_AUTH_CONFIGMAP}" "deploy/${candidate}" -n "${candidate}" \
        --ignore-not-found -o name 2>&1
    )" || die "cannot look up configmap and deploy ${candidate}/${candidate}: ${found}"
    if grep -qxE "configmap/${NVKE_AUTH_CONFIGMAP}|deployment.apps/${candidate}" <<<"${found}"; then
      NVKE_CONTROLLER_NAMESPACE="${candidate}"
      break
    fi
  done
fi

# --- audience ---------------------------------------------------------------

# Reads the audience from the live deployment. A token minted for the wrong one
# is rejected with a 401 that looks identical to sending no token at all.
#
# Matching is any-of, so the first entry will do. Handles the flow and block
# forms, and strips quotes that would otherwise land in the token's "aud".
cwe_discover_audience() {
  "${KUBECTL[@]}" get configmap "${NVKE_AUTH_CONFIGMAP}" -n "${NVKE_CONTROLLER_NAMESPACE}" \
    -o jsonpath='{.data.auth\.yaml}' 2>/dev/null \
    | awk '
        /^[[:space:]]*audiences:[[:space:]]*\[/ {
          line = $0
          sub(/^[^[]*\[[[:space:]]*/, "", line)
          sub(/[],].*$/, "", line)
          gsub(/^[[:space:]]*["'\'']|["'\''][[:space:]]*$/, "", line)
          if (line != "") { print line; exit }
          next
        }
        /^[[:space:]]*audiences:[[:space:]]*$/ { inblock = 1; next }
        inblock && /^[[:space:]]*-[[:space:]]/ {
          line = $0
          sub(/^[[:space:]]*-[[:space:]]*/, "", line)
          gsub(/^["'\'']|["'\'']$/, "", line)
          print line; exit
        }
        inblock { inblock = 0 }
      '
}

# --audience lets other scripts reuse this parser rather than growing a copy.
if [[ "${1:-}" == "--audience" ]]; then
  # Checked before use: a command substitution inside an argument list does not
  # trip set -e, so an empty result would pass through silently.
  audience="${NKE_AUDIENCE:-$(cwe_discover_audience)}"
  [[ -n "${audience}" ]] || die "no audience in configmap ${NVKE_CONTROLLER_NAMESPACE}/${NVKE_AUTH_CONFIGMAP}; set NKE_AUDIENCE to override"
  printf '%s' "${audience}"
  exit 0
fi

# --- deployment -------------------------------------------------------------

# A greenfield site installs NVKE as nvke-controller, a site installed before
# the nvke rename as nke-controller; legacy sites still use
# cloud-workflow-engine. The container shares the Deployment's name
# in all three, which the AUTH_CONFIG_PATH lookup below relies on.
# Only a confirmed absence falls back; any other lookup failure is fatal.
if [[ -z "${NKE_DEPLOYMENT}" ]]; then
  found="$(
    "${KUBECTL[@]}" get deploy nvke-controller nke-controller -n "${NVKE_CONTROLLER_NAMESPACE}" \
      --ignore-not-found -o name 2>&1
  )" || die "cannot look up deploy nvke-controller or nke-controller in ${NVKE_CONTROLLER_NAMESPACE}:
  ${found}
  Check KUBE_CONTEXT, NVKE_CONTROLLER_NAMESPACE and that this identity may read Deployments,
  or set NKE_DEPLOYMENT."
  NKE_DEPLOYMENT="cloud-workflow-engine"
  for candidate in nvke-controller nke-controller; do
    if [[ "${found}" == *"deployment.apps/${candidate}"* ]]; then
      NKE_DEPLOYMENT="${candidate}"
      break
    fi
  done
fi

# --deployment lets other scripts reuse this lookup, as --audience does above.
if [[ "${1:-}" == "--deployment" ]]; then
  printf '%s' "${NKE_DEPLOYMENT}"
  exit 0
fi

# Reads AUTH_CONFIG_PATH from the Deployment rather than checking whether the
# ConfigMap exists, which Tilt applies either way. A lookup failure is fatal,
# not "authentication is off".
auth_config_path="$(
  "${KUBECTL[@]}" get deploy "${NKE_DEPLOYMENT}" -n "${NVKE_CONTROLLER_NAMESPACE}" \
    -o jsonpath="{.spec.template.spec.containers[?(@.name=='${NKE_DEPLOYMENT}')].env[?(@.name=='AUTH_CONFIG_PATH')].value}" 2>&1
)" || die "cannot read deploy ${NVKE_CONTROLLER_NAMESPACE}/${NKE_DEPLOYMENT}:
  ${auth_config_path}
  Check KUBE_CONTEXT, NVKE_CONTROLLER_NAMESPACE and that this identity may read Deployments."

if [[ -z "${auth_config_path}" ]]; then
  # Authentication is off. No token, no error, no output.
  exit 0
fi

AUDIENCE="${NKE_AUDIENCE:-$(cwe_discover_audience)}"
if [[ -z "${AUDIENCE}" ]]; then
  die "NVKE is authenticating (AUTH_CONFIG_PATH=${auth_config_path}) but no audience could be read
  from configmap ${NVKE_CONTROLLER_NAMESPACE}/${NVKE_AUTH_CONFIGMAP}. Set NKE_AUDIENCE to override."
fi

# --- mint -------------------------------------------------------------------

# Uses TokenRequest: NVKE accepts only audience-scoped tokens, and a legacy
# Secret carries no audience. Needs `create` on serviceaccounts/token, granted
# by deploy/k8s/nvke-auth-dev.yaml.
token="$(
  "${KUBECTL[@]}" create token "${NKE_TOKEN_SERVICE_ACCOUNT}" \
    -n "${NKE_TOKEN_NAMESPACE}" \
    --audience="${AUDIENCE}" \
    --duration="${NKE_TOKEN_TTL}" 2>&1
)" || die "minting a token for ${NKE_TOKEN_NAMESPACE}/${NKE_TOKEN_SERVICE_ACCOUNT} failed:
  ${token}
  Apply deploy/k8s/nvke-auth-dev.yaml, or run tilt up, so the service account and
  its token-minter RoleBinding exist."

[[ -n "${token}" ]] || die "kubectl create token produced no token"

printf '%s' "${token}"
