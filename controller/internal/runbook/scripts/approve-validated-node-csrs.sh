#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0


# Purpose: Approves pending kubelet-serving CSRs only for nodes whose identity
#          is verified against a known-nodes allowlist passed by CWE, and whose
#          CSR SAN entries match the name and IP that CWE recorded for the node
#          at provisioning time.
#
#          The allowlist (name and IP) comes from CWE's postgres database, which
#          is an authoritative, out-of-band record independent of anything the
#          node self-reports to Kubernetes. We deliberately do NOT validate
#          against the Kubernetes Node object's .status.addresses, because that
#          field is reported by the kubelet itself (via --node-ip) and is
#          therefore attacker-controllable — the exact threat that makes
#          auto-approving kubelet-serving CSRs dangerous in the first place.
#
# Prerequisites:
# - Must be run on a Kubernetes control plane node
# - kubectl and openssl must be available
# - KNOWN_NODES must be set (semicolon-separated list of "name=ip" pairs for
#   CWE-managed nodes)

set -euo pipefail

KNOWN_NODES=${KNOWN_NODES:-""}
KUBECTL_BIN=${KUBECTL_BIN:-kubectl}
DATE_BIN=${DATE_BIN:-date}
KUBECTL_TIMEOUT=${KUBECTL_TIMEOUT:-"30s"}
export KUBECONFIG=${KUBECONFIG:-/etc/kubernetes/admin.conf}

APPROVED_COUNT=0
SKIPPED_COUNT=0
FAILED_COUNT=0
DENIED_COUNT=0

error() {
    echo "Error: $1" >&2
    exit 1
}

check_dependencies() {
    local deps=("$KUBECTL_BIN" "$DATE_BIN" "openssl" "awk" "sort")
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
    if [[ -z "$KNOWN_NODES" ]]; then
        error "KNOWN_NODES must be set (semicolon-separated list of name=ip pairs for nodes managed by CWE)"
    fi

    if [[ ! -f "$KUBECONFIG" ]]; then
        error "kubeconfig $KUBECONFIG does not exist, is this running on a control plane node?"
    fi
}

# Build associative array mapping known node name -> expected IP for O(1) lookup.
# Both sides come from CWE's postgres record for the node.
declare -A KNOWN_NODES_MAP
build_known_nodes_map() {
    IFS=';' read -ra pairs <<< "$KNOWN_NODES"
    for pair in "${pairs[@]}"; do
        pair=$(echo "$pair" | xargs)
        [[ -z "$pair" ]] && continue

        local name="${pair%%=*}"
        local ip="${pair#*=}"
        if [[ -z "$name" || -z "$ip" || "$name" == "$pair" ]]; then
            echo "  WARN: ignoring malformed KNOWN_NODES entry '$pair' (expected name=ip)"
            continue
        fi
        KNOWN_NODES_MAP["$name"]="$ip"
    done
    echo "Loaded ${#KNOWN_NODES_MAP[@]} known nodes from CWE"
}

# Extract the node name from a CSR requestor (system:node:<name> -> <name>)
extract_node_name() {
    local requestor="$1"
    if [[ "$requestor" =~ ^system:node:(.+)$ ]]; then
        echo "${BASH_REMATCH[1]}"
    fi
}

fetch_csr_request() {
    local csr_name="$1"
    "$KUBECTL_BIN" --request-timeout="${KUBECTL_TIMEOUT}" get csr "$csr_name" \
        -o jsonpath='{.spec.request}' 2>/dev/null || true
}

fetch_csr_groups() {
    local csr_name="$1"
    "$KUBECTL_BIN" --request-timeout="${KUBECTL_TIMEOUT}" get csr "$csr_name" \
        -o 'jsonpath={range .spec.groups[*]}{@}{"\n"}{end}' 2>/dev/null || true
}

fetch_csr_usages() {
    local csr_name="$1"
    "$KUBECTL_BIN" --request-timeout="${KUBECTL_TIMEOUT}" get csr "$csr_name" \
        -o 'jsonpath={range .spec.usages[*]}{@}{"\n"}{end}' 2>/dev/null || true
}

