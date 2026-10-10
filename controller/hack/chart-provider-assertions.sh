#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Asserts what the chart renders for supportedInfraProviders. The controller
# binary owns value validation at startup; this only checks the chart's default
# and opt-in rendering.

set -euo pipefail

CHART="${CHART:-"$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/deploy/charts/controller"}"
[[ -d "${CHART}" ]] || { echo "chart not found at ${CHART}" >&2; exit 2; }

failures=0

render() { helm template cwe "${CHART}" "$@"; }

# setting <description> <config.yaml key> <value> <rendered>
# Asserts one key of the rendered nke-controller-config, where the controller
# reads its settings. A list compares as its items joined with commas.
setting() {
  local got
  got="$(yq 'select(.kind == "ConfigMap" and .metadata.name == "nke-controller-config") | .data["config.yaml"]' <<<"$4" |
    yq ".$2 | (select(tag == \"!!seq\") | join(\",\")) // .")"
  if [[ "${got}" == "$3" ]]; then
    printf '  ok    %s\n' "$1"
  else
    printf '  FAIL  %s (expected %s = %s, got %s)\n' "$1" "$2" "$3" "${got}" >&2
    failures=$((failures + 1))
  fi
}

echo "supportedInfraProviders default:"
out="$(render)"
setting "defaults deployed controller to nico only" supportedInfraProviders "nico" "${out}"

echo "supportedInfraProviders opt-in:"
out="$(render --set-string 'supportedInfraProviders=nico\,infra')"
setting "renders both providers when requested" supportedInfraProviders "nico,infra" "${out}"

if [[ "${failures}" -ne 0 ]]; then
  echo "${failures} chart provider assertion(s) failed" >&2
  exit 1
fi

echo "all chart provider assertions passed"
