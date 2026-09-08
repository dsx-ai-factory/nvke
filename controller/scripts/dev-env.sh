#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Development Environment Helper Functions
# This script contains all the complex logic for development environment management

set -euo pipefail

# Allow sourcing outside Makefile.dev.
SCRIPTS_DIR="${SCRIPTS_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)}"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Helper functions
info() { echo -e "${BLUE}$*${NC}"; }
success() { echo -e "${GREEN}$*${NC}"; }
warn() { echo -e "${YELLOW}$*${NC}" >&2; }
error() { echo -e "${RED}$*${NC}" >&2; }

# Check if temporal CLI is available
check_temporal_available() {
    if command -v temporal >/dev/null 2>&1; then
        success "✅ temporal CLI available"
        return 0
    elif [ -x "${HOME}/.temporalio/bin/temporal" ]; then
        success "✅ temporal CLI available at ${HOME}/.temporalio/bin/temporal"
        return 0
    elif command -v flox >/dev/null 2>&1; then
        success "✅ temporal CLI available via flox"
        return 0
    else
        error "❌ temporal CLI is required. Install it with one of:"
        echo "   brew install temporal"
        echo "   curl -sSf https://temporal.download/cli.sh | sh"
        echo "   brew install flox && flox activate"
        return 1
    fi
}

# Get the temporal command (with flox if needed)
get_temporal_cmd() {
    if command -v temporal >/dev/null 2>&1; then
        echo "temporal"
    # Where temporal.download/cli.sh installs it. It does not touch PATH, so a
    # non-interactive shell does not see it.
    elif [ -x "${HOME}/.temporalio/bin/temporal" ]; then
        echo "${HOME}/.temporalio/bin/temporal"
    elif command -v flox >/dev/null 2>&1; then
        echo "flox activate -- temporal"
    else
        echo "temporal"
    fi
}

# Start PostgreSQL container
postgres_start() {
    echo "Starting PostgreSQL..."
    
    if docker ps -a --format '{{.Names}}' | grep -q '^cwe-postgres$'; then
        if docker ps --format '{{.Names}}' | grep -q '^cwe-postgres$'; then
            success "✅ PostgreSQL is already running"
        else
            echo "Starting existing PostgreSQL container..."
            docker start cwe-postgres
            success "✅ PostgreSQL started"
        fi
    else
        echo "Creating new PostgreSQL container..."
        docker run -d --name=cwe-postgres -p 5432:5432 -e POSTGRES_PASSWORD=postgres postgres:18.6
        success "✅ PostgreSQL created and started"
        echo "Waiting for PostgreSQL to be ready..."
        sleep 3
    fi
}

# Stop PostgreSQL container
postgres_stop() {
    echo "Stopping PostgreSQL..."
    docker stop cwe-postgres 2>/dev/null || echo "PostgreSQL was not running"
}

# Clean PostgreSQL container
postgres_clean() {
    echo "Removing PostgreSQL container..."
    docker rm -f cwe-postgres 2>/dev/null || echo "PostgreSQL container does not exist"
    success "✅ PostgreSQL cleaned up"
}

# Start local Temporal server
temporal_start() {
    echo "Starting local Temporal server..."
    
    if pgrep -f "temporal server start-dev" > /dev/null; then
        success "✅ Temporal server is already running"
    else
        echo "Starting Temporal server in background..."
        local temporal_cmd
        temporal_cmd=$(get_temporal_cmd)
        nohup $temporal_cmd server start-dev > /tmp/temporal-dev.log 2>&1 &
        success "✅ Temporal server started (logs: /tmp/temporal-dev.log)"
        sleep 3
    fi
}

# Stop local Temporal server
temporal_stop() {
    echo "Stopping local Temporal server..."
    pkill -f "temporal server start-dev" || echo "Temporal server was not running"
    success "✅ Temporal server stopped"
}

# Check Teleport authentication status
tsh_check_status() {
    echo "Checking Teleport authentication status..."
    
    if tsh status > /dev/null 2>&1; then
        success "✅ Teleport authentication active"
        tsh status | head -10
        return 0
    else
        error "❌ Not logged in to Teleport"
        echo "   Run: make dev-tsh-login"
        return 1
    fi
}

# Login to Teleport
tsh_login() {
    local proxy="${1:-nv-prd-dgxc.teleport.sh:443}"
    
    echo "Logging in to Teleport..."
    echo "Proxy: $proxy"
    echo ""
    echo "⏳ This will open your browser for authentication..."
    tsh login --proxy="$proxy"
    success "✅ Teleport authentication successful"
}

