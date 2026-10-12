#!/bin/bash
# SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Generate or resync platform CRs from the controller API.
# Fetches all clusters and node pools from the controller, generates ForgeTenant, ForgeSite,
# ForgeVPC, NKECluster, NKENodePool. Use for onboarding new clusters or resyncing
# after changes made via the controller API. Prerequisites: jq.

set -euo pipefail

API_GROUP="compute.private.x-dgxc.nvidia.com"
API_VERSION="v1alpha1"

usage() {
    echo "Usage: $0 --cwe-api-url <url> --forge-api-url <url> --namespace <ns> [--output-dir <dir>] [--dry-run] [--apply]"
    echo ""
    echo "Options:"
    echo "  --cwe-api-url URL     controller API base (e.g. http://localhost:8080 with: kubectl port-forward -n cloud-workflow-engine svc/cloud-workflow-engine 8080:8080)"
    echo "  --forge-api-url URL   forge-console API base for /forge/* metadata (e.g. http://localhost:8090)"
    echo "  --namespace NS        Platform namespace for CRs (target; may differ from controller/site)"
    echo "  --output-dir DIR      Output directory (default: ./output/<forgeVPC.name>)"
    echo "  --dry-run             Print YAML only, do not write or apply"
    echo "  --apply               After generating, apply CRs (kubeconfig = platform API)"
    exit 1
}

CWE_API_URL=""
FORGE_API_URL=""
NAMESPACE_ARG=""
OUTPUT_DIR=""
DRY_RUN=false
APPLY=false

while [[ $# -gt 0 ]]; do
    case "$1" in
        --cwe-api-url) CWE_API_URL="${2%/}"; shift 2 ;;
        --forge-api-url) FORGE_API_URL="${2%/}"; shift 2 ;;
        --namespace)   NAMESPACE_ARG="$2"; shift 2 ;;
        --output-dir)  OUTPUT_DIR="$2"; shift 2 ;;
        --dry-run)     DRY_RUN=true; shift ;;
        --apply)       APPLY=true; shift ;;
        -h|--help)     usage ;;
        *) echo "Unknown option: $1"; usage ;;
    esac
done

if [[ -z "$CWE_API_URL" ]] || [[ -z "$FORGE_API_URL" ]] || [[ -z "$NAMESPACE_ARG" ]]; then
    echo "Error: --cwe-api-url, --forge-api-url, and --namespace are required"
    usage
fi

if ! command -v jq &>/dev/null; then
    echo "Error: jq is required. Install jq."
    exit 1
fi

# ---------- API base ----------
API="$CWE_API_URL"
if [[ "$API" != */api ]]; then
    API="${CWE_API_URL}/api"
fi

FORGE_API="$FORGE_API_URL"
if [[ "$FORGE_API" != */api ]]; then
    FORGE_API="${FORGE_API}/api"
fi

# ---------- Resolve list of cluster IDs (all clusters on the controller) ----------
CLUSTER_IDS=()
all_clusters=$(curl -sS --max-time 30 "${API}/clusters" || true)
if [[ -z "$all_clusters" ]] || [[ "$(echo "$all_clusters" | jq -r '.clusters // empty')" == "" ]]; then
    echo "Error: failed to fetch clusters from the controller"
    exit 1
fi
while IFS= read -r id; do
    [[ -n "$id" ]] && CLUSTER_IDS+=("$id")
