#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Render the NKTP chart and validate every manifest with kubeconform.
# NKTP_CHART may override the chart directory. Its dependencies must already be
# built; `make nktp-chart-lint` does that first.

set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
chart="${NKTP_CHART:-${repo_root}/nktp/deploy/charts/nktp}"

# A directory, not `mktemp -t`: kubeconform selects files by extension. See
# lint-chart in make/checks.mk.
dir="$(mktemp -d)"
trap 'rm -rf "${dir}"' EXIT HUP INT TERM

rendered="${dir}/rendered.yaml"
helm template nktp "${chart}" > "${rendered}"
count="$(grep -c '^kind:' "${rendered}" || true)"
if [[ "${count}" -eq 0 ]]; then
  echo "ERROR: the chart rendered no resources." >&2
  exit 1
fi
echo "  rendered ${count} resources"

crds="${dir}/crds.yaml"
helm template nktp "${chart}" --include-crds \
  | yq 'select(.kind == "CustomResourceDefinition")' > "${crds}"

# The chart renders cert-manager Issuers and Certificates for the Operator's
# webhook, but cert-manager is a prerequisite rather than a dependency, so its
# CRDs come from the version a site runs. CERT_MANAGER_VERSION is pinned in
# make/tool-versions.mk; `make nktp-chart-lint` passes it.
: "${CERT_MANAGER_VERSION:?set CERT_MANAGER_VERSION, or run make nktp-chart-lint}"
{
  echo "---"
  helm template cert-manager oci://quay.io/jetstack/charts/cert-manager \
    --version "${CERT_MANAGER_VERSION}" --set crds.enabled=true \
    | yq 'select(.kind == "CustomResourceDefinition")'
} >> "${crds}"

strict='(.. | select(tag == "!!map" and has("properties") and (has("additionalProperties") | not) and ((.["x-kubernetes-preserve-unknown-fields"] // false) | not))) |= . + {"additionalProperties": false}'

schemas=0
while IFS= read -r crd; do
  q="select(.metadata.name == \"${crd}\")"
  group="$(yq "${q} | .spec.group" "${crds}")"
  kind="$(yq "${q} | .spec.names.kind | downcase" "${crds}")"
  mkdir -p "${dir}/schemas/${group}"
  while IFS= read -r version; do
    yq -o=json "${q} | .spec.versions[] | select(.name == \"${version}\") | .schema.openAPIV3Schema | ${strict}" \
      "${crds}" > "${dir}/schemas/${group}/${kind}_${version}.json"
    schemas=$((schemas + 1))
  done < <(yq "${q} | .spec.versions[].name" "${crds}")
done < <(yq '.metadata.name' "${crds}" | grep -v '^---$')

if [[ "${schemas}" -eq 0 ]]; then
  echo "ERROR: no CRD schemas were generated from the chart." >&2
  exit 1
fi
echo "  generated ${schemas} schemas from the chart's and cert-manager's CRDs"

kubeconform -strict -summary \
  -schema-location default \
  -schema-location "${dir}/schemas/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json" \
  "${rendered}"