# Login to Kubernetes cluster via Teleport
tsh_kube_login() {
    local cluster="$1"
    
    if [ -z "$cluster" ]; then
        error "❌ Cluster name is required"
        return 1
    fi
    
    echo "Logging in to Kubernetes cluster via Teleport..."
    echo "Cluster: $cluster"
    
    if ! tsh status > /dev/null 2>&1; then
        error "❌ Not logged in to Teleport. Please login first."
        return 1
    fi
    
    echo "Getting kubeconfig for cluster..."
    tsh kube login "$cluster"
    success "✅ Kubernetes access configured for $cluster"
    echo ""
    echo "Verifying connection..."
    kubectl cluster-info | head -3
}

# Check if kubectl can access cluster
remote_check() {
    echo "Checking remote cluster access..."
    
    if ! kubectl cluster-info > /dev/null 2>&1; then
        error "❌ kubectl cannot access cluster."
        echo ""
        echo "Run: TARGET_CLUSTER=<cluster-name> make dev-remote-connect"
        echo "Example: TARGET_CLUSTER=nke-site-forge-az24-dev6 make dev-remote-connect"
        return 1
    fi
    
    kubectl get namespace temporal > /dev/null 2>&1 || warn "⚠️  Warning: 'temporal' namespace not found in cluster"
    success "✅ kubectl is configured and cluster is accessible"
}

# Start port-forward to remote Temporal
remote_temporal_start() {
    local frontend_port="${1:-17233}"
    local ui_port="${2:-18233}"
    
    echo "Setting up port-forward to remote Temporal..."
    
    # Always stop existing port-forwards first to ensure clean state
    echo "Stopping any existing port-forwards..."
    pkill -f "kubectl port-forward.*temporal.*${frontend_port}:7233" 2>/dev/null || true
    pkill -f "kubectl port-forward.*temporal.*${ui_port}:8080" 2>/dev/null || true
    sleep 1
    
    # Start frontend port-forward
    echo "Starting Temporal frontend port-forward (${frontend_port}:7233)..."
    nohup kubectl port-forward -n temporal svc/temporal-frontend "${frontend_port}:7233" > /tmp/kubectl-temporal-frontend.log 2>&1 &
    success "✅ Temporal frontend port-forward started (logs: /tmp/kubectl-temporal-frontend.log)"
    sleep 2
    
    # Start UI port-forward
    echo "Starting Temporal UI port-forward (${ui_port}:8080)..."
    nohup kubectl port-forward -n temporal svc/temporal-web "${ui_port}:8080" > /tmp/kubectl-temporal-ui.log 2>&1 &
    success "✅ Temporal UI port-forward started (logs: /tmp/kubectl-temporal-ui.log)"
    sleep 2
    
    local current_cluster
    current_cluster=$(kubectl config current-context 2>/dev/null || echo "unknown")
    
    echo ""
    echo "📊 Remote Temporal Access:"
    echo "  • Cluster: $current_cluster"
    echo "  • Remote Temporal: localhost:${frontend_port}"
    echo "  • Remote Temporal UI: http://localhost:${ui_port}"
    echo ""
    echo "Set environment variables:"
    echo "  export TEMPORAL_HOST_PORT=localhost:${frontend_port}"
}

# Stop port-forward to remote Temporal
remote_temporal_stop() {
    local frontend_port="${1:-17233}"
    local ui_port="${2:-18233}"
    
    echo "Stopping remote Temporal port-forwards..."
    pkill -f "kubectl port-forward.*temporal.*${frontend_port}:7233" 2>/dev/null || echo "Temporal frontend port-forward was not running"
    pkill -f "kubectl port-forward.*temporal.*${ui_port}:8080" 2>/dev/null || echo "Temporal UI port-forward was not running"
    success "✅ Remote Temporal port-forwards stopped"
}

