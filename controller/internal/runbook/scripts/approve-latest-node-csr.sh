#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0


# Purpose: Approves the most recent CSR (Certificate Signing Request) for a specific Node (not older than CSR_MAX_AGE_MINUTES)
# Prerequisites:
# - Must be run on a Kubernetes control plane node
# - kubectl must be available
# - Must have admin privileges (kubeconfig with appropriate permissions)

set -euo pipefail

NODE_NAME=${NODE_NAME:-""}
CSR_MAX_AGE_MINUTES=${CSR_MAX_AGE_MINUTES:-10}
INTERVAL_SECONDS=${INTERVAL_SECONDS:-30}
NODE_MAX_AGE_MINUTES=${NODE_MAX_AGE_MINUTES:-60} # default: 1h 
INITIAL_CSR_WAIT_TIMEOUT_SECONDS=${INITIAL_CSR_WAIT_TIMEOUT_SECONDS:-900} # default: 15mins
NEW_CSR_WAIT_TIMEOUT_SECONDS=${NEW_CSR_WAIT_TIMEOUT_SECONDS:-360} # default: 6mins
KUBECTL_TIMEOUT=${KUBECTL_TIMEOUT:-"30s"}
KUBECTL_BIN=${KUBECTL_BIN:-kubectl}
DATE_BIN=${DATE_BIN:-date}
export KUBECONFIG=${KUBECONFIG:-/etc/kubernetes/admin.conf}

error() {
    echo "Error: $1" >&2
    exit 1
}

