#!/bin/bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Renew kubeadm-managed leaf certificates and restart the exact kubeadm static
# pods one at a time. A persistent phase file makes activity retries converge:
# once renewal succeeds, later attempts must finish the restart even though the
# renewed files are no longer inside the expiration threshold.

set -euo pipefail

LOG_FILE="${LOG_FILE:-/var/log/k8s-cert-renewal.log}"
THRESHOLD_DAYS="${THRESHOLD_DAYS:-7}"
EXPECTED_ETCD_MEMBERS="${EXPECTED_ETCD_MEMBERS:-}"
FORCE="${FORCE:-false}"
OPERATION_ID="${OPERATION_ID:-manual}"
STATUS_ONLY="${STATUS_ONLY:-false}"
# Four stop/start cycles can consume 28 minutes; bounded command and health
# checks add margin. A retry waits for the prior attempt to persist its outcome.
LOCK_TIMEOUT="${LOCK_TIMEOUT:-3600}"
START_TIMEOUT="${START_TIMEOUT:-300}"
STOP_TIMEOUT="${STOP_TIMEOUT:-120}"
POLL_INTERVAL="${POLL_INTERVAL:-3}"
KUBERNETES_DIR="${KUBERNETES_DIR:-/etc/kubernetes}"
KUBECONFIG="${KUBECONFIG:-${KUBERNETES_DIR}/admin.conf}"
PKI_DIR="${PKI_DIR:-${KUBERNETES_DIR}/pki}"
MANIFESTS_DIR="${MANIFESTS_DIR:-${KUBERNETES_DIR}/manifests}"
STATE_DIR="${STATE_DIR:-/var/lib/nke/k8s-cert-renewal}"
STAGING_DIR="${STAGING_DIR:-${KUBERNETES_DIR}/nke-cert-renewal-staging}"
STATE_FILE="${STATE_DIR}/state"
LOCK_FILE="${STATE_DIR}/lock"
RECEIPTS_DIR="${STATE_DIR}/completed"
RECEIPT_VERSION="terminal-receipt-v1"

COMPONENTS=("etcd" "kube-apiserver" "kube-controller-manager" "kube-scheduler")
CA_FILES=(
  "${PKI_DIR}/ca.crt"
  "${PKI_DIR}/etcd/ca.crt"
  "${PKI_DIR}/front-proxy-ca.crt"
)

STATE_OPERATION_ID=""
STATE_PHASE=""
STATE_COMPONENT=""
STATE_OUTCOME=""
STATE_CA_FINGERPRINTS=""
STATE_RECEIPT_VERSION=""
RECEIPT_OUTCOME=""
ACTIVE_COMPONENT=""

log() {
  echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*" | tee -a "$LOG_FILE"
}

die() {
  log "ERROR: $*" >&2
  exit 1
}

validate_input() {
  [[ "$THRESHOLD_DAYS" =~ ^[0-9]+$ ]] && (( THRESHOLD_DAYS > 0 )) || die "THRESHOLD_DAYS must be a positive integer."
  if [[ -n "$EXPECTED_ETCD_MEMBERS" ]]; then
    [[ "$EXPECTED_ETCD_MEMBERS" =~ ^[0-9]+$ ]] && (( EXPECTED_ETCD_MEMBERS > 0 )) || die "EXPECTED_ETCD_MEMBERS must be a positive integer."
  fi
  [[ "$LOCK_TIMEOUT" =~ ^[0-9]+$ ]] || die "LOCK_TIMEOUT must be a non-negative integer."
  [[ "$START_TIMEOUT" =~ ^[0-9]+$ ]] || die "START_TIMEOUT must be a non-negative integer."
  [[ "$STOP_TIMEOUT" =~ ^[0-9]+$ ]] || die "STOP_TIMEOUT must be a non-negative integer."
  [[ "$POLL_INTERVAL" =~ ^[0-9]+([.][0-9]+)?$ ]] || die "POLL_INTERVAL must be a non-negative number."
  [[ "$FORCE" == "true" || "$FORCE" == "false" ]] || die "FORCE must be true or false."
  [[ "$OPERATION_ID" =~ ^[A-Za-z0-9._:/-]+$ ]] || die "OPERATION_ID contains unsupported characters."
}

is_known_component() {
  local candidate="$1"
  local component
  [[ -z "$candidate" ]] && return 0
  for component in "${COMPONENTS[@]}"; do
    [[ "$candidate" == "$component" ]] && return 0
  done
  return 1
}

