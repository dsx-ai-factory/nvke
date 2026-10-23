#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
mode="${1:-test}"
[[ "$(uname -s)" == Darwin ]] || { echo 'local-mac.sh requires macOS' >&2; exit 1; }
for tool in go qemu-img; do
 command -v "$tool" >/dev/null || { echo "Missing $tool; see controller/nico-sim/README.md macOS setup" >&2; exit 1; }
done
if ! command -v mkisofs >/dev/null && ! command -v genisoimage >/dev/null && ! command -v cloud-localds >/dev/null; then
 echo 'Missing ISO builder: install cdrtools or cloud-utils (see README.md)' >&2; exit 1
fi
: "${IMAGE:?Set IMAGE to the absolute path of the ARM64 non-FDE qcow2}"
[[ -f "$IMAGE" ]] || { echo "Image does not exist: $IMAGE" >&2; exit 1; }
IMAGE="$(cd "$(dirname "$IMAGE")" && pwd)/$(basename "$IMAGE")"
arch="${ARCH:-$(uname -m)}"
case "$arch" in arm64|aarch64) arch=arm64; qemu=qemu-system-aarch64;; amd64|x86_64) arch=amd64; qemu=qemu-system-x86_64;; *) echo "Unsupported ARCH: $arch" >&2; exit 1;; esac
case "$(uname -m):$arch" in arm64:arm64|x86_64:amd64) ;; *) echo 'HVF requires a native-architecture image' >&2; exit 1;; esac
export NICO_VMS_QEMU_SYSTEM_PATH="${NICO_VMS_QEMU_SYSTEM_PATH:-$qemu}"
command -v "$NICO_VMS_QEMU_SYSTEM_PATH" >/dev/null || { echo "Missing $NICO_VMS_QEMU_SYSTEM_PATH" >&2; exit 1; }
export NICO_VMS_BASE_IMAGE="$IMAGE" NICO_VMS_VM_BACKEND=darwin-qemu
export NICO_VMS_BGP_ENABLED=false NICO_CONTAINERS_ENABLED=false
export NICO_VMS_CLEANUP_ORPHANED_VMS=false
cd "$root/controller/nico-sim"
case "$mode" in
 up)
  # Each checkout has its own database and VM overlays. No global process cleanup.
  export NICO_SIM_DB_PATH="${NICO_SIM_DB_PATH:-$root/controller/nico-sim/.dev/mac/nico-sim.db}"
  export NICO_SIM_LISTEN_ADDR="${NICO_SIM_LISTEN_ADDR:-127.0.0.1:18080}"
  make build
  exec ./bin/nico-sim
  ;;
 test)
  export NICO_VMS_FLOW_TIMEOUT="${NICO_VMS_FLOW_TIMEOUT:-8m}"
  exec go test -tags=integration -count=1 -parallel=1 -timeout=20m \
    -run 'TestCreatePollReadyAndDeleteFlow|TestPatchLabelsAndRebootFlow|TestVMToVMConnectivity|TestSSHVerification' \
    -v ./test/integration/qemu
  ;;
 test-cluster)
  export NICO_VMS_TEST_NVKE_IMAGE=true
  export NICO_VMS_FLOW_TIMEOUT="${NICO_VMS_FLOW_TIMEOUT:-8m}"
  exec go test -tags=integration -count=1 -parallel=1 -timeout=25m -run '^TestNVKEImageCluster$' -v ./test/integration/qemu
  ;;
 *) echo "Unknown mode: $mode (expected up, test or test-cluster)" >&2; exit 1;;
esac
