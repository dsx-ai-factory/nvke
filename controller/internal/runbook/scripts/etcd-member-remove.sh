#!/bin/bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# etcd-member-remove.sh
#
# Requires bash 4. This script uses an associative array to hold per-member
# health, and bash 3.2 does not reject `declare -A`: it warns on stderr, falls
# back to an indexed array, resolves every non-numeric subscript to 0 and exits
# 0. Every member would then report the last member's health, in the script that
# decides whether removing one breaks quorum. The controller runs this on Linux
# control-plane nodes with bash 5, so the guard below is for anyone testing it
# on a Mac, where /bin/bash is 3.2. shellcheck cannot catch this at any
# severity.
#
# Safely remove an etcd member with quorum safeguards.
# - Blocks removals that would drop the cluster below quorum (unless --allow-quorum-loss).
# - Blocks removals that would leave a single-voter cluster (unless --allow-single-voter).
# - Supports --dry-run to preview safety.
# - Parses etcdctl JSON output (requires jq).
#
# Must be run on a control-plane node that can exec into the local etcd pod.
#
# Usage:
#   ./etcd-member-remove.sh [--dry-run] [--allow-quorum-loss] [--allow-single-voter] <member_name>
#
# Examples:
#   ./etcd-member-remove.sh node-1.example.com
#   ./etcd-member-remove.sh --dry-run node-1.example.com
#   ./etcd-member-remove.sh --allow-single-voter node-1.example.com
#   ./etcd-member-remove.sh --allow-quorum-loss --allow-single-voter node-1.example.com
#
# In dry-run mode, exit codes are:
# 0: safe to remove
# 1: input error (e.g. no match)
# 2: removal would break quorum
# 3: removal would leave single-voter cluster
#
# In normal mode, exit codes are:
# 0: success
# 1: general error (e.g. missing args, cannot access etcd)
# 2: removal would break quorum (without --allow-quorum-loss)
# 3: removal would leave single-voter cluster (without --allow-single-voter)

set -euo pipefail


#-------------------------------------
# Utilities
#--------------------------------------
die() { echo "ERROR: $*" >&2; exit 1; }
warn() { echo "WARNING: $*" >&2; }
info() { echo "[*] $*"; }

#--------------------------------------
# Parse flags/args
#--------------------------------------
DRY_RUN=0
ALLOW_QUORUM_LOSS=0
ALLOW_SINGLE_VOTER=0
TARGET_NAME=""

export KUBECONFIG="${KUBECONFIG:-/etc/kubernetes/admin.conf}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run) DRY_RUN=1; shift ;;
    --allow-quorum-loss) ALLOW_QUORUM_LOSS=1; shift ;;
    --allow-single-voter) ALLOW_SINGLE_VOTER=1; shift ;;
    -h|--help)
      cat <<EOF
Usage: $0 [--dry-run] [--allow-quorum-loss] [--allow-single-voter] <member_name|member_id>

Safely remove an etcd voting member with quorum checks.

Flags:
  --dry-run              Preview the removal without executing it
  --allow-quorum-loss    Allow removal even if it would cause quorum loss
  --allow-single-voter   Allow removal that leaves only one voter (no HA)
EOF
      exit 0
      ;;
    *) TARGET_NAME="${1}"; shift ;;
  esac
done

[[ -n "${TARGET_NAME}" ]] || die "Missing <member_name>. Use -h for help."
[[ -f "${KUBECONFIG}" ]] || die "KUBECONFIG file not found at '${KUBECONFIG}'."

