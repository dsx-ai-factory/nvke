#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Starts nico-sim in the background and waits for it to answer.
#
# Tilt supervises nico-sim as a local_resource serve_cmd, so every lane that
# reaches the simulator through Tilt gets it for free. The deploy path does not
# run Tilt at all -- backend-e2e.sh branches on NKE_E2E_USE_STACK and the two
# halves share nothing -- so without this nothing starts the simulator,
# nico-credentials points at a dead endpoint, and every NicoMachine hangs.
#
# The environment below mirrors the container-backend half of
# controller/nico-sim/Tiltfile. Keep the two in step: a value that drifts here
# does not fail, it changes what the deploy path is testing.
#
# Not a supervisor. It starts the process and leaves it; the runner container is
# torn down at the end of the lane, which is what stops it.

set -euo pipefail

: "${NICO_SIM_DIR:?NICO_SIM_DIR is required (the nico-sim source directory)}"
: "${NICO_SIM_BINARY:=/tmp/cwe-nico-sim}"
: "${NICO_SIM_PORT:=18080}"
: "${NICO_SIM_LOG:=/state/nico-sim.log}"
: "${NICO_SIM_PIDFILE:=/state/nico-sim.pid}"
: "${NICO_SIM_READY_TIMEOUT:=120}"
: "${CAPD_KINDEST_NODE_IMAGE:=cwe-capd-kindest-node:local}"

if [[ ! -x "${NICO_SIM_BINARY}" ]]; then
  echo "nico-sim binary is missing or not executable: ${NICO_SIM_BINARY}" >&2
  echo "backend-e2e.sh builds it on the host and copies it in; NICO_SIM_BINARY_PREBUILT was probably false." >&2
  exit 1
fi

# cd, not a flag: NICO_SIM_SEED_PATH defaults to ./internal/seed/default-seed.yaml,
# so the process only finds its seed data from the source directory.
cd "${NICO_SIM_DIR}"

dev_dir="${NICO_SIM_DIR}/.dev"
mkdir -p "${dev_dir}/logs" "${dev_dir}/files" "$(dirname "${NICO_SIM_LOG}")"

# VM backend mock, container backend on: the deploy path provisions kind-style
# node containers, exactly as tilt_config.json's nico_sim_backends=["container"]
# selects. mock is what makes NICO_VMS_BASE_IMAGE unnecessary -- config.go
# requires it for every other backend.
export NICO_SIM_LISTEN_ADDR=":${NICO_SIM_PORT}"
export NICO_SIM_DB_PATH="${dev_dir}/nico-sim.db"
export NICO_SIM_FILES_DIR="${dev_dir}/files"
export NICO_VMS_VM_BACKEND=mock
export NICO_VMS_BGP_ENABLED=false
# No default in config.go, and it must parse as a CIDR, so leaving it unset is
# a startup failure rather than a fallback.
export NICO_VMS_BRIDGE_CIDR="${NICO_VMS_BRIDGE_CIDR:-172.30.0.0/24}"
export NICO_VMS_BGP_VIP_CIDRS="${NICO_VMS_VIP_CIDR:-172.31.0.0/24}"
export NICO_VMS_VM_DISK_GIB=40
export NICO_CONTAINERS_ENABLED=true
export NICO_CONTAINERS_NODE_IMAGE="${CAPD_KINDEST_NODE_IMAGE}"
export NICO_CONTAINERS_REQUIRE_HEALTHY=true

echo "starting nico-sim on :${NICO_SIM_PORT} (log: ${NICO_SIM_LOG})"
# setsid where it exists, so the process does not share a process group with the
# docker exec that started it. Backgrounding alone is enough on a container that
# keeps running, but only if nothing signals the group.
if command -v setsid >/dev/null 2>&1; then
  setsid "${NICO_SIM_BINARY}" >"${NICO_SIM_LOG}" 2>&1 &
else
  nohup "${NICO_SIM_BINARY}" >"${NICO_SIM_LOG}" 2>&1 &
fi
sim_pid=$!
echo "${sim_pid}" >"${NICO_SIM_PIDFILE}"

deadline=$((SECONDS + NICO_SIM_READY_TIMEOUT))
while true; do
  if ! kill -0 "${sim_pid}" 2>/dev/null; then
    echo "nico-sim exited before it became ready:" >&2
    tail -n 100 "${NICO_SIM_LOG}" >&2 || true
    exit 1
  fi
  # /dev/tcp rather than curl: the runner image is not guaranteed to carry one,
  # and this is the same check Tilt's readiness_probe makes.
  if (exec 3<>"/dev/tcp/127.0.0.1/${NICO_SIM_PORT}") 2>/dev/null; then
    exec 3<&- 2>/dev/null || true
    break
  fi
  if (( SECONDS >= deadline )); then
    echo "nico-sim did not listen on :${NICO_SIM_PORT} within ${NICO_SIM_READY_TIMEOUT}s:" >&2
    tail -n 100 "${NICO_SIM_LOG}" >&2 || true
    exit 1
  fi
  sleep 2
done

echo "nico-sim is listening on :${NICO_SIM_PORT} (pid ${sim_pid})"
