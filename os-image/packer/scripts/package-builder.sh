#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
# Packer adapter: native package targets or the Nix-owned build-packages app.
set -euo pipefail
arch=${ARCH:-amd64}
case "$arch" in amd64|arm64) ;; *) echo 'ARCH must be amd64 or arm64' >&2; exit 1;; esac
repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
native=0
case "$(uname -s)/$(uname -m)/$arch" in
  Linux/x86_64/amd64|Linux/aarch64/arm64) native=1 ;;
esac
# A host build links the agent against the builder's glibc, and its .deb declares
# a bare `Depends: libc6`, so a host newer than the target Ubuntu installs
# cleanly and then fails to exec. Only newer is a problem. Unknown on either
# side keeps the previous behaviour.
series_num() { local v=${1%%.*} r=${1#*.}; printf '%d' $(( 10#$v * 100 + 10#${r%%.*} )); }
target_series=${NKE_TARGET_UBUNTU_SERIES:-}
host_series=${NKE_HOST_UBUNTU_SERIES:-}
if [[ -z "$host_series" && -r /etc/os-release ]]; then
  host_series=$(. /etc/os-release 2>/dev/null; [[ "${ID:-}" == ubuntu ]] && echo "${VERSION_ID:-}")
fi
host_too_new=0
if [[ -n "$target_series" && -n "$host_series" ]] &&
   (( $(series_num "$host_series") > $(series_num "$target_series") )); then
  host_too_new=1
fi

mode=${NKE_PACKAGE_BUILDER:-auto}
if [[ "$mode" == auto ]]; then
  if [[ "$native" == 1 && "$host_too_new" == 0 ]]; then mode=host; else mode=nix; fi
fi
case "$mode" in
  host)
    [[ "$native" == 1 ]] || { echo 'host package builds require a matching Linux host' >&2; exit 1; }
    [[ "$host_too_new" == 0 ]] || {
      echo "this Ubuntu $host_series host cannot build packages for an Ubuntu $target_series image:" >&2
      echo "  the agent would link against a glibc the image does not have and fail to exec." >&2
      echo "  Use NKE_PACKAGE_BUILDER=nix, or build on Ubuntu $target_series or older." >&2
      exit 1
    }
    command -v go >/dev/null || { echo 'go is required for host package builds' >&2; exit 1; }
    ;;
  nix)
    # The flake builds VMs for these hosts only; see os-image/flake.nix.
    case "$(uname -s)/$(uname -m)" in
      Darwin/arm64|Linux/x86_64|Linux/aarch64) ;;
      *) echo "VM package builds are not supported on $(uname -s)/$(uname -m)" >&2; exit 1 ;;
    esac
    command -v nix >/dev/null || {
      echo 'nix is required for VM package builds.' >&2
      echo 'Install Nix and enable nix-command and flakes; see os-image/nix/README.md.' >&2
      exit 1
    }
    ;;
  *) echo 'NKE_PACKAGE_BUILDER must be auto, host, or nix' >&2; exit 1 ;;
esac
echo "package builder: $mode ($arch${target_series:+, target ubuntu $target_series})"
if [[ "${1:-}" == --check ]]; then
  if [[ "$mode" == nix ]]; then
    # Realize the guest runner now: without a matching Linux builder or a cache
    # hit this fails here rather than part-way through an image bake.
    echo "realizing the builder VM runner; this may download the guest closure"
    nix build --no-link "$repo_root/os-image#builder-$arch"
  fi
  exit 0
fi
if [[ "$mode" == nix ]]; then
  exec nix run "$repo_root/os-image#build-packages-$arch" -- \
    --source "$repo_root" --output "${ARTIFACTS_DIR:?ARTIFACTS_DIR is required}"
fi
export NKE_BUILD_IN_PLACE=1
exec bash "$repo_root/os-image/packer/scripts/fetch-artifacts.sh"
