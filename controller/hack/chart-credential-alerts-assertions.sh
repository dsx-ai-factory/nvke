#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
cd "${repo_root}"
chart="${CHART:-controller/deploy/charts/controller}"

# shellcheck source=../../hack/read-tool-version.sh
. "${repo_root}/hack/read-tool-version.sh"

version="$(read_tool_version PROMTOOL_VERSION)"
schema_commit="$(read_tool_version PROMETHEUSRULE_SCHEMA_VERSION)"
schema_checksum="$(read_tool_version PROMETHEUSRULE_SCHEMA_SHA256)"
case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) echo "ERROR: promtool install supports Linux and macOS" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "ERROR: promtool install supports amd64 and arm64" >&2; exit 1 ;;
esac
platform="${os}-${arch}"
checksum="$(read_tool_version "PROMTOOL_SHA256_$(printf '%s_%s' "${os}" "${arch}" | tr '[:lower:]' '[:upper:]')")"
bin_dir="${repo_root}/.local/promtool/${version}-${platform}"

verify_sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    printf '%s  %s\n' "$1" "$2" | sha256sum -c -
  else
    printf '%s  %s\n' "$1" "$2" | shasum -a 256 -c -
  fi
}
mkdir -p "${bin_dir}"
tmp="$(mktemp -d "${bin_dir}/tmp.XXXXXX")"
trap 'rm -rf "${tmp}"' EXIT
trap 'exit 1' HUP INT TERM

# The chart CI job does not install promtool. Cache only a checksum-verified
# release binary, never a partial download, and use the same pin locally.
if [[ ! -x "${bin_dir}/promtool" ]]; then
  archive="prometheus-${version}.${os}-${arch}"
  echo "Installing promtool ${version} (${platform})"
  # Retry flags follow .github/workflows/README.md invariant 9; the per-attempt
  # cap keeps the last retry inside the chart job's timeout.
  curl -fSL --retry 8 --retry-all-errors --retry-max-time 240 --connect-timeout 10 --max-time 120 \
    "https://github.com/prometheus/prometheus/releases/download/v${version}/${archive}.tar.gz" \
    -o "${tmp}/prometheus.tar.gz"
  verify_sha256 "${checksum}" "${tmp}/prometheus.tar.gz"
  tar -xzf "${tmp}/prometheus.tar.gz" -C "${tmp}" "${archive}/promtool"
  mv "${tmp}/${archive}/promtool" "${bin_dir}/promtool"
fi
export PATH="${bin_dir}:${PATH}"
./hack/check-tool-version.sh promtool "${version}"
promtool check rules "${chart}/files/credential-alerts.yaml"
# The fixtures resolve rule_files relative to their own directory.
promtool test rules controller/deploy/monitoring/credential-alerts-test.yaml

# The rule is opt-in, and the umbrella monitoring switch overrides the opt-in.
# Render the whole chart so an absent template cannot hide a stray rule.
for disabled in "" "--set credentialMonitoring.alerts.enabled=true --set global.monitoring.enabled=false"; do
  # shellcheck disable=SC2086 # Intentional word splitting of the flag list.
  if helm template cwe "${chart}" ${disabled} | grep -q '^kind: PrometheusRule$'; then
    echo "ERROR: PrometheusRule rendered with ${disabled:-default values}" >&2
    exit 1
  fi
done

helm template cwe "${chart}" --set credentialMonitoring.alerts.enabled=true \
  --show-only templates/credential-prometheusrule.yaml > "${tmp}/rule.yaml"
grep -q '^kind: PrometheusRule$' "${tmp}/rule.yaml"
# Unlike the whole-chart render, do not ignore missing schemas here: the new
# resource must actually be validated, not merely counted as skipped.
# kubeconform cannot retry its own fetch, so download the one schema with the
# same retries as the tool download and validate against the local copy. The
# catalog commit and digest are pinned, so an upstream change cannot alter the
# result for an unchanged commit.
mkdir -p "${tmp}/schemas/monitoring.coreos.com"
curl -fSL --retry 8 --retry-all-errors --retry-max-time 240 --connect-timeout 10 --max-time 120 \
  "https://raw.githubusercontent.com/datreeio/CRDs-catalog/${schema_commit}/monitoring.coreos.com/prometheusrule_v1.json" \
  -o "${tmp}/schemas/monitoring.coreos.com/prometheusrule_v1.json"
verify_sha256 "${schema_checksum}" "${tmp}/schemas/monitoring.coreos.com/prometheusrule_v1.json"
kubeconform -strict -summary \
  -schema-location "${tmp}/schemas/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json" \
  "${tmp}/rule.yaml"
