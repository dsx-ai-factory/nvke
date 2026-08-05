#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# Generate a kubeconfig for an NKE cluster running on forge-vms.
#
# Queries the CWE API for the cluster VIP and control plane node IPs,
# SSHes into a CP node, extracts the cluster CA, signs a short-lived
# client certificate, and writes a kubeconfig file.
#
# Usage:
#   get-kubeconfig.sh --cluster-name <name> [options]
#   get-kubeconfig.sh --cluster-id <uuid>   [options]
#
# Options:
#   --cluster-name  NAME    Cluster name (looked up via CWE API)
#   --cluster-id    UUID    Cluster ID (direct lookup)
#   --cwe-api       URL     CWE API base URL        (default: http://localhost:8080/api)
#   --ssh-user      USER    SSH user on CP nodes     (default: forge-diag)
#   --ssh-key       PATH    SSH private key          (default: ~/.ssh/id_rsa)
#   --ssh-password          Use password auth instead of key (reads SSHPASS env var)
#   --sudo-password PASS    Password for sudo on the node (default: same as SSH password)
#   --cert-days     N       Client cert validity     (default: 365)
#   --cert-cn       CN      Client cert common name  (default: nke-admin)
#   --cert-org      ORG     Client cert organization (default: system:masters)
#   --output        PATH    Output kubeconfig path   (default: ./kubeconfig-<cluster-name>)
#   --context-name  NAME    Kubeconfig context name  (default: <cluster-name>)
#   -h, --help              Show this help

set -euo pipefail

# ── defaults ─────────────────────────────────────────────────────────
# Default diagnostic user/password injected into all VMs by forge-vms.
DIAG_SSH_USER="forge-diag"
DIAG_SSH_PASSWORD="forge-diag"

CWE_API="${CWE_API:-http://localhost:8080/api}"
SSH_USER="${SSH_USER:-}"
SSH_KEY="${SSH_KEY:-$HOME/.ssh/id_rsa}"
SSH_PASSWORD=false
SUDO_PASSWORD=""
CERT_DAYS=365
CERT_CN="nke-admin"
CERT_ORG="system:masters"
CLUSTER_NAME=""
CLUSTER_ID=""
OUTPUT=""
CONTEXT_NAME=""

# ── parse args ───────────────────────────────────────────────────────
while [[ $# -gt 0 ]]; do
  case "$1" in
    --cluster-name) CLUSTER_NAME="$2"; shift 2 ;;
    --cluster-id)   CLUSTER_ID="$2";   shift 2 ;;
    --cwe-api)      CWE_API="$2";      shift 2 ;;
    --ssh-user)     SSH_USER="$2";      shift 2 ;;
    --ssh-key)      SSH_KEY="$2";       shift 2 ;;
    --ssh-password) SSH_PASSWORD=true;  shift   ;;
    --sudo-password) SUDO_PASSWORD="$2"; shift 2 ;;
    --cert-days)    CERT_DAYS="$2";     shift 2 ;;
    --cert-cn)      CERT_CN="$2";       shift 2 ;;
    --cert-org)     CERT_ORG="$2";      shift 2 ;;
    --output)       OUTPUT="$2";        shift 2 ;;
    --context-name) CONTEXT_NAME="$2";  shift 2 ;;
    -h|--help)
      sed -n '2,/^$/{ s/^# \{0,1\}//; p }' "$0"
      exit 0
      ;;
    *) echo "Unknown option: $1" >&2; exit 1 ;;
  esac
done

if [[ -z "$CLUSTER_NAME" && -z "$CLUSTER_ID" ]]; then
  echo "Error: provide --cluster-name or --cluster-id" >&2
  exit 1
fi

# ── helpers ──────────────────────────────────────────────────────────
die()  { echo "Error: $*" >&2; exit 1; }
info() { echo "==> $*" >&2; }

# Fall back to the diagnostic user/password injected into all forge-vms VMs.
if [[ -z "$SSH_USER" ]]; then
  SSH_USER="$DIAG_SSH_USER"
  SSH_PASSWORD=true
  export SSHPASS="$DIAG_SSH_PASSWORD"
  SUDO_PASSWORD="$DIAG_SSH_PASSWORD"
  info "No --ssh-user provided; using diagnostic credentials ($SSH_USER)"
