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

wantfail \
  "rejects join token when SPIFFE is disabled" \
  'spiffe.enabled must be true when nodeAgent.spireAttestationMethod is joinToken' \
  --set-string nodeAgent.spireAttestationMethod=joinToken \
  --set spiffe.enabled=false

wantfail \
  "rejects unsupported methods" \
  'nodeAgent.spireAttestationMethod must be one of tpm or joinToken' \
  --set-string nodeAgent.spireAttestationMethod=unsupported

if [[ "${failures}" -ne 0 ]]; then
  echo "${failures} chart SPIRE attestation assertion(s) failed" >&2
  exit 1
fi

echo "all chart SPIRE attestation assertions passed"