# Upstream requirement 1 (partial): the requestor must be in system:nodes.
# https://kubernetes.io/docs/reference/access-authn-authz/kubelet-tls-bootstrapping/#certificate-rotation
validate_csr_groups() {
    local csr_name="$1"
    local groups
    groups=$(fetch_csr_groups "$csr_name")

    if [[ -z "$groups" ]]; then
        echo "  REJECT: CSR '$csr_name' has no groups in spec.groups"
        return 1
    fi

    while IFS= read -r group; do
        [[ -z "$group" ]] && continue
        if [[ "$group" == "system:nodes" ]]; then
            return 0
        fi
    done <<< "$groups"

    echo "  REJECT: CSR '$csr_name' spec.groups does not include 'system:nodes'"
    return 1
}

# Upstream requirement 2: usages must contain "server auth", may optionally
# contain "digital signature" and "key encipherment", and nothing else.
validate_csr_usages() {
    local csr_name="$1"
    local usages
    usages=$(fetch_csr_usages "$csr_name")

    if [[ -z "$usages" ]]; then
        echo "  REJECT: CSR '$csr_name' has no usages in spec.usages"
        return 1
    fi

    local has_server_auth=false
    while IFS= read -r usage; do
        [[ -z "$usage" ]] && continue
        case "$usage" in
            "server auth")
                has_server_auth=true
                ;;
            "digital signature"|"key encipherment")
                ;;
            *)
                echo "  REJECT: CSR '$csr_name' contains disallowed usage '$usage'"
                return 1
                ;;
        esac
    done <<< "$usages"

    if [[ "$has_server_auth" != "true" ]]; then
        echo "  REJECT: CSR '$csr_name' is missing required usage 'server auth'"
        return 1
    fi

    return 0
}

# Extract SANs from a base64-encoded CSR PEM, preserving the type prefix so the
# caller can distinguish DNS names from IP addresses. Emits one normalized entry
# per line: "DNS:<value>" or "IP:<value>".
extract_csr_sans() {
    local csr_pem_b64="$1"

    if [[ -z "$csr_pem_b64" ]]; then
        return
    fi

    echo "$csr_pem_b64" | base64 -d 2>/dev/null \
        | openssl req -noout -text 2>/dev/null \
        | awk '/Subject Alternative Name/{getline; print}' \
        | tr ',' '\n' \
        | sed -E 's/^[[:space:]]+//; s/[[:space:]]+$//; s/^IP Address:/IP:/' \
        | sort -u
}

# Defense-in-depth: verify the CSR Subject has O=system:nodes and
# CN=system:node:<node-name>. Not strictly required for serving certs (they are
# not used for client auth) but ensures the cert identity matches the node.
validate_csr_subject() {
    local csr_pem_b64="$1"
    local csr_name="$2"
    local node_name="$3"

    local subject_line
    subject_line=$(echo "$csr_pem_b64" | base64 -d 2>/dev/null \
        | openssl req -noout -text 2>/dev/null \
        | awk '/^[[:space:]]+Subject:/{print; exit}')

    if [[ -z "$subject_line" ]]; then
        echo "  WARN: could not extract Subject from CSR '$csr_name', skipping"
        return 1
    fi

    # The openssl -text Subject line format varies across versions:
    #   OpenSSL 3.x:    Subject: O = system:nodes, CN = system:node:test-node-1
    #   LibreSSL:       Subject: O=system:nodes, CN=system:node:test-node-1
    local org cn
    org=$(echo "$subject_line" | sed 's/.*Subject:[[:space:]]*//' | tr ',' '\n' \
        | awk '/^[[:space:]]*O[[:space:]]*=[[:space:]]*/{ sub(/^[[:space:]]*O[[:space:]]*=[[:space:]]*/,""); print; exit }' | xargs)
    cn=$(echo "$subject_line" | sed 's/.*Subject:[[:space:]]*//' | tr ',' '\n' \
        | awk '/^[[:space:]]*CN[[:space:]]*=[[:space:]]*/{ sub(/^[[:space:]]*CN[[:space:]]*=[[:space:]]*/,""); print; exit }' | xargs)

    if [[ "$org" != "system:nodes" ]]; then
        echo "  REJECT: CSR '$csr_name' Subject Organization is '$org', expected 'system:nodes'"
        return 1
    fi

    local expected_cn="system:node:${node_name}"
    if [[ "$cn" != "$expected_cn" ]]; then
        echo "  REJECT: CSR '$csr_name' Subject CommonName is '$cn', expected '${expected_cn}'"
        return 1
    fi

    return 0
}

