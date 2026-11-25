#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# The container engine behind the local dev flow (Tilt, controller/Makefile.dev,
# the hack scripts, nico-sim's container backend, kind). Source it; it defines
# functions only.
#
# CONTAINER_CLI selects the engine CLI, in the environment of both `make` and
# `tilt up`: docker, which is what an unset value means, nerdctl, or ctrd.
# nerdctl runs the flow on containerd with no Docker daemon. ctrd runs it on
# containerd alone: it is this repository's own docker-compatible CLI over ctr,
# the CNI plugins and buildctl (hack/container-engine/ctrd), and there is no
# nerdctl on the host. It is the same variable
# hack/capd-kindest-node/build-image.sh already reads; OCI_BIN, which the CNI
# bundle builds read, and KIND_EXPERIMENTAL_PROVIDER follow it.
#
# What nerdctl and ctrd change, all of it here or in the callers' one-line uses of it:
#   - shim/nerdctl runs the real nerdctl through `sudo -n`, because containerd is
#     rootful. ce_init puts it first on PATH. shim/ctrd/ctrd does the same for
#     bin/ctrd, and shim/ctrd/nerdctl is the same script under the name kind
#     looks up: kind has providers for docker, podman and nerdctl only, and
#     runs a binary called nerdctl. No nerdctl is installed or executed.
#   - kind runs on its nerdctl provider, which has no `kind load docker-image`.
#     ce_kind_load saves the image and loads the archive, the way nvkectl does.
#   - nerdctl has no `network connect` and no `{{.Server.Arch}}`; the callers
#     that used either do something else under nerdctl.
#   - Docker accepts forwarded traffic that enters and leaves one bridge, which
#     is the path to the control-plane VIP. nerdctl and ctrd do not add that rule, and
#     the nvkectl bootstrap lane (cli/e2e/bootstrap/run-nico.sh) adds it itself:
#     ce_allow_same_bridge_forwarding.

CE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CE_SHIM_DIR="${CE_DIR}/shim"

ce_cli() { printf '%s\n' "${CONTAINER_CLI:-docker}"; }

# ce_init rejects an unsupported engine and prepares the environment for
# nerdctl or ctrd. It changes nothing for docker. Idempotent.
ce_init() {
  case "$(ce_cli)" in
    docker) ;;
    nerdctl)
      case ":${PATH}:" in
        ":${CE_SHIM_DIR}:"*) ;;
        *) export PATH="${CE_SHIM_DIR}:${PATH}" ;;
      esac
      export KIND_EXPERIMENTAL_PROVIDER=nerdctl
      ;;
    ctrd)
      case ":${PATH}:" in
        ":${CE_SHIM_DIR}/ctrd:"*) ;;
        *) export PATH="${CE_SHIM_DIR}/ctrd:${PATH}" ;;
      esac
      # kind's provider for this engine is its nerdctl one, and the shim directory
      # holds the binary of that name that kind runs.
      export KIND_EXPERIMENTAL_PROVIDER=nerdctl
      ;;
    *)
      echo "CONTAINER_CLI=$(ce_cli) is not supported; use docker (the default), nerdctl or ctrd" >&2
      return 1
      ;;
  esac
}

# ce_build ARGS... builds an image the way `docker build` does.
ce_build() {
  if [[ "$(ce_cli)" == docker ]]; then
    DOCKER_BUILDKIT=1 docker build "$@"
  else
    "$(ce_cli)" build "$@"
  fi
}

# ce_kind_load IMAGE CLUSTER loads a locally built image into a kind cluster.
#
# `kind load docker-image` runs `docker` whatever the provider is. The archive
# goes through a directory of its own and the caller's shell writes it, so it
# stays readable by the user that runs kind.
#
# This is for images the engine built here, which are one manifest. An image
# pulled as a multi-platform index needs nvkectl's flattening first
# (cli/internal/bootstrap/engine).
ce_kind_load() {
  local image="$1" cluster="$2" dir status=0
  if [[ "$(ce_cli)" == docker ]]; then
    "${KIND_BIN:-kind}" load docker-image "${image}" --name "${cluster}"
    return
  fi
  dir="$(mktemp -d)"
  "$(ce_cli)" save "${image}" >"${dir}/image.tar" &&
    "${KIND_BIN:-kind}" load image-archive "${dir}/image.tar" --name "${cluster}" || status=$?
  rm -rf "${dir}"
  return "${status}"
}

# ce_as_root CMD... runs CMD as root, without ever prompting.
ce_as_root() {
  if (( EUID == 0 )); then
    "$@"
  else
    sudo -n "$@"
  fi
}

# ce_bridge_of NETWORK prints the host interface nerdctl or ctrd gives NETWORK's bridge: br- and the first 12 digits of its ID.
ce_bridge_of() {
  local id
  id="$("$(ce_cli)" network inspect --format '{{.Id}}' "$1")" || return 1
  printf 'br-%s\n' "${id:0:12}"
}

# ce_allow_same_bridge_forwarding NETWORK lets traffic that enters and leaves
# NETWORK's bridge be forwarded: the control-plane VIP is reached through the
# host's BGP route, so a worker's packet comes in on the bridge and goes back
# out of it. A no-op for docker, which allows it already.
ce_allow_same_bridge_forwarding() {
  [[ "$(ce_cli)" != docker ]] || return 0
  local bridge
  bridge="$(ce_bridge_of "$1")" || return 1
  ce_as_root iptables -C FORWARD -i "${bridge}" -o "${bridge}" -j ACCEPT 2>/dev/null ||
    ce_as_root iptables -I FORWARD 1 -i "${bridge}" -o "${bridge}" -j ACCEPT
}

# ce_remove_same_bridge_forwarding NETWORK undoes it. Nothing to undo when the
# network is already gone.
ce_remove_same_bridge_forwarding() {
  [[ "$(ce_cli)" != docker ]] || return 0
  local bridge
  bridge="$(ce_bridge_of "$1" 2>/dev/null)" || return 0
  ce_as_root iptables -D FORWARD -i "${bridge}" -o "${bridge}" -j ACCEPT 2>/dev/null || true
}
