#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# SPIRE helpers for local development (sourced by dev-env.sh)

set -euo pipefail

# SPIRE local development setup
# spire-agent should be installed via flox and available on PATH
SPIRE_DATA_DIR="/tmp/spire-agent"
SPIRE_SOCKET="${SPIRE_DATA_DIR}/agent.sock"
SPIRE_PID_FILE="${SPIRE_DATA_DIR}/agent.pid"
SPIRE_CONFIG="${SPIRE_DATA_DIR}/agent.conf"
SPIRE_NAMESPACE="${SPIRE_NAMESPACE:-spire-server}"
SPIRE_SERVER_POD="${SPIRE_SERVER_POD:-spire-server-0}"
SPIRE_SERVER_PORT="${SPIRE_SERVER_PORT:-9091}"
SPIRE_ENTRY_CLASS_NAME="${SPIRE_ENTRY_CLASS_NAME:-spire-mgmt-spire}"

spire_print_status() {
    echo "SPIRE Local Development:"
    if spire_port_forward_is_tilt_managed; then
        success "  ✅ SPIRE Server port-forward running on port ${SPIRE_SERVER_PORT} (managed by Tilt)"
    elif spire_port_forward_is_running; then
        success "  ✅ SPIRE Server port-forward running (port $SPIRE_SERVER_PORT)"
        echo "  Logs: /tmp/kubectl-spire-server.log"
    else
        warn "  ❌ SPIRE Server port-forward not running"
        echo "  Start with: make dev-spire-start"
    fi

    if spire_agent_is_running; then
        success "  ✅ SPIRE Agent running (PID: $(cat "${SPIRE_PID_FILE}"))"
        echo "  Socket: ${SPIRE_SOCKET}"
        echo "  Logs: ${SPIRE_DATA_DIR}/agent.log"
    else
        warn "  ❌ SPIRE Agent not running"
        echo "  Start with: make dev-spire-start"
    fi

    echo ""
}

spire_cluster_static_entry_name() {
    local sanitized=$(hostname | tr '[:upper:]' '[:lower:]' | sed -E 's/[^a-z0-9-]+/-/g; s/^-+//; s/-+$//; s/-+/-/g')
    local username=$(id -un)
    echo "cwe-local-dev-${username}-${sanitized}"
}

# The controller's ServiceAccount, which is the last segment of its SPIFFE ID:
# nvke-controller where the cluster runs the controller under that name (Tilt,
# greenfield sites), then nke-controller for a site installed from the
# greenfield stacks before the nvke rename, else cloud-workflow-engine. Only a
# confirmed absence of all of them falls back to the legacy name.
spire_controller_service_account() {
    local found candidate
    for candidate in nvke-controller nke-controller; do
        found=$(kubectl get serviceaccount "${candidate}" -n "${candidate}" --ignore-not-found -o name 2>&1) || {
            error "❌ Cannot look up serviceaccount ${candidate}/${candidate}: ${found}"
            return 1
        }
        if grep -qx "serviceaccount/${candidate}" <<<"${found}"; then
            echo "${candidate}"
            return 0
        fi
    done
    echo "cloud-workflow-engine"
}

spire_apply_cluster_static_entry() {
    local trust_domain="$1"

    local entry_name=$(spire_cluster_static_entry_name)
    local service_account
    service_account=$(spire_controller_service_account) || return 1
    local spiffe_id="spiffe://${trust_domain}/nvke-service/${service_account}"
    local parent_id="spiffe://${trust_domain}/agent/local-dev/$(hostname)"

    local crd_yaml
    crd_yaml=$(cat <<EOF
apiVersion: spire.spiffe.io/v1alpha1
kind: ClusterStaticEntry
metadata:
  name: ${entry_name}
spec:
  className: "${SPIRE_ENTRY_CLASS_NAME}"
  spiffeID: "${spiffe_id}"
  parentID: "${parent_id}"
  selectors:
    - "unix:uid:$(id -u)"
EOF
)

    printf "%b\n" "${crd_yaml}" | kubectl apply -f - >/dev/null
}

spire_delete_cluster_static_entry() {
    local entry_name=$(spire_cluster_static_entry_name)
    if ! kubectl get crd clusterstaticentries.spire.spiffe.io >/dev/null 2>&1; then
        return 0
    fi
    kubectl delete clusterstaticentry.spire.spiffe.io "${entry_name}" --ignore-not-found || true
}

