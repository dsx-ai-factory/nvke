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
# Only processes this checkout started: each instance directory under .dev holds
# the PID files nico-sim's runtime wrote, so another checkout's VMs are left
# alone. vmnet-client.pid holds the process group of vmnet-client and QEMU.
# A PID file can outlive its process (a reboot, say), so a PID counts only while
# its command line still names the instance directory.
tracked=()
groups=()
owned_pid() { # pidfile
  local pid
  pid="$(<"$1")"
  ps -p "${pid}" -o command= 2>/dev/null | grep -qF "$(dirname "$1")" && echo "${pid}"
}
for pidfile in "${CONTROLLER_DIR}"/nico-sim/.dev/instances/*/{qemu,swtpm}.pid; do
  [[ -f ${pidfile} ]] && pid="$(owned_pid "${pidfile}")" && tracked+=("${pid}")
done
for pidfile in "${CONTROLLER_DIR}"/nico-sim/.dev/instances/*/vmnet-client.pid; do
  [[ -f ${pidfile} ]] && pid="$(owned_pid "${pidfile}")" && groups+=("${pid}")
done
if ((${#tracked[@]} + ${#groups[@]} > 0)); then
  for pgid in ${groups[@]+"${groups[@]}"}; do
    run kill -TERM -- "-${pgid}" 2>/dev/null || true
  done
  for pid in ${tracked[@]+"${tracked[@]}"}; do
    run kill -TERM "${pid}" 2>/dev/null || true
  done
  note "waiting for the VM processes to exit"
  alive=true
  for _ in $(seq 30); do
    alive=false
    for pid in ${tracked[@]+"${tracked[@]}"}; do kill -0 "${pid}" 2>/dev/null && alive=true; done
    for pgid in ${groups[@]+"${groups[@]}"}; do kill -0 -- "-${pgid}" 2>/dev/null && alive=true; done
    "${alive}" || break
    sleep 1
  done
  if "${alive}"; then
    note "still running after 30s; sending SIGKILL"
    for pgid in ${groups[@]+"${groups[@]}"}; do kill -KILL -- "-${pgid}" 2>/dev/null || true; done
    for pid in ${tracked[@]+"${tracked[@]}"}; do kill -KILL "${pid}" 2>/dev/null || true; done
  fi
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
