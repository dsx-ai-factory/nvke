#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Asserts what the chart renders for apiAuth in each state, which `helm lint`
# does not check.
#
# Needs only helm. Resolves the chart relative to this script, so it runs from
# anywhere; CHART overrides that, which is how the workflow points it at
# controller/deploy/charts/cloud-workflow-engine from the repository root.

set -euo pipefail

CHART="${CHART:-"$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/deploy/charts/cloud-workflow-engine"}"
[[ -d "${CHART}" ]] || { echo "chart not found at ${CHART}" >&2; exit 2; }

failures=0

render() { helm template cwe "${CHART}" "$@"; }

# want <description> <pattern> <rendered>
want() {
  if grep -qE "$2" <<<"$3"; then
    printf '  ok    %s\n' "$1"
  else
    printf '  FAIL  %s (expected to find: %s)\n' "$1" "$2" >&2
    failures=$((failures + 1))
  fi
}

# absent <description> <pattern> <rendered>
absent() {
  if grep -qE "$2" <<<"$3"; then
    printf '  FAIL  %s (should not have rendered: %s)\n' "$1" "$2" >&2
    failures=$((failures + 1))
  else
    printf '  ok    %s\n' "$1"
  fi
}

# pair <description> <key> <value> <rendered>
# Asserts value on the line after key. A bare `value: "true"` matches unrelated
# env vars, so checking key and value independently would pass either way.
pair() {
  if grep -A1 "name: $2" <<<"$4" | grep -q "value: \"$3\""; then
    printf '  ok    %s\n' "$1"
  else
    printf '  FAIL  %s (expected %s = %s)\n' "$1" "$2" "$3" >&2
    failures=$((failures + 1))
  fi
}

echo "apiAuth disabled (the default):"
out="$(render)"
# Absent, not empty: NKE reads an empty AUTH_CONFIG_PATH as unset.
absent "AUTH_CONFIG_PATH is not set at all"   'AUTH_CONFIG_PATH'                     "${out}"
absent "PERMISSIVE_MODE is not set"           'PERMISSIVE_MODE'                      "${out}"
absent "no config volume"                     'name: nke-auth-config'                "${out}"
absent "no issuer-discovery binding"          'system:service-account-issuer-discovery' "${out}"

echo "apiAuth enabled, enforcing:"
out="$(render --set apiAuth.enabled=true)"
want   "AUTH_CONFIG_PATH points at the mount" 'value: "/etc/nke/auth/auth.yaml"'     "${out}"
pair   "PERMISSIVE_MODE is false"            'PERMISSIVE_MODE' 'false'              "${out}"
want   "ConfigMap volume"                     'name: nke-auth-config'                "${out}"
want   "mounted where the path says"          'mountPath: "/etc/nke/auth"'           "${out}"
want   "issuer-discovery binding"             'system:service-account-issuer-discovery' "${out}"

echo "apiAuth enabled, permissive:"
out="$(render --set apiAuth.enabled=true --set apiAuth.permissive=true)"
pair   "PERMISSIVE_MODE is true"             'PERMISSIVE_MODE' 'true'               "${out}"

echo "clusterIssuerDiscovery=false:"
out="$(render --set apiAuth.enabled=true --set apiAuth.clusterIssuerDiscovery=false)"
absent "drops the cluster-scoped grant"       'system:service-account-issuer-discovery' "${out}"
want   "but the rest of apiAuth still renders" 'AUTH_CONFIG_PATH'                    "${out}"

echo "the mount path and file name are fixed by the chart:"
out="$(render --set apiAuth.enabled=true)"
want   "mounted at the path AUTH_CONFIG_PATH names" 'mountPath: "/etc/nke/auth"'     "${out}"
want   "and the two agree"                    'value: "/etc/nke/auth/auth.yaml"'     "${out}"

echo "a named ConfigMap is what gets mounted:"
out="$(render --set apiAuth.enabled=true --set apiAuth.configMap.name=my-auth)"
want   "configMap.name is honoured"           'name: my-auth'                        "${out}"

if [[ "${failures}" -ne 0 ]]; then
  echo "${failures} chart assertion(s) failed" >&2
  exit 1
fi
echo "all chart auth assertions passed"