# Get trust domain from SPIRE server
spire_get_trust_domain() {
    # Use SPIRE's JSON output for clean parsing
    local bundle_output
    bundle_output=$(kubectl exec "${SPIRE_SERVER_POD}" -n "${SPIRE_NAMESPACE}" -c spire-server -- \
        bin/spire-server bundle show -output json 2>&1)
    local exit_code=$?

    if [[ ${exit_code} -ne 0 ]]; then
        error "❌ Failed to fetch bundle from SPIRE server (exit code: ${exit_code})"
        echo "" >&2
        echo "   Command output:" >&2
        echo "${bundle_output}" | sed 's/^/      /' >&2
        return 1
    fi

    local trust_domain
    trust_domain=$(echo "${bundle_output}" | jq -r '.trust_domain' 2>&1)

    if [[ -z "${trust_domain}" ]] || [[ "${trust_domain}" == "null" ]]; then
        error "❌ Failed to parse trust domain from bundle"
        echo "" >&2
        echo "   Bundle output:" >&2
        echo "${bundle_output}" | sed 's/^/      /' >&2
        return 1
    fi

    echo "${trust_domain}"
}

# Fetch trust bundle from SPIRE server
spire_fetch_trust_bundle() {
    local bundle_path="$1"

    if [[ -z "${bundle_path}" ]]; then
        error "❌ Trust bundle path is required"
        return 1
    fi

    # `2>&1 > file`, and the order is the whole point: 2>&1 first sends stderr
    # to where stdout currently goes -- the command substitution -- and only
    # then is stdout redirected into the file. The bundle lands in the file and
    # the diagnostics land in bundle_output.
    #
    # Written the other way round, as `> file 2>&1`, both streams end up in the
    # file, bundle_output is always empty, and the failure branch below prints
    # an empty "Command output:".
    bundle_output=$(kubectl exec "${SPIRE_SERVER_POD}" -n "${SPIRE_NAMESPACE}" -c spire-server -- \
        bin/spire-server bundle show 2>&1 > "${bundle_path}")
    local exit_code=$?

    if [[ ${exit_code} -ne 0 ]]; then
        error "❌ Failed to fetch trust bundle from SPIRE server (exit code: ${exit_code})"
        echo "" >&2
        echo "   Command output:" >&2
        echo "${bundle_output}" | sed 's/^/      /' >&2
        return 1
    fi

    success "✅ Trust bundle saved to ${bundle_path}"
}

