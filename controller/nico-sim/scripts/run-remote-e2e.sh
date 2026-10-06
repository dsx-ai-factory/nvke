#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Run libvirt e2e tests on a remote Linux machine via SSH or Teleport.
#
# This script syncs code to the remote machine, then runs Go integration
# tests there. Output is streamed to the terminal AND saved to a timestamped
# log file in ./logs/ for later inspection.
#
# The remote machine must have been set up with scripts/setup-linux.sh
# beforehand (bridge, dnsmasq, KVM, OVMF, etc.).
#
# IMPORTANT: Remote machines may have csh/tcsh as default shell. This script
# always wraps commands with `bash -s` to avoid shell incompatibilities.
#
# Usage: ./scripts/run-remote-e2e.sh [--ssh|--tsh] <host> [options]
#
# Transport:
#   --ssh              Force SSH transport
#   --tsh              Force Teleport transport
#   (default: auto-detects — uses Teleport if `tsh` is available and logged in)
#
# Test selection:
#   --run <regex>      Go test -run filter (default: runs all libvirt tests)
#   --timeout <dur>    Go test timeout (default: 30m, or 180m for --batch)
#   --batch            Run TestBatchProductionCloudInit instead of standard tests
#   --batch-count <n>  Number of VMs for batch test (default: 5)
#
# Environment:
#   --production       Set NICO_VMS_TEST_PRODUCTION_IMAGE=true
#   --preserve-logs    Set NICO_VMS_PRESERVE_LOGS=true (saves to /tmp/nico-vms-preserved/)
#   --keep-vms         Set NICO_VMS_E2E_TESTS_KEEP_VMS=true (don't delete VMs after test)
#   --cleanup-vms      Set NICO_VMS_CLEANUP_ORPHANED_VMS=true (destroy orphaned VMs on startup)
#   --image-url <url>  Set NICO_VMS_PROD_IMAGE_URL (filename or full URL for custom image)
#   --files-dir <dir>  Set NICO_SIM_FILES_DIR (directory for the file server root, remote path)
#   --vm-memory <MiB>  Set NICO_VMS_VM_MEMORY_MIB (default: 2048)
#   --vm-disk <GiB>    Set NICO_VMS_VM_DISK_GIB (default: 20)
#   --workers <n>      Set NICO_VMS_RECONCILE_WORKERS (default: 4)
#
# Sync:
#   --skip-sync        Skip rsync step (useful when code is already up to date)
#   NICO_SIM_REMOTE_DIR=<dir>  Remote nico-sim directory
#                      (default: ~/dev/nke/controller/nico-sim)
#
# Examples:
#   # Run all libvirt tests on SSH machine
#   ./scripts/run-remote-e2e.sh my-machine.example.com
#
#   # Run a specific test via Teleport
#   ./scripts/run-remote-e2e.sh --tsh some-host --run TestSSHVerification
#
#   # Run production image test with log preservation
#   ./scripts/run-remote-e2e.sh --ssh my-machine.example.com \
#     --production --preserve-logs --run TestIPXEProductionImageFlow
#
#   # Run batch test with 10 VMs
#   ./scripts/run-remote-e2e.sh --tsh some-host --batch --batch-count 10
#
#   # Re-run without syncing (code already up to date)
#   ./scripts/run-remote-e2e.sh --ssh my-machine.example.com --skip-sync --run TestSSHVerification

set -euo pipefail

