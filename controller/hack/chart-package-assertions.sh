#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Assert that the documented packaging command replaces both placeholder chart
# versions. CHART and CHART_PACKAGE_VERSION may override the defaults.

set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
chart="${CHART:-${repo_root}/controller/deploy/charts/cloud-workflow-engine}"
version="${CHART_PACKAGE_VERSION:-0.0.0-$(git -C "${repo_root}" rev-parse --short=12 HEAD)}"
dist="$(mktemp -d)"
trap 'rm -rf "${dist}"' EXIT HUP INT TERM

helm package "${chart}" \
  --version "${version}" \
  --app-version "${version}" \
  --destination "${dist}" >/dev/null

archive=""
for candidate in "${dist}"/*.tgz; do
  [[ -e "${candidate}" ]] || continue
  if [[ -n "${archive}" ]]; then
    echo "ERROR: helm package produced more than one archive" >&2
    exit 1
  fi
  archive="${candidate}"
done
[[ -n "${archive}" ]] || { echo "ERROR: helm package produced no archive" >&2; exit 1; }

# Consume the complete tar listing. Exiting awk at the first match closes the
# pipe early, and GNU tar treats the resulting SIGPIPE as a write error under
# pipefail even though BSD tar does not.
chart_yaml="$(tar -tzf "${archive}" | awk '/\/Chart.yaml$/ && chart == "" { chart = $0 } END { print chart }')"
[[ -n "${chart_yaml}" ]] || { echo "ERROR: packaged chart has no Chart.yaml" >&2; exit 1; }

metadata="$(tar -xOzf "${archive}" "${chart_yaml}")"
packaged_version="$(awk '$1 == "version:" { gsub(/"/, "", $2); print $2; exit }' <<<"${metadata}")"
packaged_app_version="$(awk '$1 == "appVersion:" { gsub(/"/, "", $2); print $2; exit }' <<<"${metadata}")"

if [[ "${packaged_version}" != "${version}" ]]; then
  echo "ERROR: packaged chart version is ${packaged_version:-missing}; expected ${version}" >&2
  exit 1
fi
if [[ "${packaged_app_version}" != "${version}" ]]; then
  echo "ERROR: packaged chart appVersion is ${packaged_app_version:-missing}; expected ${version}" >&2
  exit 1
fi

echo "packaged $(basename "${archive}") at version ${packaged_version}"
