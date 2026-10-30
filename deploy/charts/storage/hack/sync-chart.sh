#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Refresh ../vendor/local-path-provisioner from Rancher's repository.
#
#   deploy/charts/storage/hack/sync-chart.sh [version]
#
# The chart is vendored because Rancher publishes no Helm repository for it.
# The chart lives in the provisioner's own git repository, under
# deploy/chart, and a git reference is not something a Helm dependency can
# name. Third-party republishes of it exist; this repository does not depend
# on one.
#
# The default version is LOCAL_PATH_PROVISIONER_VERSION from
# make/tool-versions.mk, so the chart and `deploy/nvke-deploy-local.sh` install
# the same provisioner. Pass a version to override, and update the pin as well
# so the two do not drift.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${CHART_DIR}/../../.." && pwd)"
UPSTREAM="https://github.com/rancher/local-path-provisioner.git"

version="${1:-}"
if [[ -z "${version}" ]]; then
  version="$(sed -n 's/^LOCAL_PATH_PROVISIONER_VERSION[[:space:]]*?*=[[:space:]]*//p' \
    "${REPO_ROOT}/make/tool-versions.mk" | head -1)"
fi
if [[ -z "${version}" ]]; then
  echo "no version given and make/tool-versions.mk pins no LOCAL_PATH_PROVISIONER_VERSION" >&2
  exit 1
fi

work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

git clone --quiet --depth 1 --branch "${version}" "${UPSTREAM}" "${work}/src"

src="${work}/src/deploy/chart/local-path-provisioner"
if [[ ! -f "${src}/Chart.yaml" ]]; then
  echo "${version} has no deploy/chart/local-path-provisioner" >&2
  exit 1
fi

rm -rf "${CHART_DIR}/vendor/local-path-provisioner"
mkdir -p "${CHART_DIR}/vendor"
cp -R "${src}" "${CHART_DIR}/vendor/local-path-provisioner"
cp "${work}/src/LICENSE" "${CHART_DIR}/vendor/local-path-provisioner/LICENSE"

cat > "${CHART_DIR}/vendor/README.md" <<EOT
# vendor

[\`local-path-provisioner\`](local-path-provisioner/) is Rancher's chart,
vendored verbatim from ${UPSTREAM} at **${version}**, under Apache-2.0. Its
LICENSE travels with it.

Do not edit it. Refresh it with:

\`\`\`bash
deploy/charts/storage/hack/sync-chart.sh [version]
\`\`\`

Rancher publishes no Helm repository, and the chart lives inside the
provisioner's git repository, which a Helm dependency cannot name. The default
version is \`LOCAL_PATH_PROVISIONER_VERSION\` from \`make/tool-versions.mk\`, so
this chart and \`deploy/nvke-deploy-local.sh\` install the same provisioner.
EOT

printf 'vendored local-path-provisioner %s\n' "${version}"