# --- Parse arguments ---
TRANSPORT=""
HOST=""
RUN_FILTER=""
TIMEOUT=""
PRODUCTION=false
PRESERVE_LOGS=false
KEEP_VMS=false
CLEANUP_VMS=false
SKIP_SYNC=false
BATCH=false
BATCH_COUNT=5
IMAGE_URL=""
FILES_DIR=""
VM_MEMORY=""
VM_DISK=""
WORKERS=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --ssh)           TRANSPORT="ssh"; shift ;;
    --tsh)           TRANSPORT="tsh ssh"; shift ;;
    --run)           RUN_FILTER="$2"; shift 2 ;;
    --timeout)       TIMEOUT="$2"; shift 2 ;;
    --production)    PRODUCTION=true; shift ;;
    --preserve-logs) PRESERVE_LOGS=true; shift ;;
    --keep-vms)      KEEP_VMS=true; shift ;;
    --cleanup-vms)   CLEANUP_VMS=true; shift ;;
    --skip-sync)     SKIP_SYNC=true; shift ;;
    --batch)         BATCH=true; shift ;;
    --batch-count)   BATCH_COUNT="$2"; shift 2 ;;
    --image-url)     IMAGE_URL="$2"; shift 2 ;;
    --files-dir)     FILES_DIR="$2"; shift 2 ;;
    --vm-memory)     VM_MEMORY="$2"; shift 2 ;;
    --vm-disk)       VM_DISK="$2"; shift 2 ;;
    --workers)       WORKERS="$2"; shift 2 ;;
    --help|-h)
      # Print the comment block at the top of this file as help text
      sed -n '2,/^[^#]/{ /^#/s/^# \{0,1\}//p; }' "$0"
      exit 0
      ;;
    -*)
      echo "Error: unknown option: $1" >&2
      echo "Run '$0 --help' for usage" >&2
      exit 1
      ;;
    *)
      HOST="$1"; shift ;;
  esac
done

if [[ -z "$HOST" ]]; then
  echo "Error: host is required" >&2
  echo "Usage: $0 [--ssh|--tsh] <host> [options]" >&2
  echo "Run '$0 --help' for full usage" >&2
  exit 1
fi

# --- Auto-detect transport ---
if [[ -z "$TRANSPORT" ]]; then
  if command -v tsh >/dev/null 2>&1 && tsh status >/dev/null 2>&1; then
    TRANSPORT="tsh ssh"
  else
    TRANSPORT="ssh"
  fi
fi

# --- Default timeout depends on test type ---
if [[ -z "$TIMEOUT" ]]; then
  if [[ "$BATCH" == true ]]; then
    TIMEOUT="180m"
  else
    TIMEOUT="30m"
  fi
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
REMOTE_DIR="${NICO_SIM_REMOTE_DIR:-~/dev/nke/controller/nico-sim}"

# --- Log file setup ---
LOG_DIR="$REPO_ROOT/logs"
mkdir -p "$LOG_DIR"
TIMESTAMP=$(date +%Y%m%d-%H%M%S)
LOG_FILE="$LOG_DIR/e2e-${TIMESTAMP}.log"

echo "=== nico-vms remote e2e test runner ==="
echo "Host:      $HOST"
echo "Transport: $TRANSPORT"
echo "Timeout:   $TIMEOUT"
echo "Log file:  $LOG_FILE"
[[ -n "$RUN_FILTER" ]] && echo "Filter:    $RUN_FILTER"
[[ "$BATCH" == true ]] && echo "Batch:     $BATCH_COUNT VMs"
[[ -n "$IMAGE_URL" ]] && echo "Image URL: $IMAGE_URL"
[[ -n "$VM_MEMORY" ]] && echo "VM Memory: ${VM_MEMORY} MiB"
[[ -n "$VM_DISK" ]] && echo "VM Disk:   ${VM_DISK} GiB"
echo ""

# --- Sync code to remote ---
if [[ "$SKIP_SYNC" == false ]]; then
  echo "--- Syncing code to $HOST ---"

  RSYNC_EXCLUDES=(
    --exclude='.git'
    --exclude='.cache'
    --exclude='.gocache'
    --exclude='.gomodcache'
    --exclude='var'
    --exclude='images'
    --exclude='bin'
    --exclude='logs'
  )

  # Do not sync a locally generated dependency bundle to the remote host.
  RSYNC_EXCLUDES+=(--exclude='vendor')

  # shellcheck disable=SC2086
  rsync -avz -e "$TRANSPORT" \
    "${RSYNC_EXCLUDES[@]}" \
    "$REPO_ROOT/" \
    "$HOST:$REMOTE_DIR"

  echo ""
  echo "Sync complete."
  echo ""
fi

# --- Build remote command ---
# We build the full command as a string, then pipe it to `bash -s` on the
# remote machine. This avoids csh/tcsh quoting issues on machines where the
# default shell is not bash.

# PATH: remote machines may not have Go in the default PATH
REMOTE_CMD="export PATH=\$PATH:/usr/local/go/bin:\$HOME/go/bin"

