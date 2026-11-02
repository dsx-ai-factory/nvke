#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Asserts what the chart renders for supportedInfraProviders and the per-provider
# RBAC toggles, and that it refuses a provider list the controller would refuse
# at startup.

set -euo pipefail

CHART="${CHART:-"$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/deploy/charts/controller"}"
[[ -d "${CHART}" ]] || { echo "chart not found at ${CHART}" >&2; exit 2; }

failures=0

render() { helm template cwe "${CHART}" "$@"; }

# setting <description> <config.yaml key> <value> <rendered>
# Asserts one key of the rendered nvke-controller-config, where the controller
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

# clusterrole <description> <name suffix> <expected count> <rendered>
# Asserts how many ClusterRoles named <fullname>-<suffix> were rendered.
clusterrole() {
  local got
  got="$(yq "select(.kind == \"ClusterRole\" and .metadata.name == \"cwe-nvke-controller-$2\") | .metadata.name" <<<"$4" | grep -c . || true)"
  if [[ "${got}" == "$3" ]]; then
    printf '  ok    %s\n' "$1"
  else
    printf '  FAIL  %s (expected %s ClusterRole(s) -%s, got %s)\n' "$1" "$3" "$2" "${got}" >&2
    failures=$((failures + 1))
  fi
}

echo "supportedInfraProviders default:"
out="$(render)"
setting "defaults deployed controller to nico only" supportedInfraProviders "nico" "${out}"

echo "supportedInfraProviders opt-in:"
out="$(render --set-string 'supportedInfraProviders=nico\,infra')"
setting "renders both providers when requested" supportedInfraProviders "nico,infra" "${out}"
out="$(render --set-string 'supportedInfraProviders=nico\,infra\,launchLayer')"
setting "renders launchLayer appended to the overlay value" supportedInfraProviders "nico,infra,launchLayer" "${out}"
wantfail "rejects an unknown provider" \
  'supportedInfraProviders: "launchlayer" is not a supported provider' \
  --set-string 'supportedInfraProviders=nico\,launchlayer'
wantfail "rejects a repeated provider" \
  'supportedInfraProviders: "nico" is listed more than once' \
  --set-string 'supportedInfraProviders=nico\, nico'

echo "provider RBAC toggles:"
out="$(render --set capi.rbac.create=true)"
clusterrole "grants NICo RBAC by default" nico 1 "${out}"
clusterrole "grants LaunchLayer RBAC by default" launchlayer 1 "${out}"
out="$(render --set capi.rbac.create=true --set capi.rbac.nico=false --set controllers.nico.enabled=false)"
clusterrole "omits NICo RBAC when capi.rbac.nico is false" nico 0 "${out}"
out="$(render --set capi.rbac.create=true --set capi.rbac.launchLayer=false)"
clusterrole "omits LaunchLayer RBAC when capi.rbac.launchLayer is false" launchlayer 0 "${out}"

if [[ "${failures}" -ne 0 ]]; then
  echo "${failures} chart provider assertion(s) failed" >&2
  exit 1
fi

echo "all chart provider assertions passed"
