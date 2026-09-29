#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Asserts the deployment-wide SPIRE attestation method rendered by the chart.

set -euo pipefail

CHART="${CHART:-"$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/deploy/charts/controller"}"
[[ -d "${CHART}" ]] || { echo "chart not found at ${CHART}" >&2; exit 2; }

failures=0

render() { helm template cwe "${CHART}" "$@"; }

pair() {
  if grep -A1 "name: $2" <<<"$4" | grep -q "value: \"$3\""; then
    printf '  ok    %s\n' "$1"
  else
    printf '  FAIL  %s (expected %s = %s)\n' "$1" "$2" "$3" >&2
    failures=$((failures + 1))
  fi
}

wantfail() {
  local description="$1" pattern="$2"
  shift 2
  local output
  if output="$(render "$@" 2>&1)"; then
    printf '  FAIL  %s (render succeeded; expected it to fail)\n' "${description}" >&2
    failures=$((failures + 1))
  elif grep -qF "${pattern}" <<<"${output}"; then
    printf '  ok    %s\n' "${description}"
  else
    printf '  FAIL  %s (failed, but not with: %s)\n' "${description}" "${pattern}" >&2
    failures=$((failures + 1))
  fi
}

output="$(render)"
pair "defaults to TPM" "NODE_AGENT_SPIRE_ATTESTATION_METHOD" "tpm" "${output}"

output="$(render --set-string nodeAgent.spireAttestationMethod=joinToken --set spiffe.enabled=true)"
pair "renders join token" "NODE_AGENT_SPIRE_ATTESTATION_METHOD" "joinToken" "${output}"

output="$(render --set-string nodeAgent.spireAttestationMethod=dsxImds --set-string nodeAgent.dsxImds.operatorTenant=operator-1 --set spiffe.enabled=true)"
pair "renders DSX IMDS method" "NODE_AGENT_SPIRE_ATTESTATION_METHOD" "dsxImds" "${output}"
pair "renders DSX IMDS operator tenant" "NODE_AGENT_DSX_IMDS_OPERATOR_TENANT" "operator-1" "${output}"
pair "renders default DSX IMDS endpoint" "NODE_AGENT_DSX_IMDS_ENDPOINT" "http://169.254.169.254" "${output}"
pair "renders empty checksum override" "NODE_AGENT_DSX_IMDS_PLUGIN_CHECKSUM" "" "${output}"

checksum="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
output="$(render --set-string nodeAgent.spireAttestationMethod=dsxImds --set-string nodeAgent.dsxImds.operatorTenant=operator-1 --set spiffe.enabled=true --set-string nodeAgent.dsxImds.endpoint=https://metadata.example.test --set-string nodeAgent.dsxImds.pluginChecksum=${checksum})"
pair "renders DSX IMDS endpoint override" "NODE_AGENT_DSX_IMDS_ENDPOINT" "https://metadata.example.test" "${output}"
pair "renders DSX IMDS checksum override" "NODE_AGENT_DSX_IMDS_PLUGIN_CHECKSUM" "${checksum}" "${output}"

wantfail \
  "rejects join token when SPIFFE is disabled" \
  'spiffe.enabled must be true when nodeAgent.spireAttestationMethod is joinToken' \
  --set-string nodeAgent.spireAttestationMethod=joinToken \
  --set spiffe.enabled=false

wantfail \
  "rejects unsupported methods" \
  'nodeAgent.spireAttestationMethod must be one of tpm, joinToken, or dsxImds' \
  --set-string nodeAgent.spireAttestationMethod=unsupported

wantfail \
	"rejects an empty DSX IMDS operator tenant" \
	'nodeAgent.dsxImds.operatorTenant is required' \
	--set-string nodeAgent.spireAttestationMethod=dsxImds \
	--set spiffe.enabled=true

wantfail \
  "rejects a DSX IMDS operator tenant with a slash" \
  'nodeAgent.dsxImds.operatorTenant must be a valid SPIFFE path segment' \
  --set-string nodeAgent.spireAttestationMethod=dsxImds \
  --set-string nodeAgent.dsxImds.operatorTenant=operator/one \
  --set spiffe.enabled=true

wantfail \
  "rejects a dot-only DSX IMDS operator tenant" \
  'nodeAgent.dsxImds.operatorTenant must be a valid SPIFFE path segment' \
  --set-string nodeAgent.spireAttestationMethod=dsxImds \
  --set-string nodeAgent.dsxImds.operatorTenant=. \
  --set spiffe.enabled=true

wantfail \
	"rejects an empty DSX IMDS endpoint" \
	'nodeAgent.dsxImds.endpoint is required' \
	--set-string nodeAgent.spireAttestationMethod=dsxImds \
	--set-string nodeAgent.dsxImds.operatorTenant=operator-1 \
  --set spiffe.enabled=true \
  --set-string nodeAgent.dsxImds.endpoint=

wantfail \
  "rejects a non-HTTP DSX IMDS endpoint" \
  'nodeAgent.dsxImds.endpoint must be an absolute HTTP(S) URL with a host' \
  --set-string nodeAgent.spireAttestationMethod=dsxImds \
  --set-string nodeAgent.dsxImds.operatorTenant=operator-1 \
  --set spiffe.enabled=true \
  --set-string nodeAgent.dsxImds.endpoint=ftp://metadata.example.test

wantfail \
  "rejects a DSX IMDS endpoint without a host" \
  'nodeAgent.dsxImds.endpoint must be an absolute HTTP(S) URL with a host' \
  --set-string nodeAgent.spireAttestationMethod=dsxImds \
  --set-string nodeAgent.dsxImds.operatorTenant=operator-1 \
  --set spiffe.enabled=true \
  --set-string nodeAgent.dsxImds.endpoint=http:///metadata

wantfail \
  "rejects an invalid DSX IMDS checksum" \
  'nodeAgent.dsxImds.pluginChecksum must be empty or a 64-character SHA-256 hex digest' \
	--set-string nodeAgent.spireAttestationMethod=dsxImds \
	--set-string nodeAgent.dsxImds.operatorTenant=operator-1 \
  --set spiffe.enabled=true \
  --set-string nodeAgent.dsxImds.pluginChecksum=abc123

wantfail \
  "rejects DSX IMDS when SPIFFE is disabled" \
  'spiffe.enabled must be true when nodeAgent.spireAttestationMethod is dsxImds' \
	--set-string nodeAgent.spireAttestationMethod=dsxImds \
	--set-string nodeAgent.dsxImds.operatorTenant=operator-1 \
  --set spiffe.enabled=false

if [[ "${failures}" -ne 0 ]]; then
  echo "${failures} chart SPIRE attestation assertion(s) failed" >&2
  exit 1
fi

echo "all chart SPIRE attestation assertions passed"
