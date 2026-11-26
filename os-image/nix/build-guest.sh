#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail
# CC is explicit so the build does not depend on the guest's shell init. The
# package VM installs the compiler system-wide; stock-guest.sh, which runs this
# from a devShell, passes the shell's in NKE_UBUNTU_CC.
# OCI_BIN: both guests build cni-ovn's image with podman, not docker.
export SOURCE_ROOT="$REPO_ROOT" NKE_BUILD_IN_PLACE=1 \
  CC=${NKE_UBUNTU_CC:-/run/current-system/sw/bin/nke-ubuntu-cc} OCI_BIN=podman
for package in kubeadm-distro cni-calico cni-ovn spire-agent doca-ovs; do
  # /root/.cache in the package VM; the build user's own cache in the stock builder.
  cache=$HOME/.cache/nke-packages/$package
  for directory in bin dist; do
    mkdir -p "$cache/$directory"
    ln -sfn "$cache/$directory" "$REPO_ROOT/os-image/packages/$package/$directory"
  done
done
bash "$REPO_ROOT/os-image/packer/scripts/fetch-artifacts.sh"
check-ubuntu-elf --arch "$ARCH" \
  "$REPO_ROOT/agent/bin/linux_$ARCH/nvke-agent"
updater=$REPO_ROOT/agent/bin/linux_$ARCH/nvke-agent-updater
[[ -f "$updater" ]] ||
  { echo "FAIL: $updater: expected updater binary" >&2; exit 1; }
headers=$(readelf -lW "$updater")
[[ "$headers" != *INTERP* ]] ||
  { echo "FAIL: $updater: expected a static binary, found a program interpreter" >&2; exit 1; }
echo "PASS: $updater: static, no interpreter"
