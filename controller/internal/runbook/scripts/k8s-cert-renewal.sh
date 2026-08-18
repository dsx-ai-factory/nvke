#!/bin/bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0


# Purpose: Renew expiring Kubernetes control-plane certs and safely restart static pods (apiserver, controller-manager, scheduler, etcd) with minimal downtime.
# Prerequisites:
# - Run on a control-plane node with sudo/root access
# - kubeadm installed and in $PATH
# - Control-plane components running as static pods
# - /var/log/ writable for logs

set -euo pipefail

LOG_FILE="/var/log/k8s-cert-renewal.log"

THRESHOLD_DAYS="${THRESHOLD_DAYS:-7}"
START_TIMEOUT="${START_TIMEOUT:-300}"
STOP_TIMEOUT="${STOP_TIMEOUT:-120}" 

MANIFESTS_DIR="/etc/kubernetes/manifests/"
TMP_DIR="/tmp/k8s_manifests_restart/"
CP_REGEX='kube-(apiserver|controller-manager|scheduler)|etcd'


log() {
  echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*" | tee -a "$LOG_FILE"
}

# Robust awk: pick $1 (cert) and $7 (residual), skip noise lines and CA table header
check_expiring_certs() {
  kubeadm certs check-expiration 2>/dev/null | awk -v threshold="$THRESHOLD_DAYS" '
    # Skip chatter and blank lines
    /^\[check-expiration\]/ || NF==0 { next }
    # Skip table headers
    /^CERTIFICATE[[:space:]]+EXPIRES/ { next }
    # Stop when the CA section begins
    /^CERTIFICATE AUTHORITY/ { exit }
    # Skip missing certs
    $1 == "!MISSING!" { next }

    {
      cert = $1
      residual = $7        # confirmed by your kubeadm version
      if (residual ~ /d$/) {
        days = substr(residual, 1, length(residual)-1)
        if (days+0 < threshold+0) { print cert " " days "d" }
      } else if (residual ~ /h$/) {
        print cert " 0d"
      } else if (residual == "<invalid>") {
        # Certificate is already expired
        print cert " EXPIRED"
      }
    }'
}

renew_certs() {
  log "Renewing certificates..."
  kubeadm certs renew all
}

# Return 0 if all CP containers are Running, else 1
cp_all_running() {
  # For each component, ensure at least one Running container exists
  local comps=("kube-apiserver" "kube-controller-manager" "kube-scheduler" "etcd")
  for c in "${comps[@]}"; do
    if ! crictl ps --state Running --name "$c" --quiet 2>/dev/null | grep -q .; then
      return 1
    fi
  done
  return 0
}

# Wait for all CP containers to terminate (after removing manifests)
wait_for_cp_terminated() {
  log "Waiting for static control-plane containers to terminate..."
  local deadline=$(( $(date +%s) + STOP_TIMEOUT ))
  while true; do
    if ! crictl ps --name "$CP_REGEX" --quiet 2>/dev/null | grep -q .; then
      log "All control-plane containers have terminated."
      break
    fi
    if (( $(date +%s) > deadline )); then
      log "ERROR: Timeout waiting for control-plane containers to terminate."
      exit 1
    fi
    sleep 3
  done
}

# Wait for all CP containers to be Running (after restoring manifests)
wait_for_cp_running() {
  log "Waiting for control-plane containers to be Running..."
  local deadline=$(( $(date +%s) + START_TIMEOUT ))
  while true; do
    if cp_all_running; then
      log "All control-plane components are Running."
      break
    fi
    if (( $(date +%s) > deadline )); then
      log "ERROR: Timeout waiting for control-plane components to be Running."
      # Helpful diagnostics
      crictl ps --name "$CP_REGEX" || true
      exit 1
    fi
    sleep 5
  done
}

# Pre/post health check using container runtime (works even if API server is down)
check_cp_health() {
  log "Checking control-plane component health..."
  if ! cp_all_running; then
    log "ERROR: One or more control-plane components are not Running."
    crictl ps --name "$CP_REGEX" || true
    exit 1
  fi
  log "All control-plane components are running."
}

restart_static_pods() {
  log "Restarting static pod components..."
  mkdir -p "$TMP_DIR"
  mv "$MANIFESTS_DIR"/* "$TMP_DIR"/

  wait_for_cp_terminated

  log "Restoring manifests..."
  mv "$TMP_DIR"/* "$MANIFESTS_DIR"/

  wait_for_cp_running
}

main() {
  log "=== Kubernetes Certificate Expiration Check Started ==="

  # Ensure CP is healthy before we touch anything
  check_cp_health

  EXPIRING=$(check_expiring_certs)
  if [[ -z "$EXPIRING" ]]; then
    log "All certificates are valid for more than $THRESHOLD_DAYS days."
    exit 0
  fi

  log "Certificates expiring soon:"
  echo "$EXPIRING" | tee -a "$LOG_FILE"

  renew_certs
  restart_static_pods

  # Final sanity
  check_cp_health
  log "Certificate renewal and static pod restart completed successfully!"
}

if [[ "${BASH_SOURCE[0]:-}" == "${0}" ]] || [[ -z "${BASH_SOURCE[0]:-}" ]]; then
    main "$@"
fi