done < <(echo "$all_clusters" | jq -r '.clusters[].id')
if [[ ${#CLUSTER_IDS[@]} -eq 0 ]]; then
    echo "Error: no clusters found in the controller"
    exit 1
fi
echo "Found ${#CLUSTER_IDS[@]} cluster(s) on the controller"

namespace="$NAMESPACE_ARG"

# ---------- Fetch Forge site metadata from forge-console ----------
# forge-console is single-org/site: GET /forge/site returns the configured site
# (including orgID/name). Tenants list and /forge/sites?orgID= no longer exist.
FORGE_SITE_JSON=$(curl -sS "${FORGE_API}/forge/site" 2>/dev/null || echo '{}')
FORGE_ORG_ID=$(echo "$FORGE_SITE_JSON" | jq -r '.orgID // empty')
FORGE_SITE_NAME=$(echo "$FORGE_SITE_JSON" | jq -r '.name // empty')
FORGE_SITE_NATIVE=$(echo "$FORGE_SITE_JSON" | jq -r '.capabilities.nativeNetworking // false')
FORGE_TENANT_NAME=$(echo "$FORGE_SITE_JSON" | jq -r '.name // empty')
if [[ -z "$FORGE_ORG_ID" ]]; then
    echo "Error: failed to fetch Forge site metadata from ${FORGE_API}/forge/site"
    exit 1
fi

# Track written VPC and site names so we only emit each once
WRITTEN_VPC_NAMES=()
WRITTEN_SITE_NAMES=()
# Accumulate apply script lines and cluster summary for CLUSTER-INFO
APPLY_LINES=()
INFO_CLUSTERS=()

write_file() {
    local path="$1"
    local content="$2"
    if [[ "$DRY_RUN" == true ]]; then
        echo "--- $path ---"
        echo "$content"
        echo ""
    else
        echo "$content" > "$path"
        echo "  wrote $path"
    fi
}

# Normalize CNI to CR value
normalize_cni() {
    local cni="$1"
    case "$(echo "$cni" | tr '[:upper:]' '[:lower:]')" in
        cilium)   echo "Cilium" ;;
        calico)   echo "Calico" ;;
        ovnkubernetes) echo "OvnKubernetes" ;;
        mtu)      echo "Mtu" ;;
        *)        echo "Calico" ;;
    esac
}

# Emit UUID string from API JSON (handles string or object with .value/.Value)
uuid_from_json() {
    echo "$1" | jq -r 'if type == "string" then . else ((.value // .Value // .) | tostring) end // empty'
}

ZERO_UUID="00000000-0000-0000-0000-000000000000"
first_cluster=true
for cluster_id in "${CLUSTER_IDS[@]}"; do
    cluster_json=$(curl -sS "${API}/clusters/${cluster_id}" || true)
    if [[ -z "$cluster_json" ]] || [[ "$(echo "$cluster_json" | jq -r '.id // empty')" == "" ]]; then
        echo "Error: failed to fetch cluster from the controller: $cluster_id"
        exit 1
    fi

    nodepools_json=$(curl -sS "${API}/nodePools?clusterID=${cluster_id}" || true)
    nodepools_json=$(echo "$nodepools_json" | jq -c '.nodePools // []')

    cluster_name=$(echo "$cluster_json" | jq -r '.name')
    # Org ID and site ID must be present on the cluster detail from the controller API
    org_id=$(echo "$cluster_json" | jq -r '.orgID // empty')
    site_id=$(uuid_from_json "$(echo "$cluster_json" | jq -c '.siteID')")
    if [[ -z "$org_id" ]] || [[ -z "$site_id" ]]; then
        echo "Error: cluster $cluster_id is missing orgID or siteID in the controller API response; cannot generate CRs"
        exit 1
    fi
    vpc_id=$(uuid_from_json "$(echo "$cluster_json" | jq -c '.vpcID')")
    control_plane_k8s_version=$(echo "$cluster_json" | jq -r '.controlPlaneKubernetesVersion')
    cni_plugin=$(normalize_cni "$(echo "$cluster_json" | jq -r '.cniPlugin')")
    control_plane_vip=$(echo "$cluster_json" | jq -r '.controlPlaneVIP // empty')
    enable_cp_recycling=$(echo "$cluster_json" | jq -r '.enableCPNodeRecycling // false')

    # Forge site name and nativeNetworking from forge-console /forge/site
    forge_site_name="$FORGE_SITE_NAME"
    forge_site_native_networking="$FORGE_SITE_NATIVE"
    if [[ -z "$forge_site_name" ]]; then
        forge_site_name="site-${site_id:0:8}"
    fi

    forge_vpc_name=$(echo "$(curl -sS "${FORGE_API}/forge/vpcs" 2>/dev/null || echo '{"vpcs":[]}')" | jq -r --arg id "$vpc_id" '(.vpcs // [])[] | select(.id == $id) | .name // empty')
    if [[ -z "$forge_vpc_name" ]]; then
        forge_vpc_name="vpc-${vpc_id:0:8}"
    fi

    if [[ "$first_cluster" == true ]]; then
        # Forge tenant: forge-console is single-org; prefer site metadata, else namespace
        forge_tenant_name="$FORGE_TENANT_NAME"
        [[ -z "$forge_tenant_name" ]] && forge_tenant_name="$namespace"
        forge_tenant_id="$org_id"
        forge_site_id="$site_id"
        if [[ -z "$OUTPUT_DIR" ]]; then
            OUTPUT_DIR="$(pwd)/output/${forge_vpc_name}"
        fi
        if [[ "$DRY_RUN" != true ]]; then
            mkdir -p "$OUTPUT_DIR"
        fi

        # --- ForgeTenant ---
        write_file "$OUTPUT_DIR/forge-tenant.yaml" "apiVersion: ${API_GROUP}/${API_VERSION}
