#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Asserts what the chart renders for SUPPORTED_INFRA_PROVIDERS. The controller
# binary owns value validation at startup; this only checks the chart's default
# and opt-in rendering.

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

echo "supportedInfraProviders default:"
out="$(render)"
pair "defaults deployed CWE to nico only" "SUPPORTED_INFRA_PROVIDERS" "nico" "${out}"

echo "supportedInfraProviders opt-in:"
out="$(render --set-string 'supportedInfraProviders=nico\,infra')"
pair "renders both providers when requested" "SUPPORTED_INFRA_PROVIDERS" "nico,infra" "${out}"

if [[ "${failures}" -ne 0 ]]; then
  echo "${failures} chart provider assertion(s) failed" >&2
  exit 1
fi

echo "all chart provider assertions passed"
