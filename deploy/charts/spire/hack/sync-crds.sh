#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Refresh ../crds from the upstream spire-crds chart.
#
#   deploy/charts/spire/hack/sync-crds.sh [version]
#
# The CRDs are vendored rather than installed as a chart dependency because
# helm validates every manifest in a release against the API server BEFORE it
# applies any of them. A ClusterSPIFFEID and the CRD that defines it therefore
# cannot be in one release: the install fails with
#
#   no matches for kind "ClusterSPIFFEID" ... ensure CRDs are installed first
#
# A chart's crds/ directory is applied first, with a discovery refresh after
# it, which is the one ordering helm does guarantee.
#
# The cost is that helm NEVER upgrades a crds/ directory, not on install and
# not on upgrade. Moving to a newer SPIRE chart means applying the new CRDs by
# hand:  kubectl apply --server-side -f deploy/charts/spire/crds/
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_URL="https://spiffe.github.io/helm-charts-hardened/"

# Defaults to the spire-crds version that pairs with the spire version pinned
# in Chart.yaml. Pass one to override.
VERSION="${1:-0.6.1}"

work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

helm pull spire-crds --repo "${REPO_URL}" --version "${VERSION}" --untar --untardir "${work}"

rm -f "${CHART_DIR}"/crds/*.yaml
for f in "${work}"/spire-crds/templates/*.yaml; do
  name="$(basename "${f}")"
  {
    printf '# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.\n'
    printf '# SPDX-License-Identifier: Apache-2.0\n\n'
    printf '# Vendored from the spire-crds chart %s. Do not edit by hand:\n' "${VERSION}"
    printf '#   deploy/charts/spire/hack/sync-crds.sh %s\n' "${VERSION}"
    helm template spire-crds "${work}/spire-crds" --show-only "templates/${name}" | sed '/^---$/d; /^# Source:/d'
  } > "${CHART_DIR}/crds/${name}"
done

printf 'synced %s CRDs from spire-crds %s\n' "$(find "${CHART_DIR}/crds" -name '*.yaml' | wc -l)" "${VERSION}"