# Validate that the CSR's SANs are exactly the identities CWE recorded for the
# node: a DNS SAN matching the node name, and an IP SAN matching the postgres
# IP. Any SAN that is not one of these two is grounds for rejection, since it
# would let the node assert an identity it was not provisioned with.
validate_csr_sans() {
    local csr_pem_b64="$1"
    local csr_name="$2"
    local node_name="$3"
    local expected_ip="$4"

    local csr_sans
    csr_sans=$(extract_csr_sans "$csr_pem_b64")
    if [[ -z "$csr_sans" ]]; then
        echo "  WARN: could not extract SANs from CSR '$csr_name', skipping"
        return 1
    fi

    local expected_dns="DNS:${node_name}"
    local expected_ip_san="IP:${expected_ip}"

    while IFS= read -r san; do
        [[ -z "$san" ]] && continue
        if [[ "$san" != "$expected_dns" && "$san" != "$expected_ip_san" ]]; then
            echo "  REJECT: CSR '$csr_name' contains unexpected SAN '$san' (node '$node_name' is provisioned as '$expected_dns', '$expected_ip_san')"
            return 1
        fi
    done <<< "$csr_sans"

    return 0
}

# Validate a single CSR through all security checks. Returns 0 if valid.
validate_csr() {
    local csr_name="$1"
    local requestor="$2"

    local node_name
    node_name=$(extract_node_name "$requestor")
    if [[ -z "$node_name" ]]; then
        echo "  SKIP: CSR '$csr_name' requestor '$requestor' is not a system:node identity"
        SKIPPED_COUNT=$((SKIPPED_COUNT + 1))
        return 1
    fi

    if [[ -z "${KNOWN_NODES_MAP[$node_name]+x}" ]]; then
        echo "  SKIP: CSR '$csr_name' for node '$node_name' — not in CWE known-nodes list"
        SKIPPED_COUNT=$((SKIPPED_COUNT + 1))
        return 1
    fi

    if ! validate_csr_groups "$csr_name"; then
        SKIPPED_COUNT=$((SKIPPED_COUNT + 1))
        return 1
    fi

    if ! validate_csr_usages "$csr_name"; then
        SKIPPED_COUNT=$((SKIPPED_COUNT + 1))
        return 1
    fi

    local csr_pem_b64
    csr_pem_b64=$(fetch_csr_request "$csr_name")
    if [[ -z "$csr_pem_b64" ]]; then
        echo "  WARN: could not fetch CSR request for '$csr_name', skipping"
        SKIPPED_COUNT=$((SKIPPED_COUNT + 1))
        return 1
    fi

    if ! validate_csr_subject "$csr_pem_b64" "$csr_name" "$node_name"; then
        SKIPPED_COUNT=$((SKIPPED_COUNT + 1))
        return 1
    fi

    if ! validate_csr_sans "$csr_pem_b64" "$csr_name" "$node_name" "${KNOWN_NODES_MAP[$node_name]}"; then
        SKIPPED_COUNT=$((SKIPPED_COUNT + 1))
        return 1
    fi

    return 0
}

# Deny a CSR as superseded. Best-effort: don't fail the runbook if deny errors.
deny_csr() {
    local csr_name="$1"
    local node_name="$2"
    local reason="${3:-Superseded by newer CSR}"

    if "$KUBECTL_BIN" --request-timeout="${KUBECTL_TIMEOUT}" certificate deny "$csr_name" 2>/dev/null; then
        echo "  DENIED: stale CSR '$csr_name' for node '$node_name' ($reason)"
        DENIED_COUNT=$((DENIED_COUNT + 1))
    else
        echo "  WARN: could not deny CSR '$csr_name' (may have been GC'd already)"
    fi
}

