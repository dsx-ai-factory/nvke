#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

CHART="${CHART:-"$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/deploy/charts/controller"}"
failures=0

want() {
  if grep -qE "$2" <<<"$3"; then
    printf '  ok    %s\n' "$1"
  else
    printf '  FAIL  %s (expected %s)\n' "$1" "$2" >&2
    failures=$((failures + 1))
  fi
}

absent() {
  if grep -qE "$2" <<<"$3"; then
    printf '  FAIL  %s (unexpected %s)\n' "$1" "$2" >&2
    failures=$((failures + 1))
  else
    printf '  ok    %s\n' "$1"
  fi
}

want_env_value() {
  if awk -v name="$2" -v value="$3" '
    $0 ~ "^[[:space:]]*- name: " name "$" {
      getline
      line = $0
      sub(/^[[:space:]]+/, "", line)
      if (line == "value: \"" value "\"") found = 1
    }
    END { exit !found }
  ' <<<"$4"; then
    printf '  ok    %s\n' "$1"
  else
    printf '  FAIL  %s (expected %s=%s)\n' "$1" "$2" "$3" >&2
    failures=$((failures + 1))
  fi
}

echo "node-agent TLS/JWT disabled:"
out="$(helm template cwe "${CHART}")"
want_env_value "capability is explicitly false" NODE_AGENT_TLS_JWT_ENABLED false "${out}"
absent "no client identity mount" 'name: node-agent-client-tls' "${out}"
absent "no JWT issuer configuration" 'NODE_AGENT_TLS_JWT_ISSUER' "${out}"
absent "no JWT key-source configuration" 'NODE_AGENT_TLS_JWT_JWKS_URI' "${out}"

echo "node-agent TLS/JWT enabled:"
tls_jwt_values=(
  --set spiffe.enabled=true
  --set nodeAgent.tlsJwt.enabled=true
  --set nodeAgent.tlsJwt.issuer=https://identity.example.test
  --set nodeAgent.tlsJwt.jwksURI=https://identity.example.test/.well-known/jwks.json
  --set 'nodeAgent.tlsJwt.algorithms={RS256,ES256}'
  --set nodeAgent.tlsJwt.subjectTrustDomain=identity.example.test
  --set nodeAgent.tlsJwt.subjectTenant=tenant-a
  --set nodeAgent.tlsJwt.subjectService=node-agent
  --set nodeAgent.tlsJwt.dsxIdentityExchangeURL=https://identity.example.test/exchange
)
out="$(helm template cwe "${CHART}" "${tls_jwt_values[@]}")"
want_env_value "capability is true" NODE_AGENT_TLS_JWT_ENABLED true "${out}"
absent "no token configuration ConfigMap" 'name: nke-node-agent-auth' "${out}"
absent "no client identity Secret" 'secretName: nke-controller-agent-client-tls' "${out}"
absent "no client identity mount" 'name: node-agent-client-tls' "${out}"
want_env_value "JWT issuer comes from controller config" NODE_AGENT_TLS_JWT_ISSUER https://identity.example.test "${out}"
want_env_value "JWT key source comes from controller config" NODE_AGENT_TLS_JWT_JWKS_URI https://identity.example.test/.well-known/jwks.json "${out}"
want_env_value "JWT algorithms come from controller config" NODE_AGENT_TLS_JWT_ALGORITHMS RS256,ES256 "${out}"
want_env_value "JWT subject trust domain comes from controller config" NODE_AGENT_TLS_JWT_SUBJECT_TRUST_DOMAIN identity.example.test "${out}"
want_env_value "JWT subject tenant comes from controller config" NODE_AGENT_TLS_JWT_SUBJECT_TENANT tenant-a "${out}"
want_env_value "JWT subject service comes from controller config" NODE_AGENT_TLS_JWT_SUBJECT_SERVICE node-agent "${out}"
want_env_value "identity exchange URL comes from controller config" NODE_AGENT_TLS_JWT_DSX_IDENTITY_EXCHANGE_URL https://identity.example.test/exchange "${out}"
if whitespace_out="$(helm template cwe "${CHART}" "${tls_jwt_values[@]}" \
  --set-string nodeAgent.tlsJwt.dsxIdentityExchangeURL='   ' 2>&1)"; then
  printf '  FAIL  whitespace-only identity exchange URL was accepted\n' >&2
  failures=$((failures + 1))
else
  want "whitespace-only identity exchange URL is rejected" 'nodeAgent.tlsJwt.dsxIdentityExchangeURL is required' "${whitespace_out}"
fi
absent "no certificate Secret reload" 'secret.reloader.stakater.com/reload: "nke-controller-agent-client-tls"' "${out}"
absent "no certificate mount" 'mountPath: /etc/nke/node-agent-tls' "${out}"

if ((failures > 0)); then
  printf '%d node-agent TLS/JWT chart assertion(s) failed\n' "${failures}" >&2
  exit 1
fi