# Validate cluster argument
validate_cluster_arg() {
    local target_cluster="${1:-}"
    
    if [ -z "$target_cluster" ]; then
        error "❌ No cluster specified"
        echo ""
        echo "Usage:"
        echo "  TARGET_CLUSTER=<cluster-name> make dev-start-remote"
        echo "  TARGET_CLUSTER=<cluster-name> make remote  (alias)"
        echo ""
        echo "Example:"
        echo "  TARGET_CLUSTER=nke-site-forge-az24-dev6 make remote"
        echo ""
        echo "Or export it first:"
        echo "  export TARGET_CLUSTER=nke-site-forge-az24-dev6"
        echo "  make remote"
        echo ""
        echo "💡 Tip: Use 'FWE_API_KEY=<key> TARGET_CLUSTER=<name> make run-remote' to setup and run in one command"
        echo ""
        return 1
    fi
}

# Validate API key
validate_api_key() {
    local api_key="${1:-}"
    local mode="${2:-remote}"

    # The test Forge environment signs its own JWT instead of authenticating
    # against NGC, so it needs no key.
    if [ "${FORGE_ENVIRONMENT:-}" = "test" ]; then
        return 0
    fi

    if [ -z "$api_key" ]; then
        error "❌ FWE_API_KEY is not set"
        echo ""
        echo "Usage:"
        if [ "$mode" = "local" ]; then
            echo "  FWE_API_KEY=<your-key> make run-local"
        else
            echo "  FWE_API_KEY=<your-key> TARGET_CLUSTER=<cluster-name> make run-remote"
        fi
        echo ""
        return 1
    fi
    return 0
}

# Check if local Temporal is running
is_local_temporal_running() {
    pgrep -f "temporal server start-dev" > /dev/null
}

# Check if remote Temporal port-forwards are running
is_remote_temporal_running() {
    local frontend_port="${1:-17233}"
    pgrep -f "kubectl port-forward.*temporal.*${frontend_port}:7233" > /dev/null
}

# Check if PostgreSQL is running
is_postgres_running() {
    docker ps --format '{{.Names}}' | grep -q '^cwe-postgres$'
}

# Detect current development mode
detect_mode() {
    if is_local_temporal_running; then
        echo "local"
    elif is_remote_temporal_running; then
        echo "remote"
    else
        echo "none"
    fi
}

# Print status summary
print_status() {
    local frontend_port="${1:-17233}"
    local ui_port="${2:-18233}"
    
    echo ""
    echo "🔍 Development Environment Status"
    echo "=================================="
    echo ""
    
    # PostgreSQL
    echo "PostgreSQL:"
    if is_postgres_running; then
        success "  ✅ Running (port 5432)"
    else
        warn "  ❌ Not running"
        echo "  Start with: make dev-postgres-start"
    fi
    
    echo ""
    
    # Local Temporal
    echo "Local Temporal Server:"
    if is_local_temporal_running; then
        success "  ✅ Running (port 7233)"
        echo "  UI: http://localhost:8233"
        echo "  Logs: /tmp/temporal-dev.log"
    else
        warn "  ❌ Not running"
        echo "  Start with: make dev-temporal-start"
    fi
    
    echo ""
    
    # Remote Temporal
    echo "Remote Temporal (via kubectl port-forward):"
    if is_remote_temporal_running "$frontend_port"; then
        local current_cluster
        current_cluster=$(kubectl config current-context 2>/dev/null || echo "unknown")
        success "  ✅ Running (port $frontend_port)"
        echo "  Cluster: $current_cluster"
        echo "  UI: http://localhost:$ui_port"
        echo "  Logs: /tmp/kubectl-temporal-frontend.log"
        echo "        /tmp/kubectl-temporal-ui.log"
    else
        warn "  ❌ Not running"
        echo "  Start with: make dev-remote-temporal-start"
    fi
    
    echo ""
    
    # Teleport
    echo "Teleport:"
    if tsh status > /dev/null 2>&1; then
        success "  ✅ Authenticated"
        tsh status | grep -E "(Profile|Cluster|Logged in as)" | sed 's/^/  /'
    else
        warn "  ❌ Not authenticated"
        echo "  Login with: make dev-tsh-login"
    fi
    
    echo ""
    
}

# SPIRE helpers
source "${SCRIPTS_DIR}/dev-env-spire.sh"

# Export functions for use in Makefile
export -f info success warn error
export -f check_temporal_available get_temporal_cmd
export -f postgres_start postgres_stop postgres_clean
export -f temporal_start temporal_stop
export -f tsh_check_status tsh_login tsh_kube_login
export -f remote_check remote_temporal_start remote_temporal_stop
export -f validate_cluster_arg validate_api_key
export -f is_local_temporal_running is_remote_temporal_running is_postgres_running
export -f detect_mode print_status