kind: ForgeTenant
metadata:
  name: ${forge_tenant_name}
  namespace: ${namespace}
spec:
  ngcOrgId: ${forge_tenant_id}
"
        APPLY_LINES+=("kubectl apply -f forge-tenant.yaml")
        first_cluster=false
    fi

    # --- ForgeSite ---
    _seen_site=false
    for _w in "${WRITTEN_SITE_NAMES[@]:-}"; do [[ "$_w" == "$forge_site_name" ]] && _seen_site=true && break; done
    if [[ "$_seen_site" != true ]]; then
        write_file "$OUTPUT_DIR/forge-site-${forge_site_name}.yaml" "apiVersion: ${API_GROUP}/${API_VERSION}
kind: ForgeSite
metadata:
  name: ${forge_site_name}
  namespace: ${namespace}
spec:
  id: ${forge_site_id}
  nativeNetworkingEnabled: ${forge_site_native_networking}
"
        WRITTEN_SITE_NAMES+=("$forge_site_name")
        APPLY_LINES+=("kubectl apply -f forge-site-${forge_site_name}.yaml")
    fi

    # --- ForgeVPC ---
    _seen_vpc=false
    for _w in "${WRITTEN_VPC_NAMES[@]:-}"; do [[ "$_w" == "$forge_vpc_name" ]] && _seen_vpc=true && break; done
    if [[ "$_seen_vpc" != true ]]; then
        write_file "$OUTPUT_DIR/forge-vpc-${forge_vpc_name}.yaml" "apiVersion: ${API_GROUP}/${API_VERSION}
kind: ForgeVPC
metadata:
  name: ${forge_vpc_name}
  namespace: ${namespace}
spec:
  id: ${vpc_id}
  forgeTenantRef:
    name: ${forge_tenant_name}
  forgeSiteRef:
    name: ${forge_site_name}
"
        WRITTEN_VPC_NAMES+=("$forge_vpc_name")
        APPLY_LINES+=("kubectl apply -f forge-vpc-${forge_vpc_name}.yaml")
    fi

    echo "Generating CRs for cluster: $cluster_name"
    echo "  Forge VPC CR name: $forge_vpc_name  K8s version: $control_plane_k8s_version  CNI: $cni_plugin"

    # --- NKECluster ---
    cluster_spec="  forgeVPCRef:
    name: ${forge_vpc_name}
  controlPlaneKubernetesVersion: \"${control_plane_k8s_version}\"
  cniPlugin: ${cni_plugin}
  enableCPNodeRecycling: ${enable_cp_recycling}
"
    if [[ -n "$control_plane_vip" ]]; then
        cluster_spec+="  controlPlaneVIP: \"${control_plane_vip}\"
