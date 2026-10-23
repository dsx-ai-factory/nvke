#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Tear down what mac-vm-dev-up.sh brought up: Tilt, the nico-sim VMs and their
# state, the kind management cluster and the Colima VM. Nothing is deleted
# gracefully through NVKE: the management plane goes with kind anyway.
#
#   --yes    do not pause between steps
#
# The node image under os-image/packer/output-non-fde/ is kept; it takes about
# ten minutes to bake.

set -euo pipefail
# shellcheck source=hack/mac-vm-dev-lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/mac-vm-dev-lib.sh"
reexec_in_flox "$@"

for arg in "$@"; do
  case "${arg}" in
    --yes) ASSUME_YES=true ;;
    *) die "unknown argument ${arg}" ;;
  esac
done

step "1. Stop Tilt" \
  "Stopping 'tilt up' also stops what it runs on the host: nico-sim and the" \
  "port-forwards."
pause
if pkill -f "tilt up .*--port ${TILT_PORT}"; then
  note "waiting for Tilt to exit"
  while pgrep -f "tilt up .*--port ${TILT_PORT}" >/dev/null; do sleep 1; done
else
  note "Tilt was not running"
fi
rm -f "${STATE_DIR}/tilt.pid"

step "2. Stop the nico-sim VMs and delete their state" \
  "nico-sim leaves its VMs running when it exits. Its database goes too: with" \
  "the management plane gone, any instance left in it would be rebuilt as an" \
  "orphan on the next start."
pause
if pgrep -f 'qemu-system-aarch64 .*-name nico-vm-' >/dev/null; then
  run pkill -f 'qemu-system-aarch64 .*-name nico-vm-'
  pkill -f 'swtpm .*nico-sim' || true
else
  note "no VMs running"
fi
run rm -rf "${CONTROLLER_DIR}/nico-sim/.dev"

step "3. Delete the kind management cluster" \
  "This removes NVKE, Temporal, Postgres, SPIRE and CAPI along with it."
if colima status --profile "${COLIMA_PROFILE}" >/dev/null 2>&1; then
  use_colima_docker
  pause
  run make -C "${CONTROLLER_DIR}" kind-down
else
  note "Colima profile ${COLIMA_PROFILE} is not running; skipping"
fi

step "4. Stop the Colima VM" \
  "Stopped, not deleted, so its image cache makes the next bring-up faster."
if colima status --profile "${COLIMA_PROFILE}" >/dev/null 2>&1; then
  pause
  run colima stop --profile "${COLIMA_PROFILE}"
else
  note "already stopped"
fi

step "Done" "Bring it back with: hack/mac-vm-dev-up.sh"