load_state() {
  STATE_OPERATION_ID=""
  STATE_PHASE=""
  STATE_COMPONENT=""
  STATE_OUTCOME=""
  STATE_CA_FINGERPRINTS=""
  STATE_RECEIPT_VERSION=""
  [[ -f "$STATE_FILE" ]] || return 0

  local -a state_lines
  mapfile -t state_lines < "$STATE_FILE"
  case "${#state_lines[@]}" in
    5) ;; # Old attempts do not honor receipts; resuming them must not enable fencing.
    6)
      [[ "${state_lines[5]}" == "$RECEIPT_VERSION" ]] || die "Certificate-renewal state has an unknown receipt version."
      STATE_RECEIPT_VERSION="${state_lines[5]}"
      ;;
    *) die "Certificate-renewal state file is corrupt." ;;
  esac
  STATE_OPERATION_ID="${state_lines[0]}"
  STATE_PHASE="${state_lines[1]}"
  STATE_COMPONENT="${state_lines[2]}"
  STATE_OUTCOME="${state_lines[3]}"
  STATE_CA_FINGERPRINTS="${state_lines[4]}"
  [[ "$STATE_OPERATION_ID" =~ ^[A-Za-z0-9._:/-]+$ ]] || die "Certificate-renewal state has an invalid operation ID."
  [[ "$STATE_CA_FINGERPRINTS" =~ ^[0-9a-f]{64}:[0-9a-f]{64}:[0-9a-f]{64}$ ]] || die "Certificate-renewal state has invalid CA fingerprints."
  is_known_component "$STATE_COMPONENT" || die "Certificate-renewal state has invalid component '$STATE_COMPONENT'."
  case "$STATE_PHASE" in
    renewing|restart_pending)
      [[ "$STATE_OUTCOME" == "renewed" ]] || die "In-progress certificate-renewal state has invalid outcome '$STATE_OUTCOME'."
      ;;
    complete)
      [[ "$STATE_OUTCOME" == "renewed" || "$STATE_OUTCOME" == "unchanged" ]] || die "Completed certificate-renewal state has invalid outcome '$STATE_OUTCOME'."
      [[ -z "$STATE_COMPONENT" ]] || die "Completed certificate-renewal state still names a component."
      ;;
    *) die "Certificate-renewal state has invalid phase '$STATE_PHASE'." ;;
  esac
}

write_state() {
  local operation_id="$1"
  local phase="$2"
  local component="$3"
  local outcome="$4"
  local ca_fingerprints="$5"
  local temporary
  temporary="$(mktemp "${STATE_FILE}.tmp.XXXXXX")"
  printf '%s\n%s\n%s\n%s\n%s\n' \
    "$operation_id" "$phase" "$component" "$outcome" "$ca_fingerprints" > "$temporary"
  if [[ -n "$STATE_RECEIPT_VERSION" ]]; then
    printf '%s\n' "$STATE_RECEIPT_VERSION" >> "$temporary"
  fi
  chmod 0600 "$temporary"
  mv -f "$temporary" "$STATE_FILE"
  STATE_OPERATION_ID="$operation_id"
  STATE_PHASE="$phase"
  STATE_COMPONENT="$component"
  STATE_OUTCOME="$outcome"
  STATE_CA_FINGERPRINTS="$ca_fingerprints"
}

terminal_receipt_file() {
  local digest
  digest="$(printf '%s' "$1" | sha256sum | awk '{print $1}')"
  printf '%s/%s\n' "$RECEIPTS_DIR" "$digest"
}