"
    fi
    write_file "$OUTPUT_DIR/nvke-cluster-${cluster_name}.yaml" "apiVersion: ${API_GROUP}/${API_VERSION}
kind: NKECluster
metadata:
  name: ${cluster_name}
  namespace: ${namespace}
spec:
${cluster_spec}
"
    APPLY_LINES+=("kubectl apply -f nvke-cluster-${cluster_name}.yaml")

    # --- NKENodePools (per cluster) ---
    np_count=$(echo "$nodepools_json" | jq 'length')
    INFO_CLUSTERS+=("${cluster_name}: ${np_count} node pool(s)")
    for (( idx=0; idx<np_count; idx++ )); do
        np=$(echo "$nodepools_json" | jq -c ".[$idx]")
        np_name=$(echo "$np" | jq -r '.name // empty')
        np_prefix=$(echo "$np" | jq -r '.nodeNamePrefix // empty')
        np_instance_type=$(uuid_from_json "$(echo "$np" | jq -c '.instanceTypeID // empty')")
        np_os_profile=$(echo "$np" | jq -r '.osProfile // empty')
        np_config_profile=$(echo "$np" | jq -r '.configProfile // empty')
        np_desired=$(echo "$np" | jq -r '.desiredNodeCount // 1')
        np_control_plane=$(echo "$np" | jq -r '.controlPlane // false')
        np_node_type="Worker"
        [[ "$np_control_plane" == "true" ]] && np_node_type="ControlPlane"
        np_system_mem=$(echo "$np" | jq -r '.systemReservedMemory // "16Gi"')
        np_auto_provision_disabled=$(echo "$np" | jq -r '.autoProvisionDisabled // false')
        np_auto_mode_disabled=$(echo "$np" | jq -r '.autoModeDisabled // false')
        np_recycle_budget=$(echo "$np" | jq -r '.recycleBudget // 0')
        np_max_surge=$(echo "$np" | jq -r '.maxSurge // 0')
        # Validate required fields; skip node pools with missing or zero UUIDs (controller data issue)
        if [[ -z "$np_name" ]] || [[ -z "$np_prefix" ]] || [[ -z "$np_os_profile" ]] || [[ -z "$np_config_profile" ]]; then
            echo "  Warning: skipping node pool (missing name, nodeNamePrefix, osProfile, or configProfile): $np"
            continue
        fi
        if [[ -z "$np_instance_type" ]] || [[ "$np_instance_type" == "$ZERO_UUID" ]] || [[ "$np_instance_type" == "null" ]]; then
            echo "  Warning: skipping node pool '$np_name' - instanceTypeID is missing or zero in the controller API (fix data in the controller)"
            continue
        fi
        extra_spec=""
        subnet_ids=$(echo "$np" | jq -r '(.subnetIDs // [])[] | tostring' 2>/dev/null)
        if [[ -n "$subnet_ids" ]]; then
            extra_spec="  subnetIDs:"$'\n'
            while IFS= read -r s; do [[ -n "$s" ]] && [[ "$s" != "null" ]] && extra_spec="${extra_spec}    - ${s}"$'\n'; done <<< "$subnet_ids"
        fi
        vpc_prefix_ids=$(echo "$np" | jq -r '(.vpcPrefixIDs // [])[] | tostring' 2>/dev/null)
        if [[ -n "$vpc_prefix_ids" ]]; then
            extra_spec="${extra_spec}  vpcPrefixIDs:"$'\n'
            while IFS= read -r v; do [[ -n "$v" ]] && [[ "$v" != "null" ]] && extra_spec="${extra_spec}    - ${v}"$'\n'; done <<< "$vpc_prefix_ids"
        fi
        ssh_ids=$(echo "$np" | jq -r '(.sshKeyGroupIDs // [])[] | tostring' 2>/dev/null)
        if [[ -n "$ssh_ids" ]]; then
            extra_spec="${extra_spec}  sshKeyGroupIDs:"$'\n'
            while IFS= read -r k; do [[ -n "$k" ]] && [[ "$k" != "null" ]] && extra_spec="${extra_spec}    - ${k}"$'\n'; done <<< "$ssh_ids"
        fi
        np_ib=$(uuid_from_json "$(echo "$np" | jq -c '.infinibandPartitionID // empty')")
        if [[ -n "$np_ib" ]] && [[ "$np_ib" != "null" ]] && [[ "$np_ib" != "$ZERO_UUID" ]]; then
            extra_spec="${extra_spec}  infinibandPartitionID: ${np_ib}"$'\n'
        fi
        # Optional CRD fields (match platform CRD defaults when not set)
        extra_spec="${extra_spec}  autoProvisionDisabled: ${np_auto_provision_disabled}"$'\n'
        extra_spec="${extra_spec}  autoModeDisabled: ${np_auto_mode_disabled}"$'\n'
        extra_spec="${extra_spec}  recycleBudget: ${np_recycle_budget}"$'\n'
        extra_spec="${extra_spec}  maxSurge: ${np_max_surge}"$'\n'
        np_yaml="apiVersion: ${API_GROUP}/${API_VERSION}