fi

api_get() {
  local url="${CWE_API}$1"
  local resp http_code body

  resp=$(curl -sf --write-out '\n%{http_code}' "$url") || die "API call failed: GET $url"
  http_code=$(echo "$resp" | tail -1)
  body=$(echo "$resp" | sed '$d')

  if [[ "$http_code" -lt 200 || "$http_code" -ge 300 ]]; then
    die "API returned HTTP $http_code for GET $url"
  fi
  echo "$body"
}

run_ssh() {
  local host="$1"; shift
  local ssh_opts=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR)

  if [[ "$SSH_PASSWORD" == true ]]; then
    sshpass -e ssh "${ssh_opts[@]}" "${SSH_USER}@${host}" "$@"
  else
    ssh "${ssh_opts[@]}" -i "$SSH_KEY" "${SSH_USER}@${host}" "$@"
  fi
}

# Resolve the sudo password: explicit flag > SSHPASS env var (password mode) > prompt
resolve_sudo_password() {
  if [[ -n "$SUDO_PASSWORD" ]]; then
    return
  fi
  if [[ "$SSH_PASSWORD" == true && -n "${SSHPASS:-}" ]]; then
    SUDO_PASSWORD="$SSHPASS"
    return
  fi
  # Prompt interactively
  read -rsp "Enter sudo password for ${SSH_USER}@CP node: " SUDO_PASSWORD >&2
  echo >&2
}

# Run a command over SSH with sudo -S (password piped via stdin).
# Usage: run_ssh_sudo <host> <command-string>
run_ssh_sudo() {
  local host="$1"; shift
  local ssh_opts=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR)

  if [[ "$SSH_PASSWORD" == true ]]; then
    # sshpass feeds the SSH password; we pipe the sudo password into the remote command
    echo "$SUDO_PASSWORD" | sshpass -e ssh "${ssh_opts[@]}" "${SSH_USER}@${host}" "sudo -S $*"
  else
    echo "$SUDO_PASSWORD" | ssh "${ssh_opts[@]}" -i "$SSH_KEY" "${SSH_USER}@${host}" "sudo -S $*"
  fi
}

require_cmd() {
  command -v "$1" &>/dev/null || die "'$1' is required but not found in PATH"
}

# ── preflight ────────────────────────────────────────────────────────
require_cmd curl
require_cmd jq
require_cmd openssl
require_cmd ssh
if [[ "$SSH_PASSWORD" == true ]]; then
  require_cmd sshpass
fi

# ── step 1: resolve cluster ─────────────────────────────────────────
if [[ -n "$CLUSTER_NAME" && -z "$CLUSTER_ID" ]]; then
  info "Looking up cluster '$CLUSTER_NAME'..."
  clusters_json=$(api_get "/clusters?name=${CLUSTER_NAME}")
  count=$(echo "$clusters_json" | jq '.clusters | length')

  if [[ "$count" -eq 0 ]]; then
    die "No cluster found with name '$CLUSTER_NAME'"
  fi
  if [[ "$count" -gt 1 ]]; then
    die "Multiple clusters found with name '$CLUSTER_NAME' — use --cluster-id instead"
  fi

  CLUSTER_ID=$(echo "$clusters_json" | jq -r '.clusters[0].id')
  CLUSTER_NAME=$(echo "$clusters_json" | jq -r '.clusters[0].name')
fi

info "Fetching cluster $CLUSTER_ID..."
cluster_json=$(api_get "/clusters/${CLUSTER_ID}")
VIP=$(echo "$cluster_json" | jq -r '.controlPlaneVIP // empty')
CLUSTER_NAME=${CLUSTER_NAME:-$(echo "$cluster_json" | jq -r '.name')}

if [[ -z "$VIP" ]]; then
  die "Cluster has no controlPlaneVIP set"
fi
info "Cluster VIP: $VIP"

# ── step 2: find a control plane node ────────────────────────────────
info "Finding control plane nodes..."
pools_json=$(api_get "/nodePools?clusterID=${CLUSTER_ID}")
cp_pool_ids=$(echo "$pools_json" | jq -r '.nodePools[] | select(.controlPlane == true) | .id')

if [[ -z "$cp_pool_ids" ]]; then
  die "No control plane node pools found for cluster"