find_local_kubeadm_etcd_container() {
  local candidates
  candidates="$(crictl ps --label io.kubernetes.container.name=etcd -q 2>/dev/null || true)"
  [[ -n "${candidates}" ]] || return 1

  local etcd_ids=()
  local cid pod_namespace pod_name
  while read -r cid; do
    [[ -n "${cid}" ]] || continue
    pod_namespace="$(crictl inspect "${cid}" 2>/dev/null | jq -r '.status.labels["io.kubernetes.pod.namespace"] // ""')"
    pod_name="$(crictl inspect "${cid}" 2>/dev/null | jq -r '.status.labels["io.kubernetes.pod.name"] // ""')"

    if [[ "${pod_namespace}" == "kube-system" && "${pod_name}" == etcd-* ]]; then
      etcd_ids+=("${cid}")
    fi
  done <<< "${candidates}"

  if [[ "${#etcd_ids[@]}" -eq 1 ]]; then
    echo "${etcd_ids[0]}"
    return 0
  fi

  return 1
}

etcdctl_exec() {
  local ETCD_CONTAINER_ID
  local ENDPOINT="https://127.0.0.1:2379"
  
  # Check if first argument is --endpoint
  if [[ "$1" == "--endpoint" ]]; then
    ENDPOINT="$2"
    shift 2
  fi

  # Find the local kubeadm static-pod etcd container. Do not use only the
  # generic container-name label; unrelated etcd workloads can share it.
  ETCD_CONTAINER_ID="$(find_local_kubeadm_etcd_container || true)"

  if [[ -z "${ETCD_CONTAINER_ID}" ]]; then
    die "Cannot find exactly one local kube-system/etcd-* container. Ensure you are on a control-plane node with kubeadm etcd running."
  fi

  crictl exec "${ETCD_CONTAINER_ID}" etcdctl \
    --endpoints="${ENDPOINT}" \
    --cacert=/etc/kubernetes/pki/etcd/ca.crt \
    --cert=/etc/kubernetes/pki/etcd/server.crt \
    --key=/etc/kubernetes/pki/etcd/server.key \
    "$@"
}

info "Checking etcd access…"
if ! etcdctl_exec endpoint health &>/dev/null; then
  die "Cannot access etcd via local pod. Ensure you are on a control-plane node with etcd."
fi

info "Fetching current etcd cluster members…"
MEMBERS_LIST="$(etcdctl_exec member list)"

# Build voter list from default output to preserve hex IDs: "<ID-hex> <name> <clientURL>"
# Default format is CSV-like:
# 1d2ea122926a3cc7, started, estaples-dev-cp2-v2, https://10.45.144.79:2380, https://10.45.144.79:2379, false

VOTERS=()
voter_lines=$(echo "$MEMBERS_LIST" | awk -F', ' '$6 == "false" {print $1, $3, $5}')
while read -r line; do
  [[ -n "$line" ]] && VOTERS+=("$line")
done <<< "$voter_lines"