load_terminal_receipt() {
  local operation_id="$1"
  local receipt_file
  local -a receipt_lines
  RECEIPT_OUTCOME=""
  receipt_file="$(terminal_receipt_file "$operation_id")"
  if [[ ! -e "$receipt_file" && ! -L "$receipt_file" ]]; then
    return 1
  fi
  [[ -f "$receipt_file" && ! -L "$receipt_file" ]] || die "Certificate-renewal terminal receipt is not a regular file."
  mapfile -t receipt_lines < "$receipt_file"
  (( ${#receipt_lines[@]} == 3 )) || die "Certificate-renewal terminal receipt is corrupt."
  [[ "${receipt_lines[0]}" == "$RECEIPT_VERSION" ]] || die "Certificate-renewal terminal receipt has an unknown version."
  [[ "${receipt_lines[1]}" == "$operation_id" ]] || die "Certificate-renewal terminal receipt has a mismatched operation ID."
  [[ "${receipt_lines[2]}" == "renewed" || "${receipt_lines[2]}" == "unchanged" ]] || die "Certificate-renewal terminal receipt has an invalid outcome."
  # A writer may have exited after rename but before its final sync. Do not
  # acknowledge a fence until the receipt and its directory entry are durable.
  sync -f "$receipt_file" || die "Cannot persist certificate-renewal terminal receipt."
  RECEIPT_OUTCOME="${receipt_lines[2]}"
}

record_terminal_receipt() {
  # Only newly started V2 operations mark their phase file as receipt-aware.
  # Never backfill an old operation: a delayed old script could ignore receipts.
  [[ "$STATE_RECEIPT_VERSION" == "$RECEIPT_VERSION" ]] || return 0
  if load_terminal_receipt "$STATE_OPERATION_ID"; then
    [[ "$RECEIPT_OUTCOME" == "$STATE_OUTCOME" ]] || die "Certificate-renewal terminal receipt outcome changed."
    return 0
  fi
  local receipt_file temporary
  mkdir -p "$RECEIPTS_DIR"
  chmod 0700 "$RECEIPTS_DIR"
  receipt_file="$(terminal_receipt_file "$STATE_OPERATION_ID")"
  temporary="$(mktemp "${receipt_file}.tmp.XXXXXX")"
  printf '%s\n%s\n%s\n' "$RECEIPT_VERSION" "$STATE_OPERATION_ID" "$STATE_OUTCOME" > "$temporary"
  chmod 0600 "$temporary"
  sync -f "$temporary"
  mv "$temporary" "$receipt_file"
  sync -f "$RECEIPTS_DIR"
}

check_expiring_certs() {
  timeout 120 kubeadm certs check-expiration 2>/dev/null | awk -v threshold="$THRESHOLD_DAYS" '
    /^\[check-expiration\]/ || NF == 0 { next }
    /^CERTIFICATE[[:space:]]+EXPIRES/ { next }
    /^CERTIFICATE AUTHORITY/ { exit }
    $1 == "!MISSING!" { invalid = 1; next }
    {
      seen++
      residual = ""
      for (i = 2; i <= NF; i++) {
        if ($i ~ /^[0-9]+[dh]$/ || $i == "<invalid>") {
          residual = $i
          break
        }
      }
      if (residual == "") {
        invalid = 1
      } else if (residual ~ /d$/) {
        days = substr(residual, 1, length(residual) - 1)
        if (days + 0 < threshold + 0) { print $1 " " days "d" }
      } else if (residual ~ /h$/) {
        print $1 " 0d"
      } else if (residual == "<invalid>") {
        print $1 " EXPIRED"
      }
    }
    END {
      if (seen == 0 || invalid) { exit 2 }
    }'
}

ca_fingerprints() {
  local file
  for file in "${CA_FILES[@]}"; do
    [[ -f "$file" ]] || die "Required Kubernetes CA file '$file' is missing."
    sha256sum "$file" | awk '{print $1}'
  done | paste -sd: -
}

check_ca_fingerprints() {
  local expected="$1"
  local actual
  actual="$(ca_fingerprints)"
  [[ "$actual" == "$expected" ]] || die "A Kubernetes CA fingerprint changed during leaf-certificate renewal."
}

check_manifests() {
  local component
  for component in "${COMPONENTS[@]}"; do
    [[ -f "${MANIFESTS_DIR}/${component}.yaml" ]] || die "Required static-pod manifest '${component}.yaml' is missing."
  done
}

find_component_container() {
  local component="$1"
  local candidates
  local matches=()
  local container_id pod_namespace pod_name
  candidates="$(crictl ps --label "io.kubernetes.container.name=${component}" -q 2>/dev/null || true)"
  while read -r container_id; do
    [[ -n "$container_id" ]] || continue
    pod_namespace="$(crictl inspect "$container_id" 2>/dev/null | jq -r '.status.labels["io.kubernetes.pod.namespace"] // ""' || true)"
    pod_name="$(crictl inspect "$container_id" 2>/dev/null | jq -r '.status.labels["io.kubernetes.pod.name"] // ""' || true)"
    if [[ "$pod_namespace" == "kube-system" && "$pod_name" == "${component}-"* ]]; then
      matches+=("$container_id")
    fi
  done <<< "$candidates"

  if (( ${#matches[@]} == 1 )); then
    echo "${matches[0]}"
    return 0
  fi
  return 1
}

cp_all_running() {
  local component
  for component in "${COMPONENTS[@]}"; do
    find_component_container "$component" >/dev/null || return 1
  done
}

find_local_etcd_container() {
  find_component_container "etcd"
}

check_etcd_cluster_health() {
  local etcd_container_id member_list member_count voter_count quorum
  local -a etcdctl_command
  etcd_container_id="$(find_local_etcd_container || true)"
  [[ -n "$etcd_container_id" ]] || die "Cannot find exactly one running local kubeadm etcd container."
  etcdctl_command=(
    crictl exec "$etcd_container_id" etcdctl
    --endpoints=https://127.0.0.1:2379
    --cacert="${PKI_DIR}/etcd/ca.crt"
    --cert="${PKI_DIR}/etcd/server.crt"
    --key="${PKI_DIR}/etcd/server.key"
    --dial-timeout=10s
    --command-timeout=30s
  )
  member_list="$("${etcdctl_command[@]}" member list --write-out=json)" || die "Cannot read the etcd membership list."
  member_count="$(jq -er '.members | length' <<< "$member_list")" || die "Cannot parse the etcd membership list."
  voter_count="$(jq -er '[.members[] | select(.isLearner != true)] | length' <<< "$member_list")" || die "Cannot parse the etcd voter count."
  [[ "$member_count" =~ ^[0-9]+$ && "$voter_count" =~ ^[0-9]+$ ]] || die "The etcd membership count is invalid."
  (( voter_count == member_count )) || die "The etcd cluster contains a non-voting member and is unsafe for certificate renewal."
  if [[ -n "$EXPECTED_ETCD_MEMBERS" ]]; then
    (( member_count == EXPECTED_ETCD_MEMBERS )) || die "The etcd membership count ($member_count) does not match the control-plane snapshot ($EXPECTED_ETCD_MEMBERS)."
  fi
  # A single-voter cluster has no peer quorum to protect: renewing its only
  # etcd necessarily restarts it.
  quorum=$(( voter_count / 2 + 1 ))
  (( voter_count == 1 || voter_count - 1 >= quorum )) || die "The etcd cluster cannot tolerate one unavailable member."
  "${etcdctl_command[@]}" endpoint health --cluster >/dev/null || die "The etcd cluster is not healthy."
}

check_kube_api_health() {
  kubectl --kubeconfig "$KUBECONFIG" \
    --server https://localhost:6443 \
    --insecure-skip-tls-verify=true \
    --request-timeout=30s \
    get --raw '/readyz?verbose' >/dev/null || die "The local Kubernetes API is not ready."
}

# Isolate probe failures so die returns a nonzero status to startup polling
# instead of terminating the renewal transaction on transient unhealthiness.
check_local_health() (
  log "Checking local control-plane, Kubernetes API, and etcd cluster health..."
  cp_all_running || die "One or more local control-plane components are not Running."
  check_kube_api_health
  check_etcd_cluster_health
  log "Local control-plane and etcd health checks passed."
)

restore_active_manifest() {
  [[ -n "$ACTIVE_COMPONENT" ]] || return 0
  local manifest="${MANIFESTS_DIR}/${ACTIVE_COMPONENT}.yaml"
  local staged="${STAGING_DIR}/${ACTIVE_COMPONENT}.yaml"
  if [[ -f "$staged" && ! -e "$manifest" ]]; then
    mv "$staged" "$manifest" || log "CRITICAL: Failed to restore static-pod manifest '$ACTIVE_COMPONENT'." >&2
  fi
}

restore_stranded_manifest() {
  local component="$1"
  local manifest="${MANIFESTS_DIR}/${component}.yaml"
  local staged="${STAGING_DIR}/${component}.yaml"
  if [[ -e "$staged" && -e "$manifest" ]]; then
    die "Both staged and active manifests exist for '$component'."
  fi
  if [[ -e "$staged" ]]; then
    log "Restoring stranded manifest for '$component'."
    mv "$staged" "$manifest"
  fi
}

wait_for_component_stopped() {
  local component="$1"
  local old_container_id="$2"
  local deadline=$(( $(date +%s) + STOP_TIMEOUT ))
  local current
  while true; do
    current="$(find_component_container "$component" || true)"
    if [[ "$current" != "$old_container_id" ]]; then
      return 0
    fi
    (( $(date +%s) <= deadline )) || die "Timed out waiting for '$component' to stop."
    sleep "$POLL_INTERVAL"
  done
}

wait_for_component_started() {
  local component="$1"
  local old_container_id="$2"
  local deadline=$(( $(date +%s) + START_TIMEOUT ))
  local current
  while true; do
    current="$(find_component_container "$component" || true)"
    # A replacement container is not necessarily ready yet. Keep the complete
    # health condition inside the same startup deadline, with logs off stdout
    # because callers capture the container ID.
    if [[ -n "$current" && "$current" != "$old_container_id" ]] && check_local_health >&2; then
      echo "$current"
      return 0
    fi
    (( $(date +%s) <= deadline )) || die "Timed out waiting for '$component' to become ready."
    sleep "$POLL_INTERVAL"
  done
}

wait_for_component_running() {
  local component="$1"
  wait_for_component_started "$component" ""
}

next_component() {
  local current="$1"
  local index
  for index in "${!COMPONENTS[@]}"; do
    if [[ "${COMPONENTS[$index]}" == "$current" ]]; then
      if (( index + 1 < ${#COMPONENTS[@]} )); then
        echo "${COMPONENTS[$((index + 1))]}"
      fi
      return 0
    fi
  done
  return 1
}

restart_component() {
  local operation_id="$1"
  local component="$2"
  local outcome="$3"
  local original_ca_fingerprints="$4"
  local manifest="${MANIFESTS_DIR}/${component}.yaml"
  local staged="${STAGING_DIR}/${component}.yaml"
  local old_container_id

  restore_stranded_manifest "$component"
  old_container_id="$(wait_for_component_running "$component")"

  write_state "$operation_id" "restart_pending" "$component" "$outcome" "$original_ca_fingerprints"
  log "Restarting static pod '$component'..."
  ACTIVE_COMPONENT="$component"
  mv "$manifest" "$staged"
  wait_for_component_stopped "$component" "$old_container_id"
  mv "$staged" "$manifest"
  ACTIVE_COMPONENT=""
  wait_for_component_started "$component" "$old_container_id" >/dev/null
}

restart_static_pods() {
  local operation_id="$1"
  local first_component="$2"
  local outcome="$3"
  local original_ca_fingerprints="$4"
  local should_restart=false
  local component next

  for component in "${COMPONENTS[@]}"; do
    if [[ "$component" == "$first_component" ]]; then
      should_restart=true
    fi
    [[ "$should_restart" == "true" ]] || continue
    restart_component "$operation_id" "$component" "$outcome" "$original_ca_fingerprints"
    next="$(next_component "$component" || true)"
    write_state "$operation_id" "restart_pending" "$next" "$outcome" "$original_ca_fingerprints"
  done
}

renew_certs() {
  log "Certificate details before renewal:"
  timeout 120 kubeadm certs check-expiration | tee -a "$LOG_FILE"
  log "Renewing kubeadm-managed leaf certificates..."
  timeout 120 kubeadm certs renew all
  log "Certificate details after renewal:"
  timeout 120 kubeadm certs check-expiration | tee -a "$LOG_FILE"
}

finish_operation() {
  local operation_id="$1"
  local outcome="$2"
  local original_ca_fingerprints="$3"
  check_local_health
  check_ca_fingerprints "$original_ca_fingerprints"
  write_state "$operation_id" "complete" "" "$outcome" "$original_ca_fingerprints"
  record_terminal_receipt
  log "RENEWAL_OUTCOME=${outcome}"
}

resume_pending_operation() {
  if [[ "$STATE_PHASE" == "renewing" ]]; then
    log "Resuming certificate renewal operation '$STATE_OPERATION_ID' from renewal."
    check_ca_fingerprints "$STATE_CA_FINGERPRINTS"
    check_manifests
    check_local_health
    renew_certs
    write_state "$STATE_OPERATION_ID" "restart_pending" "${COMPONENTS[0]}" "renewed" "$STATE_CA_FINGERPRINTS"
  fi
  if [[ "$STATE_PHASE" == "restart_pending" ]]; then
    check_ca_fingerprints "$STATE_CA_FINGERPRINTS"
    if [[ -n "$STATE_COMPONENT" ]]; then
      log "Resuming certificate renewal operation '$STATE_OPERATION_ID' at static pod '$STATE_COMPONENT'."
      restart_static_pods "$STATE_OPERATION_ID" "$STATE_COMPONENT" "$STATE_OUTCOME" "$STATE_CA_FINGERPRINTS"
    fi
    finish_operation "$STATE_OPERATION_ID" "$STATE_OUTCOME" "$STATE_CA_FINGERPRINTS"
  fi
}

main() {
  [[ "$STATUS_ONLY" == "true" || "$STATUS_ONLY" == "false" ]] || die "STATUS_ONLY must be true or false."
  if [[ "$STATUS_ONLY" == "true" ]]; then
    validate_input
    # Use the same lock, held through the renewing process's EXIT restoration.
    # Never create state, install restoration traps, or enter resume/start logic.
    exec 9<"$LOCK_FILE"
    flock -w "$LOCK_TIMEOUT" 9 || die "Timed out waiting for certificate-renewal termination."
    load_terminal_receipt "$OPERATION_ID" || die "No matching terminal certificate-renewal receipt."
    echo "RENEWAL_TERMINAL_OPERATION_ID=${OPERATION_ID}"
    return 0
  fi

  mkdir -p "$STATE_DIR" "$STAGING_DIR" "$(dirname "$LOG_FILE")"
  touch "$LOG_FILE"
  chmod 0700 "$STATE_DIR" "$STAGING_DIR"
  exec 9>"$LOCK_FILE"
  flock -w "$LOCK_TIMEOUT" 9 || die "Timed out waiting for another certificate-renewal operation to finish."
  trap restore_active_manifest EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM

  validate_input
  # Delayed attempts must return their own original outcome before loading or
  # resuming another operation's phase file, even after that file was replaced.
  if load_terminal_receipt "$OPERATION_ID"; then
    log "Certificate-renewal operation '$OPERATION_ID' already completed."
    log "RENEWAL_OUTCOME=${RECEIPT_OUTCOME}"
    return 0
  fi
  load_state
  if [[ "$STATE_PHASE" == "renewing" || "$STATE_PHASE" == "restart_pending" ]]; then
    resume_pending_operation
    load_state
  fi
  if [[ "$STATE_OPERATION_ID" == "$OPERATION_ID" && "$STATE_PHASE" == "complete" ]]; then
    record_terminal_receipt
    log "Certificate-renewal operation '$OPERATION_ID' already completed."
    log "RENEWAL_OUTCOME=${STATE_OUTCOME}"
    return 0
  fi

  STATE_RECEIPT_VERSION=""
  if [[ -n "$EXPECTED_ETCD_MEMBERS" ]]; then
    STATE_RECEIPT_VERSION="$RECEIPT_VERSION"
  fi
  log "=== Kubernetes Certificate Expiration Check Started ==="
  check_manifests
  check_local_health

  local expiring=""
  if [[ "$FORCE" != "true" ]]; then
    if ! expiring="$(check_expiring_certs)"; then
      die "Unable to parse kubeadm certificate expiration output safely."
    fi
  fi
  if [[ "$FORCE" != "true" && -z "$expiring" ]]; then
    local fingerprints
    fingerprints="$(ca_fingerprints)"
    write_state "$OPERATION_ID" "complete" "" "unchanged" "$fingerprints"
    record_terminal_receipt
    log "All certificates are valid for at least $THRESHOLD_DAYS days."
    log "RENEWAL_OUTCOME=unchanged"
    return 0
  fi

  if [[ -n "$expiring" ]]; then
    log "Certificates expiring soon:"
    echo "$expiring" | tee -a "$LOG_FILE"
  else
    log "Forced renewal requested; renewing certificates outside the expiration threshold."
  fi

  local original_ca_fingerprints
  original_ca_fingerprints="$(ca_fingerprints)"
  write_state "$OPERATION_ID" "renewing" "" "renewed" "$original_ca_fingerprints"
  renew_certs
  write_state "$OPERATION_ID" "restart_pending" "${COMPONENTS[0]}" "renewed" "$original_ca_fingerprints"
  restart_static_pods "$OPERATION_ID" "${COMPONENTS[0]}" "renewed" "$original_ca_fingerprints"
  finish_operation "$OPERATION_ID" "renewed" "$original_ca_fingerprints"
  log "Certificate renewal and sequential static-pod restart completed successfully."
}

main "$@"