fi

nodes_json=$(api_get "/nodes?clusterID=${CLUSTER_ID}")
CP_IP=""
for pool_id in $cp_pool_ids; do
  CP_IP=$(echo "$nodes_json" | jq -r \
    --arg pid "$pool_id" \
    '[.nodes[] | select(.nodePoolID == $pid and .ip != null and .ip != "")] | first | .ip // empty')
  if [[ -n "$CP_IP" ]]; then
    break
  fi
done

if [[ -z "$CP_IP" ]]; then
  die "No control plane node with an IP address found"
fi
info "Using control plane node: $CP_IP"

# ── step 3: get cluster CA from the node ─────────────────────────────
resolve_sudo_password
info "Retrieving cluster CA certificate..."
CA_CERT=$(run_ssh_sudo "$CP_IP" "cat /etc/kubernetes/pki/ca.crt" 2>/dev/null) \
  || die "Failed to read CA cert from $CP_IP"

# ── step 4: generate client key + CSR locally ───────────────────────
TMPDIR_WORK=$(mktemp -d)
trap 'rm -rf "$TMPDIR_WORK"' EXIT

CLIENT_KEY="$TMPDIR_WORK/client.key"
CLIENT_CSR="$TMPDIR_WORK/client.csr"
CLIENT_CERT="$TMPDIR_WORK/client.crt"

info "Generating client key and CSR (CN=${CERT_CN}, O=${CERT_ORG})..."
openssl genrsa -out "$CLIENT_KEY" 2048 2>/dev/null
openssl req -new -key "$CLIENT_KEY" -out "$CLIENT_CSR" \
  -subj "/CN=${CERT_CN}/O=${CERT_ORG}" 2>/dev/null

# ── step 5: sign the CSR on the control plane node ──────────────────
info "Signing client certificate on control plane node (valid ${CERT_DAYS} days)..."
REMOTE_CSR="/tmp/nke-client-${RANDOM}.csr"

# Upload CSR to the node (no sudo needed, stdin → file)
run_ssh "$CP_IP" "cat > ${REMOTE_CSR}" < "$CLIENT_CSR" \
  || die "Failed to upload CSR to $CP_IP"

# Sign with the cluster CA (needs sudo for CA key); cert goes to stdout
SIGNED_CERT=$(run_ssh_sudo "$CP_IP" \
  "openssl x509 -req -in ${REMOTE_CSR} -CA /etc/kubernetes/pki/ca.crt -CAkey /etc/kubernetes/pki/ca.key -CAcreateserial -days ${CERT_DAYS} -sha256" 2>/dev/null) \
  || die "Failed to sign client certificate on $CP_IP"

# Clean up remote temp file
run_ssh "$CP_IP" "rm -f ${REMOTE_CSR}" 2>/dev/null || true

echo "$SIGNED_CERT" > "$CLIENT_CERT"

# ── step 6: build kubeconfig ────────────────────────────────────────
CONTEXT_NAME="${CONTEXT_NAME:-$CLUSTER_NAME}"
OUTPUT="${OUTPUT:-./kubeconfig-${CLUSTER_NAME}}"

CA_B64=$(echo "$CA_CERT" | base64 -w0)
CERT_B64=$(base64 -w0 < "$CLIENT_CERT")
KEY_B64=$(base64 -w0 < "$CLIENT_KEY")
USER_NAME="${CERT_CN}@${CLUSTER_NAME}"

info "Writing kubeconfig to $OUTPUT..."
cat > "$OUTPUT" <<EOF
apiVersion: v1
kind: Config
clusters:
- name: ${CONTEXT_NAME}
  cluster:
    server: https://${VIP}:6443
    certificate-authority-data: ${CA_B64}
contexts:
- name: ${CONTEXT_NAME}
  context:
    cluster: ${CONTEXT_NAME}
    user: ${USER_NAME}
current-context: ${CONTEXT_NAME}
users:
- name: ${USER_NAME}
  user:
    client-certificate-data: ${CERT_B64}
    client-key-data: ${KEY_B64}
EOF

chmod 600 "$OUTPUT"

info "Done! Kubeconfig written to: $OUTPUT"
info ""
info "Usage:"
info "  export KUBECONFIG=$OUTPUT"
info "  kubectl get nodes"
