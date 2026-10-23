#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Shared by mac-vm-dev-up.sh and mac-vm-dev-down.sh: paths, environment, and
# the narration helpers that print each step and the commands it runs.
# shellcheck disable=SC2034 # the variables here are read by the sourcing scripts

CONTROLLER_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO_ROOT="$(cd "${CONTROLLER_DIR}/.." && pwd)"
STATE_DIR="${CONTROLLER_DIR}/.local/mac-vm-dev"

export COLIMA_PROFILE="${COLIMA_PROFILE:-nvke-mac}"
export KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-cwe-capi}"
TILT_PORT="${TILT_PORT:-10350}"
NVKE_API="http://localhost:${NKE_CONTROLLER_HTTP_PORT:-8080}/api"

ASSUME_YES=false

# reexec_in_flox runs the calling script inside this checkout's Flox
# environment, which pins the toolchain to CI's versions. Flox is optional:
# without it the tools come from PATH, and mac-vm-dev-up.sh checks for them.
reexec_in_flox() {
  command -v flox >/dev/null || return 0
  # An environment activated from another checkout can lack this branch's tools.
  if [[ "${FLOX_ENV:-}" != "${REPO_ROOT}/.flox/"* ]]; then
    exec flox activate -d "${REPO_ROOT}" -- bash "$0" "$@"
  fi
}

step() {
  printf '\n\033[1;34m==> %s\033[0m\n' "$1"
  shift
  local line
  for line in "$@"; do printf '    %s\n' "${line}"; done
}

note() { printf '    \033[2m%s\033[0m\n' "$*"; }

die() {
  printf '\033[1;31merror:\033[0m %s\n' "$*" >&2
  exit 1
}

# run prints a command to stderr, so callers can capture its output, then runs it.
run() {
  printf '    \033[36m$ %s\033[0m\n' "$*" >&2
  "$@"
}

pause() {
  "${ASSUME_YES}" && return
  read -r -p $'    \033[2mPress Enter to continue, Ctrl-C to stop.\033[0m ' _
}

# use_colima_docker points Docker, kind and the Makefile at the Colima profile.
# Colima keeps its socket under ~/.colima or ~/.config/colima depending on how
# it was installed, so ask it rather than guess.
use_colima_docker() {
  local sock
  sock="$(colima status --profile "${COLIMA_PROFILE}" --json | jq -r .docker_socket)"
  export COLIMA_DOCKER_HOST="${sock}" DOCKER_HOST="${sock}"
}

api() {
  local path="$1"
  shift
  curl -fsS -H "Authorization: Bearer $("${CONTROLLER_DIR}/hack/nvke-token.sh")" "${NVKE_API}${path}" "$@"
}
