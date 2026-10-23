#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Run the nico-vms e2e specs against the stack hack/mac-vm-dev-up.sh brought
# up, as hack/ci/nico-vms-e2e.sh runs them on a Linux KVM host. Each entry
# creates its own tenant cluster and deletes it again; a failed one is kept
# for debugging, and the run stops there.
#
# Environment:
#   LABEL_FILTER          Ginkgo label filter (default: the arm64 CI lane's, less
#                         parallel-lifecycle: its Calico tenant cannot exist here,
#                         because NVKE allows a custom pod CIDR only with OVN and
#                         Calico's default 192.168.0.0/16 overlaps vmnet's node network)
#   E2E_TIMEOUT           suite timeout       (default: 270m, as in CI)
#   NKE_E2E_UPGRADE_HOPS  upgrade hops to run (default: 1, as in CI)

set -euo pipefail
# shellcheck source=hack/mac-vm-dev-lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/mac-vm-dev-lib.sh"
reexec_in_flox "$@"

LABEL_FILTER="${LABEL_FILTER:-nico-vms && (happy-path || ovn || multi-pod-cidrs)}"
E2E_TIMEOUT="${E2E_TIMEOUT:-270m}"
export NKE_E2E_UPGRADE_HOPS="${NKE_E2E_UPGRADE_HOPS:-1}"
mkdir -p "${STATE_DIR}"

jq -n --arg context "kind-${KIND_CLUSTER_NAME}" \
  '{kube: {context: $context}, lifecycle: {preserveOnFailure: true}}' >"${STATE_DIR}/e2e-config.json"

cd "${CONTROLLER_DIR}/test/e2e"
# The fleet releases fixture gives the release matrix its metadata, as in CI.
# Ginkgo enforces the timeout; Go's own would kill the run without a report.
NKE_CONTROLLER_RELEASES_DIR="${CONTROLLER_DIR}/test/fixtures/fleet-releases" run go test -v -timeout 0 . -args \
  -e2e.config="${STATE_DIR}/e2e-config.json" \
  -ginkgo.v \
  -ginkgo.fail-on-empty \
  -ginkgo.fail-fast \
  -ginkgo.timeout="${E2E_TIMEOUT}" \
  -ginkgo.label-filter="${LABEL_FILTER}" \
  -ginkgo.junit-report="${STATE_DIR}/e2e-junit.xml" 2>&1 | tee "${STATE_DIR}/e2e.log"