(( ${#VOTERS[@]} > 0 )) || die "No voting members found."

info "Current voting members:"
printf '  %s\n' "${VOTERS[@]}"

# Find target member by name
TARGET_LINE=""
for line in "${VOTERS[@]}"; do
  member_id="$(echo "$line" | awk '{print $1}')"
  member_name="$(echo "$line" | awk '{print $2}')"
  if [[ "${member_name}" == "${TARGET_NAME}" || "${member_id}" == "${TARGET_NAME}" ]]; then
    TARGET_LINE="${line}"
    break
  fi
done

[[ -n "${TARGET_LINE}" ]] || {
  warn "No voting member matched '${TARGET_NAME}'. Nothing to do."
  exit 0
}

TARGET_ID="$(echo "$TARGET_LINE" | awk '{print $1}')"
TARGET_NAME="$(echo "$TARGET_LINE" | awk '{print $2}')"
TARGET_URL="$(echo "$TARGET_LINE" | awk '{print $3}')"

info "Target member: ${TARGET_NAME} (ID: ${TARGET_ID}) URL: ${TARGET_URL:-<none>}"

# Health assessment - do this BEFORE leader transfer so we can check member health
info "Assessing cluster health…"
HEALTH_JSON="$(etcdctl_exec endpoint health --cluster -w json || true)"

# Get healthy endpoints set
HEALTHY_EPS=()
healthy_endpoints=$(echo "${HEALTH_JSON}" | jq -r '.[] | select(.health==true) | .endpoint')
while read -r endpoint; do
  HEALTHY_EPS+=("$endpoint")
done <<< "$healthy_endpoints"

if [ "${BASH_VERSINFO[0]}" -lt 4 ]; then
  echo "ERROR: this script needs bash 4 or newer; found ${BASH_VERSION}." >&2
  echo "       On macOS /bin/bash is 3.2. Run it with a newer bash." >&2
  exit 1
fi

declare -A MEMBER_HEALTH=()  # ID -> healthy|unhealthy
for line in "${VOTERS[@]}"; do
  member_id="$(echo "$line" | awk '{print $1}')"
  client_url="$(echo "$line" | awk '{print $3}')"
  status="unhealthy"
  for endpoint in "${HEALTHY_EPS[@]}"; do
    if [[ -n "${client_url}" && "${endpoint}" == "${client_url}" ]]; then
      status="healthy"
      break
    fi
  done
  MEMBER_HEALTH["$member_id"]="${status}"
done

# Check if target member is the current leader
info "Checking if target member is the current leader…"
ENDPOINT_STATUS="$(etcdctl_exec endpoint status --cluster || true)"

# Parse leader ID from endpoint status (text format)
# Format: endpoint, member_id(hex), version, db_size, is_leader, is_learner, raft_term, raft_index, raft_applied_index
# Check if the target member ID reports itself as leader (field 2 = member_id, field 5 = is_leader)
TARGET_IS_LEADER=0
if echo "${ENDPOINT_STATUS}" | awk -F', ' -v target="${TARGET_ID}" '$2 == target && $5 == "true"' | grep -q .; then
  TARGET_IS_LEADER=1
  info "Target member '${TARGET_NAME}' reports itself as the etcd leader."
fi

if (( TARGET_IS_LEADER == 1 )); then
  if (( DRY_RUN == 0 )); then
    info "Triggering leader election to transfer leadership before removal…"
    
    # Defensive check: ensure MEMBER_HEALTH is populated
    if [[ ${#MEMBER_HEALTH[@]} -eq 0 ]]; then
      warn "Member health information not available. Proceeding with removal (etcd will elect a new leader automatically)."
    else
      # Move leadership to another member
      # Find a healthy member that is not the target
      NEW_LEADER_ID=""
      for line in "${VOTERS[@]}"; do
        member_id="$(echo "$line" | awk '{print $1}')"
        if [[ "${member_id}" != "${TARGET_ID}" && "${MEMBER_HEALTH[$member_id]}" == "healthy" ]]; then
          # etcdctl move-leader expects the member ID in the same hex form as `member list`
          NEW_LEADER_ID="${member_id}"
          break
        fi
      done
      
      if [[ -n "${NEW_LEADER_ID}" ]]; then
        info "Transferring leadership to member ID: ${NEW_LEADER_ID}…"
        
        # Best-effort attempt to transfer leadership
        # move-leader requires connecting to the current leader's endpoint
        if etcdctl_exec --endpoint "${TARGET_URL}" move-leader "${NEW_LEADER_ID}" 2>/dev/null; then
          info "Leadership transfer command succeeded."
          # Give the cluster time to stabilize
          sleep 30
        else
          warn "Leadership transfer command failed. Continuing anyway (etcd will elect a new leader on removal)."
        fi
      else
        die "No alternative healthy member found for leadership transfer. Cannot safely remove the current leader."
      fi
    fi
  else
    info "[DRY-RUN] Would trigger leader election before removal."
  fi
else
  info "Target member is not the current leader. No leadership transfer needed."
fi

# Quorum assessment (pre-change + simulated post-change)
info "Performing quorum assessment…"

# Count total voters and how many are currently healthy
total_voters="${#VOTERS[@]}"
healthy_voters=0
for member_id in "${!MEMBER_HEALTH[@]}"; do
  [[ "${MEMBER_HEALTH[$member_id]}" == "healthy" ]] && ((healthy_voters++)) || true
done

# Check if the target member is currently healthy
target_is_healthy=0
[[ "${MEMBER_HEALTH[$TARGET_ID]}" == "healthy" ]] && target_is_healthy=1

# Calculate what the cluster would look like after removal
voters_after_removal=$((total_voters - 1))
healthy_after_removal=$((healthy_voters - target_is_healthy))
required_quorum=$(( (voters_after_removal / 2) + 1 ))

info "Quorum check: voters=${total_voters}, healthy=${healthy_voters}, target_healthy=${target_is_healthy} → post voters=${voters_after_removal}, healthy=${healthy_after_removal}, required_quorum=${required_quorum}"

# Check safety conditions
WOULD_BREAK_QUORUM=0
WOULD_LEAVE_SINGLE_VOTER=0

if (( healthy_after_removal < required_quorum )); then
  WOULD_BREAK_QUORUM=1
fi

if (( voters_after_removal == 1 )); then
  WOULD_LEAVE_SINGLE_VOTER=1
fi

# Dry-run mode: report what would happen
if (( DRY_RUN == 1 )); then
  if (( WOULD_BREAK_QUORUM == 1 )); then
    warn "[DRY-RUN] Removing '${TARGET_NAME}' would leave the cluster below quorum (healthy ${healthy_after_removal} < quorum ${required_quorum})."
    warn "[DRY-RUN] Requires --allow-quorum-loss flag to proceed (not recommended)."
    exit 2
  fi

  if (( WOULD_LEAVE_SINGLE_VOTER == 1 )); then
    warn "[DRY-RUN] This removal would leave a SINGLE-VOTER etcd (no HA)."
    warn "[DRY-RUN] Requires --allow-single-voter flag to proceed."
    exit 3
  fi

  info "[DRY-RUN] Safe to remove '${TARGET_NAME}' based on current health/quorum."
  exit 0
fi

# Normal mode: enforce safety checks unless allow flags are set
if (( WOULD_BREAK_QUORUM == 1 )) && (( ALLOW_QUORUM_LOSS == 0 )); then
  warn "Removing '${TARGET_NAME}' would leave the cluster below quorum (healthy ${healthy_after_removal} < quorum ${required_quorum})."
  warn "Add/repair a member before attempting removal again, or re-run with --allow-quorum-loss to override (not recommended)."
  exit 2
fi

if (( WOULD_LEAVE_SINGLE_VOTER == 1 )) && (( ALLOW_SINGLE_VOTER == 0 )); then
  warn "This removal will leave a SINGLE-VOTER etcd (no HA). Re-run with --allow-single-voter to proceed."
  exit 3
fi

info "Current cluster health (before):"
etcdctl_exec endpoint health --cluster -w table || true

info "Removing member ${TARGET_NAME} (ID: ${TARGET_ID})…"

if etcdctl_exec member remove "${TARGET_ID}"; then
  info "Member '${TARGET_NAME}' removed."
else
  die "Failed to remove member '${TARGET_NAME}'."
fi

info "Verifying removal…"
NEW_MEMBERS_LIST="$(etcdctl_exec member list)"
if echo "${NEW_MEMBERS_LIST}" | grep -q "${TARGET_ID}"; then
  die "Member still present after removal."
fi

NEW_VOTERS_COUNT=$(echo "$NEW_MEMBERS_LIST" | awk -F', ' '$6 == "false"' | wc -l)
info "New voter count: ${NEW_VOTERS_COUNT}"

echo ""
info "Updated member list:"
etcdctl_exec member list -w table || true

echo ""
info "Cluster health (after):"
etcdctl_exec endpoint health --cluster -w table || true