kind: NKENodePool
metadata:
  name: ${np_name}
  namespace: ${namespace}
spec:
  nkeClusterRef:
    name: ${cluster_name}
  nodeType: ${np_node_type}
  nodeNamePrefix: ${np_prefix}
  instanceTypeID: ${np_instance_type}
  osProfile: ${np_os_profile}
  configProfile: ${np_config_profile}
  desiredNodeCount: ${np_desired}
  systemReservedMemory: ${np_system_mem}
${extra_spec}"
        write_file "$OUTPUT_DIR/nke-nodepool-${cluster_name}-${np_name}.yaml" "$np_yaml"
        APPLY_LINES+=("kubectl apply -f nke-nodepool-${cluster_name}-${np_name}.yaml")
    done
    echo ""
done

# --- Apply script (order: ForgeTenant -> ForgeSite -> ForgeVPC -> NKECluster -> NKENodePool) ---
apply_script="#!/bin/bash
# Apply generated CRs. Run from output dir. KUBECONFIG = platform API.
set -euo pipefail
NAMESPACE=\"${namespace}\"
echo \"Applying CRs (namespace in YAML): \$NAMESPACE\"
"
for line in "${APPLY_LINES[@]}"; do
    apply_script="${apply_script}"$'\n'"${line}"
done
apply_script="${apply_script}"$'\n'
apply_script="${apply_script}"$'\n'"echo \"Done. Monitor: kubectl get nkecluster,nkenodepool -n \$NAMESPACE\""
write_file "$OUTPUT_DIR/apply-all.sh" "$apply_script"
if [[ "$DRY_RUN" != true ]]; then
    chmod +x "$OUTPUT_DIR/apply-all.sh"
fi

# --- Cluster info (audit) ---
write_file "$OUTPUT_DIR/CLUSTER-INFO.txt" "Generated: $(date -u +%Y-%m-%dT%H:%M:%SZ)
Namespace: ${namespace}
Clusters:
$(printf '  - %s\n' "${INFO_CLUSTERS[@]}")

Apply: cd $(basename "$OUTPUT_DIR") && ./apply-all.sh
Order: ForgeTenant -> ForgeSite -> ForgeVPC -> NKECluster -> NKENodePool
"

echo "Output: $OUTPUT_DIR"
echo "Apply: cd $OUTPUT_DIR && ./apply-all.sh"
echo "Monitor: kubectl get nkecluster,nkenodepool -n $namespace"
echo ""

if [[ "$APPLY" == true ]]; then
    if [[ "$DRY_RUN" == true ]]; then
        echo "Skipping --apply when --dry-run is set."
    else
        echo "Applying CRs to platform (KUBECONFIG)..."
        (cd "$OUTPUT_DIR" && ./apply-all.sh)
    fi
fi