approve_validated_csrs() {
    echo "Fetching pending kubelet-serving CSRs..."

    local pending_csrs
    pending_csrs=$("$KUBECTL_BIN" --request-timeout="${KUBECTL_TIMEOUT}" get csr \
        --no-headers \
        --output=custom-columns=NAME:.metadata.name,USERNAME:.spec.username,SIGNER:.spec.signerName,STATUS:.status.conditions[0].type,CREATED:.metadata.creationTimestamp \
        2>/dev/null \
        | awk '$3 == "kubernetes.io/kubelet-serving" && $4 != "Approved" && $4 != "Denied" && $4 != "Failed"' \
        || true)

    if [[ -z "$pending_csrs" ]]; then
        echo "No pending kubelet-serving CSRs found"
        return 0
    fi

    local total
    total=$(echo "$pending_csrs" | wc -l | xargs)
    echo "Found $total pending kubelet-serving CSR(s)"

    # Phase 1: Group CSRs by node, sorted newest-first (reverse chronological).
    # NODE_CSRS[node_name] = newline-separated "timestamp csr_name requestor" entries
    declare -A NODE_CSRS
    while IFS= read -r line; do
        local csr_name requestor created
        csr_name=$(echo "$line" | awk '{print $1}')
        requestor=$(echo "$line" | awk '{print $2}')
        created=$(echo "$line" | awk '{print $5}')

        if [[ -z "$csr_name" || -z "$requestor" ]]; then
            continue
        fi

        local node_name
        node_name=$(extract_node_name "$requestor")
        if [[ -z "$node_name" ]]; then
            echo "  SKIP: CSR '$csr_name' requestor '$requestor' is not a system:node identity"
            SKIPPED_COUNT=$((SKIPPED_COUNT + 1))
            continue
        fi

        if [[ -z "${KNOWN_NODES_MAP[$node_name]+x}" ]]; then
            echo "  SKIP: CSR '$csr_name' for node '$node_name' — not in CWE known-nodes list"
            SKIPPED_COUNT=$((SKIPPED_COUNT + 1))
            continue
        fi

        if [[ -n "${NODE_CSRS[$node_name]+x}" ]]; then
            NODE_CSRS["$node_name"]+=$'\n'"${created} ${csr_name} ${requestor}"
        else
            NODE_CSRS["$node_name"]="${created} ${csr_name} ${requestor}"
        fi
    done <<< "$pending_csrs"

    # Phase 2: For each node, walk CSRs newest-to-oldest. Approve the first one
    # that passes validation, then deny all remaining pending CSRs for that node.
    for node_name in "${!NODE_CSRS[@]}"; do
        local sorted_csrs
        sorted_csrs=$(echo "${NODE_CSRS[$node_name]}" | sort -r)

        local approved_one=false
        local stale_csrs=()

        while IFS= read -r entry; do
            [[ -z "$entry" ]] && continue
            local csr_name requestor
            csr_name=$(echo "$entry" | awk '{print $2}')
            requestor=$(echo "$entry" | awk '{print $3}')

            if [[ "$approved_one" == "true" ]]; then
                stale_csrs+=("$csr_name")
                continue
            fi

            if ! validate_csr "$csr_name" "$requestor"; then
                continue
            fi

            if "$KUBECTL_BIN" --request-timeout="${KUBECTL_TIMEOUT}" certificate approve "$csr_name" 2>/dev/null; then
                echo "  APPROVED: CSR '$csr_name' for node '$node_name'"
                APPROVED_COUNT=$((APPROVED_COUNT + 1))
                approved_one=true
            else
                echo "  FAIL: could not approve CSR '$csr_name' for node '$node_name'"
                FAILED_COUNT=$((FAILED_COUNT + 1))
            fi
        done <<< "$sorted_csrs"

        # Deny stale CSRs only if we successfully approved a newer one
        if [[ "$approved_one" == "true" && ${#stale_csrs[@]} -gt 0 ]]; then
            for stale_csr in "${stale_csrs[@]}"; do
                deny_csr "$stale_csr" "$node_name"
            done
        fi
    done
}

main() {
    check_dependencies
    validate_input
    build_known_nodes_map
    approve_validated_csrs
    echo "Summary: approved=$APPROVED_COUNT skipped=$SKIPPED_COUNT denied=$DENIED_COUNT failed=$FAILED_COUNT"

    if (( FAILED_COUNT > 0 )); then
        exit 1
    fi
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    main
fi
