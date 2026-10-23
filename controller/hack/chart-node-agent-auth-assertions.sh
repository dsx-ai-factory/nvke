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

# setting <description> <config.yaml key> <value> <rendered>
# Asserts one key of the rendered nke-controller-config, where the controller
# reads its settings. A list compares as its items joined with commas.
setting() {
  local got
  got="$(yq 'select(.kind == "ConfigMap" and .metadata.name == "nvke-controller-config") | .data["config.yaml"]' <<<"$4" |
    yq ".$2 | (select(tag == \"!!seq\") | join(\",\")) // .")"
  if [[ "${got}" == "$3" ]]; then
    printf '  ok    %s\n' "$1"
  else
    printf '  FAIL  %s (expected %s = %s, got %s)\n' "$1" "$2" "$3" "${got}" >&2
    failures=$((failures + 1))
  fi
}

echo "node-agent TLS/JWT disabled:"
out="$(helm template cwe "${CHART}")"
setting "capability is explicitly false" nodeAgent.tlsJWT.enabled false "${out}"
absent "no client identity mount" 'name: node-agent-client-tls' "${out}"
setting "no JWT issuer configuration" nodeAgent.tlsJWT.issuer "" "${out}"
setting "no JWT key-source configuration" nodeAgent.tlsJWT.jwksURI "" "${out}"

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
setting "capability is true" nodeAgent.tlsJWT.enabled true "${out}"
absent "no token configuration ConfigMap" 'name: nke-node-agent-auth' "${out}"
absent "no client identity Secret" 'secretName: nvke-controller-agent-client-tls' "${out}"
absent "no client identity mount" 'name: node-agent-client-tls' "${out}"
setting "JWT issuer comes from controller config" nodeAgent.tlsJWT.issuer https://identity.example.test "${out}"
setting "JWT key source comes from controller config" nodeAgent.tlsJWT.jwksURI https://identity.example.test/.well-known/jwks.json "${out}"
setting "JWT algorithms come from controller config" nodeAgent.tlsJWT.algorithms RS256,ES256 "${out}"
setting "JWT subject trust domain comes from controller config" nodeAgent.tlsJWT.subjectTrustDomain identity.example.test "${out}"
setting "JWT subject tenant comes from controller config" nodeAgent.tlsJWT.subjectTenant tenant-a "${out}"
setting "JWT subject service comes from controller config" nodeAgent.tlsJWT.subjectService node-agent "${out}"
setting "identity exchange URL comes from controller config" nodeAgent.tlsJWT.dsxIdentityExchangeURL https://identity.example.test/exchange "${out}"
if whitespace_out="$(helm template cwe "${CHART}" "${tls_jwt_values[@]}" \
  --set-string nodeAgent.tlsJwt.dsxIdentityExchangeURL='   ' 2>&1)"; then
  printf '  FAIL  whitespace-only identity exchange URL was accepted\n' >&2
  failures=$((failures + 1))
else
  want "whitespace-only identity exchange URL is rejected" 'nodeAgent.tlsJwt.dsxIdentityExchangeURL is required' "${whitespace_out}"
fi
absent "no certificate Secret reload" 'secret.reloader.stakater.com/reload: "nvke-controller-agent-client-tls"' "${out}"
absent "no certificate mount" 'mountPath: /etc/nvke/node-agent-client-tls' "${out}"

if ((failures > 0)); then
  printf '%d node-agent TLS/JWT chart assertion(s) failed\n' "${failures}" >&2
  exit 1
fi