check_dependencies() {
    local deps=("$KUBECTL_BIN" "$DATE_BIN" "sort" "awk")
    local missing_deps=()
    for dep in "${deps[@]}"; do
        if ! command -v "$dep" &>/dev/null; then
            missing_deps+=("$dep")
        fi
    done

    if [[ ${#missing_deps[@]} -gt 0 ]]; then
        error "Missing required dependencies: ${missing_deps[*]}"
    fi
}

validate_input() {
    if [[ -z "$NODE_NAME" ]]; then
        error "NODE_NAME must be set"
    fi

    if [[ ! -f "$KUBECONFIG" ]]; then
        error "kubeconfig $KUBECONFIG does not exist, is this running on a control plane node?"
    fi
}

# If node is older than configured NODE_MAX_AGE_MINUTES (set to typical Garbage collection (GC) TTL of 1h), does a fallback logs endpoint check on node. 
check_node_age() {
    local node="$NODE_NAME"
    
    # Get node creation timestamp
    local node_created_ts
    node_created_ts=$("$KUBECTL_BIN" get node "$node" -o jsonpath='{.metadata.creationTimestamp}')

    if [[ -z "$node_created_ts" ]]; then
        error "could not get node timestamp"
    fi

    local node_created_sec
    node_created_sec=$("$DATE_BIN" --date="$node_created_ts" +%s)
    
    if [[ -z "$node_created_sec" ]]; then
        error "unexpected error: could not parse node creationTimestamp"
    fi

    local now_sec
    now_sec=$("$DATE_BIN" +%s)
    local node_age_min=$(( (now_sec - node_created_sec) / 60 ))

    if (( node_age_min > NODE_MAX_AGE_MINUTES )); then
        # Indicates CSR is already approved and might be garbage collected 
        echo "Node is older than configured limit of $NODE_MAX_AGE_MINUTES mins. CSRs may be garbage collected"
        return 1
    fi
    return 0
}

# Check kubelet metrics to validate CSR approval 
check_kubelet_metrics() {
    # Validation: try to fetch kubelet metrics via API server proxy which requires valid serving certificates
    echo "Attempting validation - checking kubelet metrics endpoint via API server proxy."
    
    local max_retries=3
    local retry_interval=10
    local retries=0
    local metrics
    
    while [[ $retries -lt $max_retries ]]; do
        metrics=$("$KUBECTL_BIN" --request-timeout="${KUBECTL_TIMEOUT}" get --raw "/api/v1/nodes/${NODE_NAME}/proxy/metrics" 2>/dev/null || true)
        
        if [[ -n "$metrics" ]]; then
            echo "Successfully fetched metrics from kubelet on node '$NODE_NAME' via API server proxy."
            echo "Validation complete - kubelet serving certificate is working."
            return 0
        fi
        
        retries=$((retries + 1))
        if [[ $retries -lt $max_retries ]]; then
            echo "Failed to fetch metrics (attempt $retries/$max_retries), retrying in ${retry_interval}s..."
            sleep "$retry_interval"
        fi
    done
    
    error "ERROR: failed to fetch metrics from kubelet via API server proxy after $max_retries attempts. This indicates the kubelet serving CSR may NOT be approved or kubelet is not responding"
}

get_latest_csr_for_node() {
    local requestor="system:node:$NODE_NAME"
    local signer_name="kubernetes.io/kubelet-serving"
    # Get CSR for a specific node and signer. Sort by timestamp (newest first) and pick the CSR on top of the list
    # NOTE: "pending" CSRs are not filtered as re-approving an already approved CSR is safe and idempotent
    "$KUBECTL_BIN" get csr \
      --no-headers \
      --output=custom-columns=NAME:.metadata.name,USERNAME:.spec.username,SIGNER:.spec.signerName,CREATED:.metadata.creationTimestamp \
        | awk -v req="$requestor" -v signer="$signer_name" '$2 == req && $3 == signer' \
        | sort -k4 -r \
        | head -n1 \
        | awk '{print $1, $4}' 2>/dev/null  # Output: CSR_NAME CREATED_TIMESTAMP
}

approve_latest_csr() {
    echo "DEBUG: Starting approve_latest_csr for node '$NODE_NAME'"
    
    local start_time
    start_time=$("$DATE_BIN" +%s)
    local now_sec
    local csr_info
    local initial_csr_name=""
    local elapsed
    while true; do
        
        # save current time
        now_sec=$("$DATE_BIN" +%s)

        # exit if exceeds timeout period
        elapsed=$(( now_sec - start_time ))
        if (( elapsed >= INITIAL_CSR_WAIT_TIMEOUT_SECONDS )) && [[ -z "$csr_info" ]]; then
            error "Timeout waiting to get CSR for $NODE_NAME"
        fi
        if (( elapsed >= NEW_CSR_WAIT_TIMEOUT_SECONDS )) && [[ -n "$initial_csr_name" ]]; then
            # here when we already approved a CSR but its age >10 mins. we wait for new one with age <10mins and timeout
            echo "No other new CSRs found after waiting for $NEW_CSR_WAIT_TIMEOUT_SECONDS seconds"
            break
        fi

        csr_info="$(get_latest_csr_for_node 2>/dev/null || true)"

        if [[ -z "$csr_info" ]]; then
            # "No CSR found. check if node is older than 1h (GC TTL). If it is, proceed to validation
            # Refer https://kubernetes.io/docs/reference/access-authn-authz/certificate-signing-requests/#request-signing-process for more info
            # Dev note - may not be fully accurate way to evaluate if CSRs are garbage collected (accurate would be to look at cert file on the node) but for our usecase, this should be acceptable. we validate later with "kubectl logs"
            # because, in CWE, we run this runbook on NodeJoin so running into this very unlikely
            # This is an edge case added to keep the runbook idempotent
            if ! check_node_age; then
                echo "Exiting CSR check loop."
                break
            fi
            
            # If we are here, node is newer and No CSR found. we will Retry in $INTERVAL_SECONDS seconds..."
            sleep "$INTERVAL_SECONDS"
            continue
        fi

        # if we are here, CSR is found
        local csr_name csr_created
        read -r csr_name csr_created <<< "$csr_info"

        if [[ -z "$csr_name" || -z "$csr_created" ]]; then
            error "unexpected error. couldn't parse latest CSR info"
        fi

        local csr_created_sec
        csr_created_sec=$("$DATE_BIN" --date="$csr_created" +%s || true)

        if [[ -z "$csr_created_sec" ]]; then
            error "unexpected error. couldn't parse creation time for CSR"
        fi

        local csr_age_min=$(( (now_sec - csr_created_sec) / 60 ))

        if [[ -z "$csr_age_min" ]]; then
            # it should never reach this error if everything is as expected
            error "unexpected error. csr_age_min is empty"
        fi

        # If seeing this CSR for first time (within the context on this run), approve
        if [[ -z "$initial_csr_name" || "$initial_csr_name" != "$csr_name" ]]; then
            if ! "$KUBECTL_BIN" certificate approve "$csr_name"; then
                error "Failed to approve CSR '$csr_name' for '$NODE_NAME'"
            fi
            echo "✅ Successfully approved CSR '$csr_name' (age: $csr_age_min min) for node '$NODE_NAME'"

            if (( csr_age_min <= CSR_MAX_AGE_MINUTES )); then
                # Approved newest CSR (age <10 mins). exit loop
                break
            fi
        fi

        # Set initial_csr_name
        if [[ -z "$initial_csr_name" ]]; then
            initial_csr_name="$csr_name"
        fi  
        # kubelet creates CSR every 15mins. possibility of a race condition if we approve older one and exit as kubelet creates a new one (before status is sycned)
        # Hence, to avoid this race condition if CSR is older than CSR_MAX_AGE_MINUTES(default is 10mins), we wait incase a newer CSR is created
        sleep "$INTERVAL_SECONDS"
    done
}

main() {
    check_dependencies
    validate_input
    approve_latest_csr
    check_kubelet_metrics # helps validate CSR approval especially when CSRs are garbage collected
}

# Execute main if script is run directly
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    main
fi
