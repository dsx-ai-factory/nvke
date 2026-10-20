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
#   NVKE_CONTROLLER_NAMESPACE  NVKE's namespace (default nke-controller when it
#                              holds NVKE_AUTH_CONFIGMAP or Deployment nke-controller,
#                              else cloud-workflow-engine)
#   NVKE_AUTH_CONFIGMAP         authentication ConfigMap (default nvke-auth-config;
#                              a site that pinned apiAuth.configMap.name sets
#                              this to nke-auth-config)
#   NKE_DEPLOYMENT             NVKE's Deployment (default nke-controller when it
#                              exists in NVKE_CONTROLLER_NAMESPACE, else cloud-workflow-engine)
#   NVKE_CONTROLLER_CONFIGMAP  NVKE's settings ConfigMap, read only when AUTH_CONFIG_PATH
#                              is unset (default: the one the Deployment mounts, else the
#                              first of nvke-controller-config and nke-controller-config)
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
  # ConfigMap, else the legacy one. A greenfield site with authentication off
  # has no ConfigMap, so its Deployment also selects the greenfield namespace.
  # Only a confirmed absence of both falls back.
  found="$(
    "${KUBECTL[@]}" get "configmap/${NVKE_AUTH_CONFIGMAP}" deploy/nke-controller -n nke-controller \
      --ignore-not-found -o name 2>&1
  )" || die "cannot look up configmap and deploy nke-controller/nke-controller: ${found}"
  NVKE_CONTROLLER_NAMESPACE="cloud-workflow-engine"
  grep -qxE "configmap/${NVKE_AUTH_CONFIGMAP}|deployment.apps/nke-controller" <<<"${found}" \
    && NVKE_CONTROLLER_NAMESPACE="nke-controller"
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

# A greenfield site installs NVKE as nke-controller; legacy sites still use
# cloud-workflow-engine. The container shares the Deployment's name
# in both, which the AUTH_CONFIG_PATH lookup below relies on.
# Only a confirmed absence falls back; any other lookup failure is fatal.
if [[ -z "${NKE_DEPLOYMENT}" ]]; then
  found="$(
    "${KUBECTL[@]}" get deploy nke-controller -n "${NVKE_CONTROLLER_NAMESPACE}" \
      --ignore-not-found -o name 2>&1
  )" || die "cannot look up deploy ${NVKE_CONTROLLER_NAMESPACE}/nke-controller:
  ${found}
  Check KUBE_CONTEXT, NVKE_CONTROLLER_NAMESPACE and that this identity may read Deployments,
  or set NKE_DEPLOYMENT."
  if [[ "${found}" == *"deployment.apps/nke-controller"* ]]; then
    NKE_DEPLOYMENT="nke-controller"
  else
    NKE_DEPLOYMENT="cloud-workflow-engine"
  fi
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

# A configuration the chart renders reaches NVKE as apiAuth in its settings
# ConfigMap instead, with no AUTH_CONFIG_PATH, so that ConfigMap is read only
# when the path is unset. NVKE_CONTROLLER_CONFIGMAP names it outright; otherwise
# the Deployment's own volume says which ConfigMap this controller mounts, and
# nvke-controller-config then nke-controller-config are tried only when the
# Deployment does not say. Read with awk, not yq, so kubectl stays this script's
# only dependency. A controller older than the section, or than the ConfigMap,
# has no such key, which is off.
api_auth_enabled=""
if [[ -z "${auth_config_path}" ]]; then
  # stdout alone carries each answer: a kubectl warning on stderr must not read
  # as a ConfigMap name, or as the first name existing and skip the second.
  kubectl_stderr="$(mktemp)"
  if [[ -n "${NVKE_CONTROLLER_CONFIGMAP:-}" ]]; then
    controller_configmaps=("${NVKE_CONTROLLER_CONFIGMAP}")
  else
    # The volume is nvke-controller-config in current charts and was
    # nke-controller-config before; its configMap.name is the object either way.
    if ! mounted_configmap="$(
      "${KUBECTL[@]}" get deploy "${NKE_DEPLOYMENT}" -n "${NVKE_CONTROLLER_NAMESPACE}" \
        -o jsonpath="{.spec.template.spec.volumes[?(@.name=='nvke-controller-config')].configMap.name}{.spec.template.spec.volumes[?(@.name=='nke-controller-config')].configMap.name}" \
        2>"${kubectl_stderr}"
    )"; then
      error="$(cat "${kubectl_stderr}")"
      rm -f "${kubectl_stderr}"
      die "cannot read deploy ${NVKE_CONTROLLER_NAMESPACE}/${NKE_DEPLOYMENT}:
  ${error}
  Check KUBE_CONTEXT, NVKE_CONTROLLER_NAMESPACE and that this identity may read Deployments."
    fi
    if [[ -n "${mounted_configmap}" ]]; then
      controller_configmaps=("${mounted_configmap}")
    else
      controller_configmaps=(nvke-controller-config nke-controller-config)
    fi
  fi
  controller_config=""
  for configmap in "${controller_configmaps[@]}"; do
    if ! controller_config="$(
      "${KUBECTL[@]}" get configmap "${configmap}" -n "${NVKE_CONTROLLER_NAMESPACE}" \
        --ignore-not-found -o jsonpath='{.data.config\.yaml}' 2>"${kubectl_stderr}"
    )"; then
      error="$(cat "${kubectl_stderr}")"
      rm -f "${kubectl_stderr}"
      die "cannot read configmap ${NVKE_CONTROLLER_NAMESPACE}/${configmap}:
  ${error}
  Check KUBE_CONTEXT, NVKE_CONTROLLER_NAMESPACE and that this identity may read ConfigMaps."
    fi
    if [[ -n "${controller_config}" ]]; then
      break
    fi
  done
  rm -f "${kubectl_stderr}"
  api_auth_enabled="$(awk '
    /^apiAuth:/ { section = 1; next }
    section && /^[^[:space:]]/ { exit }
    section && /^[[:space:]]+enabled:[[:space:]]*true[[:space:]]*$/ { print "true"; exit }
  ' <<<"${controller_config}")"
fi

if [[ -z "${auth_config_path}" && "${api_auth_enabled}" != "true" ]]; then
  # Authentication is off. No token, no error, no output.
  exit 0
fi

AUDIENCE="${NKE_AUDIENCE:-$(cwe_discover_audience)}"
if [[ -z "${AUDIENCE}" ]]; then
  die "NVKE is authenticating but no audience could be read
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
