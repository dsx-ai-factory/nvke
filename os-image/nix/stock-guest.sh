#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
# Runs inside nixpkgs' stock Linux builder as its unprivileged `builder` user,
# started from the serial console by build-packages-stock.sh, and powers the VM
# off when done. Everything goes through the VM's /tmp/xchg share. Each file
# this writes there is new and named with the run id: a file the guest creates
# on the share belongs to the host user, so the guest cannot reopen it.
set -uo pipefail
id=${1:?usage: stock-guest.sh RUN_ID}
xchg=${NKE_XCHG:-/tmp/xchg}
: >"$xchg/started-$id"
# The subshell's status is read on the next line rather than with ||, which
# would switch off its set -e.
(
  set -euo pipefail
  env_file=$xchg/env-$id.sh
  # shellcheck disable=SC1090
  . "$env_file"
  # It can hold credentials such as NGC_API_KEY.
  rm -f -- "$env_file"
  work=$HOME/nvke-build
  rm -rf -- "$work"
  mkdir -p "$work/src" "$work/artifacts"
  tar -xf "$xchg/src-$id.tar" -C "$work/src"
  # The guest has no podman module; configure rootless podman as the package
  # VM's module does: Docker Hub for short names, NixOS's default policy.
  mkdir -p "$HOME/.config/containers"
  printf 'unqualified-search-registries = ["docker.io"]\n' >"$HOME/.config/containers/registries.conf"
  printf '{"default":[{"type":"insecureAcceptAnything"}]}\n' >"$HOME/.config/containers/policy.json"
  export REPO_ROOT=$work/src ARTIFACTS_DIR=$work/artifacts
  # GNU tar keeps archive modes by default only for root, and the package VM
  # builds as root; cni-ovn's image takes its file modes from an unpacked tree.
  export TAR_OPTIONS=--same-permissions
  # shellcheck disable=SC2016  # expanded inside the devShell
  nix --extra-experimental-features 'nix-command flakes' develop "$work/src/os-image#package-build" \
    --command bash -c 'NKE_UBUNTU_CC=$(command -v nke-ubuntu-cc) exec bash "$REPO_ROOT/os-image/nix/build-guest.sh"'
  tar -cf "$xchg/out-$id.tar" -C "$work/artifacts" .
) >"$xchg/log-$id" 2>&1
echo "$?" >"$xchg/status-$id"
systemctl poweroff