# DHCP env vars required for iPXE/DHCP-enabled tests
REMOTE_CMD+=" && export NICO_VMS_DHCP_HOSTS_DIR=/var/lib/nico-vms/dhcp-hosts"
REMOTE_CMD+=" && export NICO_VMS_DHCP_LEASE_FILE=/var/run/nico-vms-dnsmasq.leases"

if [[ "$PRODUCTION" == true ]]; then
  REMOTE_CMD+=" && export NICO_VMS_TEST_PRODUCTION_IMAGE=true"
  # Production FDE images need longer timeouts: LUKS TPM firstboot + cloud-init
  # runcmd (kubeadm bundle download + install) can take 5-10 minutes on KVM.
  REMOTE_CMD+=" && export NICO_VMS_READINESS_TIMEOUT=15m"
  REMOTE_CMD+=" && export NICO_VMS_TEST_PROD_IMAGE_TIMEOUT=12m"
fi

if [[ "$PRESERVE_LOGS" == true ]]; then
  REMOTE_CMD+=" && export NICO_VMS_PRESERVE_LOGS=true"
fi

if [[ "$KEEP_VMS" == true ]]; then
  REMOTE_CMD+=" && export NICO_VMS_E2E_TESTS_KEEP_VMS=true"
fi

if [[ "$CLEANUP_VMS" == true ]]; then
  REMOTE_CMD+=" && export NICO_VMS_CLEANUP_ORPHANED_VMS=true"
fi

if [[ -n "$IMAGE_URL" ]]; then
  REMOTE_CMD+=" && export NICO_VMS_PROD_IMAGE_URL='${IMAGE_URL}'"
fi

if [[ -n "$FILES_DIR" ]]; then
  REMOTE_CMD+=" && export NICO_SIM_FILES_DIR='${FILES_DIR}'"
fi

if [[ -n "$VM_MEMORY" ]]; then
  REMOTE_CMD+=" && export NICO_VMS_VM_MEMORY_MIB=${VM_MEMORY}"
fi

if [[ -n "$VM_DISK" ]]; then
  REMOTE_CMD+=" && export NICO_VMS_VM_DISK_GIB=${VM_DISK}"
fi

if [[ -n "$WORKERS" ]]; then
  REMOTE_CMD+=" && export NICO_VMS_RECONCILE_WORKERS=${WORKERS}"
fi

# Force module mode even if an earlier run left vendor/ on the remote host.
# The remote host must be able to download public Go modules on a cold cache.
# -mod=mod is rejected in workspace mode, and a full nke checkout on the remote
# puts nico-sim under the repository go.work, so test nico-sim as its own module.
REMOTE_CMD+=" && export GOWORK=off"
TEST_CMD="go test -mod=mod -tags=integration -count=1 -timeout ${TIMEOUT} -v ./test/integration/libvirt"

if [[ "$BATCH" == true ]]; then
  TEST_CMD+=" -run TestBatchProductionCloudInit"
  REMOTE_CMD+=" && export NICO_VMS_BATCH_COUNT=${BATCH_COUNT}"
  REMOTE_CMD+=" && export NICO_VMS_RECONCILE_WORKERS=8"
  REMOTE_CMD+=" && export NICO_VMS_TEST_PRODUCTION_IMAGE=true"
  REMOTE_CMD+=" && export NICO_VMS_PRESERVE_LOGS=true"
elif [[ -n "$RUN_FILTER" ]]; then
  TEST_CMD+=" -run '${RUN_FILTER}'"
fi

REMOTE_CMD+=" && cd $REMOTE_DIR && ${TEST_CMD}"

echo "--- Running tests on $HOST ---"
echo "Test command: $TEST_CMD"
echo ""

# --- Execute on remote ---
# Disable errexit for the pipeline so we can capture the exit code.
# Output goes to both terminal (stdout) and the log file via tee.
set +e
echo "$REMOTE_CMD" | $TRANSPORT "$HOST" 'bash -s' 2>&1 | tee "$LOG_FILE"
EXIT_CODE=${PIPESTATUS[1]}
set -e

# Strip ANSI color/escape codes from log file (colors show on terminal via tee,
# but are not readable in files)
perl -i -pe 's/\e\[[0-9;]*[a-zA-Z]//g' "$LOG_FILE"

echo ""
echo "=== Test run complete ==="
echo "Exit code: $EXIT_CODE"
echo "Log file:  $LOG_FILE"

exit "$EXIT_CODE"