# Cleanup stale local-dev entries/agents for this host
spire_cleanup_local_dev() {
    if ! kubectl get pod "${SPIRE_SERVER_POD}" -n "${SPIRE_NAMESPACE}" >/dev/null 2>&1; then
        return 0
    fi

    local trust_domain=$(spire_get_trust_domain)
    local hostname=$(hostname)

    spire_delete_cluster_static_entry

    # Find and delete all entries and agents for this host
    local agent_spiffe_id="spiffe://${trust_domain}/agent/local-dev/${hostname}"
    local agent_path="/agent/local-dev/${hostname}"

    local entries_json
    entries_json=$(kubectl exec "${SPIRE_SERVER_POD}" -n "${SPIRE_NAMESPACE}" -c spire-server -- \
        bin/spire-server entry show -output json 2>&1)
    local entries_exit_code=$?

    if [[ ${entries_exit_code} -ne 0 ]]; then
        warn "⚠️  Could not fetch entries for cleanup (exit code: ${entries_exit_code})"
        echo "${entries_json}" | sed 's/^/      /' >&2
        return 0
    fi

    local entry_ids
    entry_ids=$(echo "${entries_json}" | jq -r --arg td "${trust_domain}" --arg path "${agent_path}" \
        '.entries[]
         | select((.spiffe_id.trust_domain == $td and .spiffe_id.path == $path) or
                  (.parent_id.trust_domain == $td and .parent_id.path == $path))
         | .id')

    # Delete entries owned by this local-dev agent (agent entry + workload entries)
    if [[ -n "${entry_ids}" ]]; then
        echo "Cleaning up existing entries for ${agent_spiffe_id}..."
        for entry_id in ${entry_ids}; do
            kubectl exec "${SPIRE_SERVER_POD}" -n "${SPIRE_NAMESPACE}" -c spire-server -- \
                bin/spire-server entry delete -entryID "${entry_id}" >/dev/null 2>&1 || \
                warn "⚠️  Failed to delete entry ${entry_id}"
        done
        success "✅ Removed stale entries for ${agent_spiffe_id}"
    fi

    # Delete the join_token agent(s) that attested this local-dev agent
    local join_agent_ids
    join_agent_ids=$(echo "${entries_json}" | jq -r --arg td "${trust_domain}" --arg path "${agent_path}" \
        '.entries[]
         | select(.spiffe_id.trust_domain == $td and .spiffe_id.path == $path)
         | .parent_id
         | "spiffe://" + .trust_domain + .path' | sort -u)

    if [[ -n "${join_agent_ids}" ]]; then
        echo "Cleaning up join_token agents for ${agent_spiffe_id}..."
        for join_agent_id in ${join_agent_ids}; do
            kubectl exec "${SPIRE_SERVER_POD}" -n "${SPIRE_NAMESPACE}" -c spire-server -- \
                bin/spire-server agent evict -spiffeID "${join_agent_id}" >/dev/null 2>&1 || \
                warn "⚠️  Failed to delete agent ${join_agent_id}"
        done
        success "✅ Removed stale join_token agents"
    fi
}

# Generate join token from SPIRE server
spire_generate_join_token() {
    local spiffe_id="$1"

    if [[ -z "${spiffe_id}" ]]; then
        error "❌ SPIFFE ID is required"
        return 1
    fi

    local token_output
    token_output=$(kubectl exec "${SPIRE_SERVER_POD}" -n "${SPIRE_NAMESPACE}" -c spire-server -- \
        bin/spire-server token generate -spiffeID "${spiffe_id}" -output json 2>&1)
    local exit_code=$?

    if [[ ${exit_code} -ne 0 ]]; then
        error "❌ Failed to generate join token (exit code: ${exit_code})"
        echo "" >&2
        echo "   Command output:" >&2
        echo "${token_output}" | sed 's/^/      /' >&2
        echo "" >&2
        echo "   Command:" >&2
        echo "      kubectl exec ${SPIRE_SERVER_POD} -n ${SPIRE_NAMESPACE} -c spire-server -- \\" >&2
        echo "        bin/spire-server token generate -spiffeID ${spiffe_id} -output json" >&2
        return 1
    fi

    # Extract token value from JSON
    local join_token
    join_token=$(echo "${token_output}" | jq -r '.value' 2>&1)

    if [[ -z "${join_token}" ]] || [[ "${join_token}" == "null" ]]; then
        error "❌ Could not extract join token from output"
        echo "" >&2
        echo "   Full output:" >&2
        echo "${token_output}" | sed 's/^/      /' >&2
        return 1
    fi

    echo "${join_token}"
}

# Setup SPIRE agent (config + token + workload entry)
spire_setup() {
    echo "Setting up SPIRE local development environment..."

    # Check kubectl access
    if ! kubectl cluster-info > /dev/null 2>&1; then
        error "❌ kubectl cannot access cluster"
        echo ""
        echo "   Connect to cluster first with:"
        echo "      TARGET_CLUSTER=<cluster-name> make dev-remote-connect"
        return 1
    fi

    # Check if SPIRE server pod exists
    echo "Checking for SPIRE server..."
    if ! kubectl get pod "${SPIRE_SERVER_POD}" -n "${SPIRE_NAMESPACE}" >/dev/null 2>&1; then
        error "❌ SPIRE server pod ${SPIRE_SERVER_POD} not found in namespace ${SPIRE_NAMESPACE}"
        echo ""
        return 1
    fi
    success "✅ SPIRE server pod ${SPIRE_SERVER_POD} found"

    # Check if SPIRE agent binary is available on PATH
    if ! command -v spire-agent >/dev/null 2>&1; then
        error "❌ spire-agent not found on PATH"
        echo "" >&2
        echo "   spire-agent should be installed via flox environment" >&2
        echo "   Make sure you've activated the flox environment: flox activate" >&2
        echo "" >&2
        echo "   Or install manually:" >&2
        echo "      go install github.com/spiffe/spire/cmd/spire-agent" >&2
        return 1
    fi
    success "✅ spire-agent found: $(command -v spire-agent)"

    # Get trust domain from SPIRE server
    echo "Fetching trust domain from SPIRE server..."
    local trust_domain
    trust_domain=$(spire_get_trust_domain) || return 1

    success "✅ Trust domain: ${trust_domain}"

    # Fetch trust bundle for agent bootstrap
    echo "Fetching trust bundle..."
    mkdir -p "${SPIRE_DATA_DIR}"
    spire_fetch_trust_bundle "${SPIRE_DATA_DIR}/trust-bundle.pem" || return 1

    # Generate config
    echo "Generating agent configuration..."
    cat > "${SPIRE_CONFIG}" <<EOF
agent {
  data_dir = "${SPIRE_DATA_DIR}"
  log_level = "DEBUG"
  server_address = "localhost"
  server_port = "${SPIRE_SERVER_PORT}"
  socket_path = "${SPIRE_SOCKET}"
  trust_domain = "${trust_domain}"
  trust_bundle_path = "${SPIRE_DATA_DIR}/trust-bundle.pem"
}

plugins {
  NodeAttestor "join_token" {
    plugin_data {}
  }
  KeyManager "memory" {
    plugin_data {}
  }
  WorkloadAttestor "unix" {
    plugin_data {}
  }
}
EOF
    success "✅ Config written to ${SPIRE_CONFIG}"

    # Generate join token
    echo "Generating join token..."
    local hostname
    hostname=$(hostname)
    # Use /agent/ instead of /spire/agent/ since /spire/ is reserved
    local spiffe_id="spiffe://${trust_domain}/agent/local-dev/${hostname}"

    local join_token
    join_token=$(spire_generate_join_token "${spiffe_id}") || return 1

    echo "${join_token}" > "${SPIRE_DATA_DIR}/join-token"
    success "✅ Join token generated"

    # Create workload entry
    echo "Creating workload entry for the controller..."
    if spire_apply_cluster_static_entry "${trust_domain}"; then
        success "✅ ClusterStaticEntry applied"
    else
        error "❌ Failed to apply ClusterStaticEntry"
        return 1
    fi

    success "✅ Setup complete!"
    echo ""
    echo "📋 Next steps:"
    echo "  3. Run the controller with SPIRE: set spire.spiffeEnabled: true in a copy of hack/local-controller-config.yaml, then CONTROLLER_CONFIG_PATH=<your copy> make run"
    echo ""
}

spire_port_forward_is_tilt_managed() {
    # Tilt forwards svc/spire-server-np to the NodePort (e.g. 30440:443), which is a
    # different process pattern than the script's pod-direct forward (30440:8081).
    pgrep -f "kubectl.*port-forward.*${SPIRE_NAMESPACE}.*${SPIRE_SERVER_PORT}:443" > /dev/null 2>&1
}

spire_port_forward_is_running() {
    spire_port_forward_is_tilt_managed || \
        pgrep -f "kubectl port-forward.*${SPIRE_NAMESPACE}.*${SPIRE_SERVER_PORT}:8081" > /dev/null
}

spire_port_forward_start() {
    # If Tilt is already forwarding this port (svc/spire-server-np -> 443), don't
    # start a competing port-forward; the SPIRE agent can use the Tilt-managed one.
    if spire_port_forward_is_tilt_managed; then
        success "✅ SPIRE server port-forward already active on port ${SPIRE_SERVER_PORT} (managed by Tilt)"
        return 0
    fi

    # Always stop existing port-forwards first to ensure clean state; In case they were left over from a different TARGET_CLUSTER
    spire_port_forward_stop
    sleep 1

    if pgrep -f "kubectl port-forward.*${SPIRE_NAMESPACE}.*${SPIRE_SERVER_PORT}:8081" > /dev/null; then
        success "✅ SPIRE server port-forward is already running"
    else
        echo "Starting SPIRE server port-forward (${SPIRE_SERVER_PORT}:8081)..."
        nohup kubectl port-forward -n "${SPIRE_NAMESPACE}" "${SPIRE_SERVER_POD}" "${SPIRE_SERVER_PORT}:8081" > /tmp/kubectl-spire-server.log 2>&1 &
        success "✅ SPIRE server port-forward started (logs: /tmp/kubectl-spire-server.log)"
        sleep 2
    fi
}

spire_port_forward_stop() {
    echo "Stopping SPIRE server port-forward..."
    pkill -f "kubectl port-forward.*${SPIRE_NAMESPACE}.*${SPIRE_SERVER_PORT}:8081" || echo "SPIRE server port-forward was not running"
    success "✅ SPIRE server port-forward stopped"
}

spire_agent_is_running() {
    [[ -f "${SPIRE_PID_FILE}" ]] && kill -0 "$(cat "${SPIRE_PID_FILE}")" 2>/dev/null
}

# Start SPIRE agent and port-forward
spire_agent_start() {
    # Best-effort: if SPIRE isn't installed on this cluster, skip setup
    if ! kubectl get pod "${SPIRE_SERVER_POD}" -n "${SPIRE_NAMESPACE}" >/dev/null 2>&1; then
        warn "⚠️  SPIRE server pod ${SPIRE_SERVER_POD} not found in namespace ${SPIRE_NAMESPACE}; skipping SPIRE setup"
        return 0
    fi

    spire_port_forward_start

    # Start SPIRE agent if not running
    if spire_agent_is_running; then
        local desired_trust_domain
        desired_trust_domain=$(spire_get_trust_domain) || return 1

        local current_trust_domain
        if [[ -f "${SPIRE_CONFIG}" ]]; then
            current_trust_domain=$(sed -nE 's/^[[:space:]]*trust_domain[[:space:]]*=[[:space:]]*"([^"]+)".*$/\1/p' "${SPIRE_CONFIG}" | head -n 1)
        fi

        if [[ -z "${current_trust_domain}" ]]; then
            warn "⚠️  SPIRE agent config missing trust_domain; cleaning and restarting agent"
        elif [[ "${current_trust_domain}" != "${desired_trust_domain}" ]]; then
            warn "⚠️  SPIRE trust domain changed (${current_trust_domain} -> ${desired_trust_domain}); cleaning and restarting agent"
        else
            success "✅ SPIRE agent is already running (PID: $(cat "${SPIRE_PID_FILE}") with trust domain ${current_trust_domain})"
            return 0
        fi

        spire_clean
        spire_port_forward_start
    fi

    # Ensure setup is complete before starting
    spire_setup || return 1

    local join_token
    join_token=$(cat "${SPIRE_DATA_DIR}/join-token")

    echo "Starting SPIRE agent..."
    spire-agent run -config "${SPIRE_CONFIG}" -joinToken "${join_token}" > "${SPIRE_DATA_DIR}/agent.log" 2>&1 &

    echo $! > "${SPIRE_PID_FILE}"
    sleep 2

    if kill -0 "$(cat "${SPIRE_PID_FILE}")" 2>/dev/null; then
        success "✅ SPIRE agent started (PID: $(cat "${SPIRE_PID_FILE}"))"
        echo "   Socket: ${SPIRE_SOCKET}"
        echo "   Logs: ${SPIRE_DATA_DIR}/agent.log"
    else
        error "❌ SPIRE agent failed to start"
        echo "   Check logs: ${SPIRE_DATA_DIR}/agent.log"
        return 1
    fi
}

# Stop SPIRE agent and port-forward
spire_stop() {
    spire_delete_cluster_static_entry

    spire_port_forward_stop

    if [[ ! -f "${SPIRE_PID_FILE}" ]]; then
        echo "SPIRE agent is not running"
        return 0
    fi

    local pid
    pid=$(cat "${SPIRE_PID_FILE}")
    if kill -0 "${pid}" 2>/dev/null; then
        echo "Stopping SPIRE agent (PID: ${pid})..."
        kill "${pid}"
        sleep 1
        if kill -0 "${pid}" 2>/dev/null; then
            kill -9 "${pid}"
        fi
        rm "${SPIRE_PID_FILE}"
        success "✅ SPIRE agent stopped"
    else
        echo "SPIRE agent is not running"
        rm "${SPIRE_PID_FILE}"
    fi
}

# Clean SPIRE setup
spire_clean() {
    echo "Cleaning up SPIRE local development setup..."
    spire_cleanup_local_dev || warn "⚠️  Cleanup failed (continuing)"
    spire_stop

    # Remove data directory
    if [[ -d "${SPIRE_DATA_DIR}" ]]; then
        echo "Removing data directory: ${SPIRE_DATA_DIR}"
        rm -rf "${SPIRE_DATA_DIR}"
    fi

    success "✅ SPIRE cleanup complete"
}

# Export functions for use in Makefile
export -f spire_print_status
export -f spire_get_trust_domain spire_fetch_trust_bundle spire_cleanup_local_dev spire_generate_join_token spire_setup
export -f spire_port_forward_start spire_port_forward_stop spire_agent_start spire_stop spire_clean
export -f spire_agent_is_running spire_port_forward_is_running spire_port_forward_is_tilt_managed
