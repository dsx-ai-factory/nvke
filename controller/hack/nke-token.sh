#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Print a bearer token for the NKE API, or nothing when NKE is not
# authenticating.
#
#   TOKEN="$(hack/nke-token.sh)"
#   curl ${TOKEN:+-H "Authorization: Bearer ${TOKEN}"} "${API_URL}/sites"
#
# Empty output means authentication is off and a bare request is correct. Real
# failures exit non-zero and say why on stderr.
#
# Environment:
#   KUBE_CONTEXT               kube context (default: kubeconfig current-context)
#   CWE_NAMESPACE              NKE's namespace (default cloud-workflow-engine)
#   NKE_DEPLOYMENT             NKE's Deployment (default cloud-workflow-engine)
#   NKE_TOKEN_NAMESPACE        service account namespace (default nke-clients)
#   NKE_TOKEN_SERVICE_ACCOUNT  service account (default nke-client)
#   NKE_TOKEN_TTL              requested lifetime (default 30m)
#   NKE_AUDIENCE               skip discovery and mint for this audience
#   NKE_AUTH_TOKEN             skip everything and echo this

set -euo pipefail

KUBE_CONTEXT="${KUBE_CONTEXT:-}"
CWE_NAMESPACE="${CWE_NAMESPACE:-cloud-workflow-engine}"
NKE_DEPLOYMENT="${NKE_DEPLOYMENT:-cloud-workflow-engine}"
NKE_TOKEN_NAMESPACE="${NKE_TOKEN_NAMESPACE:-nke-clients}"
NKE_TOKEN_SERVICE_ACCOUNT="${NKE_TOKEN_SERVICE_ACCOUNT:-nke-client}"
NKE_TOKEN_TTL="${NKE_TOKEN_TTL:-30m}"

# An explicit token wins, including an invalid one.
if [[ -n "${NKE_AUTH_TOKEN:-}" ]]; then
  printf '%s' "${NKE_AUTH_TOKEN}"
  exit 0
fi

die() { echo "nke-token.sh: $*" >&2; exit 1; }

command -v kubectl >/dev/null 2>&1 || die "kubectl not found on PATH"
KUBECTL=(kubectl)
[[ -n "${KUBE_CONTEXT}" ]] && KUBECTL+=(--context "${KUBE_CONTEXT}")

# --- audience ---------------------------------------------------------------

# Reads the audience from the live deployment. A token minted for the wrong one
# is rejected with a 401 that looks identical to sending no token at all.
#
# Matching is any-of, so the first entry will do. Handles the flow and block
# forms, and strips quotes that would otherwise land in the token's "aud".
cwe_discover_audience() {
  "${KUBECTL[@]}" get configmap nke-auth-config -n "${CWE_NAMESPACE}" \
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
  [[ -n "${audience}" ]] || die "no audience in configmap ${CWE_NAMESPACE}/nke-auth-config; set NKE_AUDIENCE to override"
  printf '%s' "${audience}"
  exit 0
fi

# Reads AUTH_CONFIG_PATH from the Deployment rather than checking whether the
# ConfigMap exists, which Tilt applies either way. A lookup failure is fatal,
# not "authentication is off".
auth_config_path="$(
  "${KUBECTL[@]}" get deploy "${NKE_DEPLOYMENT}" -n "${CWE_NAMESPACE}" \
    -o jsonpath="{.spec.template.spec.containers[?(@.name=='${NKE_DEPLOYMENT}')].env[?(@.name=='AUTH_CONFIG_PATH')].value}" 2>&1
)" || die "cannot read deploy ${CWE_NAMESPACE}/${NKE_DEPLOYMENT}:
  ${auth_config_path}
  Check KUBE_CONTEXT, CWE_NAMESPACE and that this identity may read Deployments."

if [[ -z "${auth_config_path}" ]]; then
  # Authentication is off. No token, no error, no output.
  exit 0
fi

AUDIENCE="${NKE_AUDIENCE:-$(cwe_discover_audience)}"
if [[ -z "${AUDIENCE}" ]]; then
  die "NKE is authenticating (AUTH_CONFIG_PATH=${auth_config_path}) but no audience could be read
  from configmap ${CWE_NAMESPACE}/nke-auth-config. Set NKE_AUDIENCE to override."
fi

# --- mint -------------------------------------------------------------------

# Uses TokenRequest: NKE accepts only audience-scoped tokens, and a legacy
# Secret carries no audience. Needs `create` on serviceaccounts/token, granted
# by deploy/k8s/nke-auth-dev.yaml.
token="$(
  "${KUBECTL[@]}" create token "${NKE_TOKEN_SERVICE_ACCOUNT}" \
    -n "${NKE_TOKEN_NAMESPACE}" \
    --audience="${AUDIENCE}" \
    --duration="${NKE_TOKEN_TTL}" 2>&1
)" || die "minting a token for ${NKE_TOKEN_NAMESPACE}/${NKE_TOKEN_SERVICE_ACCOUNT} failed:
  ${token}
  Apply deploy/k8s/nke-auth-dev.yaml, or run tilt up, so the service account and
  its token-minter RoleBinding exist."

[[ -n "${token}" ]] || die "kubectl create token produced no token"

printf '%s' "${token}"
