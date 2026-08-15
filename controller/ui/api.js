/**
 * SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
 * SPDX-License-Identifier: LicenseRef-NvidiaProprietary
 *
 * NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
 * property and proprietary rights in and to this material, related
 * documentation and any modifications thereto. Any use, reproduction,
 * disclosure or distribution of this material and related documentation
 * without an express license agreement from NVIDIA CORPORATION or
 * its affiliates is strictly prohibited.
 */

const API_BASE_URL = '/api';

/**
 * @typedef {Object} CloudInitFile
 * @property {string} path
 * @property {string} content
 * @property {string} [owner]
 * @property {string} [permissions]
 */

/**
 * Cluster-level control-plane config sent on POST /api/clusters when the server's
 * provisionControlPlaneWithControlPlaneController flag is enabled (cpControllerMode).
 * Only fields the user set are included; blank optional fields are omitted.
 * @typedef {Object} CreateClusterControlPlaneConfig
 * @property {number} desiredCount Required, >= 1.
 * @property {string[]} [instanceTypePreference] Ordered instance-type UUIDs (Forge; index 0 = highest priority).
 * @property {string[]} [subnetIDs] Forge, site without native networking.
 * @property {string[]} [vpcPrefixIDs] Forge, site with native networking.
 * @property {string} [osProfile] Forge.
 * @property {string} [configProfile] Forge.
 * @property {string} [systemReservedMemory] e.g. "16Gi".
 * @property {string[]} [sshKeyGroupIDs] Forge.
 * @property {string[]} [nodeLabels] e.g. "key=value".
 * @property {boolean} [teleportEnabled]
 * @property {number} [maxSurge] 0 or 1.
 * @property {boolean} [provisioningMonitorDisabled]
 * @property {Object} [capiInfraMachineTemplate] CAPI-only manifest.
 * @property {("tpm"|"joinToken")} [capiSpireAttestationMethod] CAPI-only.
 * @property {CloudInitFile[]} [capiBootstrapFiles] CAPI-only.
 * @property {string[]} [capiPreKubeadmCommands] CAPI-only.
 */

/**
 * Cluster-level control-plane config patch sent on PATCH /api/clusters/{id}. Accepts the
 * same spec fields as CreateClusterControlPlaneConfig (all optional here) plus the
 * previously-supported maxSurge / instanceTypePreference / provisioningMonitorDisabled.
 * Only changed fields are included.
 * @typedef {Partial<CreateClusterControlPlaneConfig>} UpdateClusterControlPlaneConfig
 */

function asArray(value) {
    return Array.isArray(value) ? value : [];
}

/**
 * GET /api/version. Response includes "version", "temporalUIURL", "temporalNamespace",
 * and the boolean "provisionControlPlaneWithControlPlaneController" feature flag that
 * toggles the UI between legacy (control-plane node pool) and cluster-level-CP modes.
 */
async function getVersion() {
    const response = await fetch(`${API_BASE_URL}/version`);

    if (!response.ok) {
        throw new Error(await parseErrorResponse(response));
    }

    return await response.json();
}

async function getClusters() {
    const response = await fetch(`${API_BASE_URL}/clusters`);

    if (!response.ok) {
        throw new Error(await parseErrorResponse(response));
    }

    const body = await response.json();
    return asArray(body.clusters);
}

async function getClusterDetails(clusterID) {
    const response = await fetch(`${API_BASE_URL}/clusters/${clusterID}`);

    if (!response.ok) {
        throw new Error(await parseErrorResponse(response));
    }

    const clusterDetails = await response.json();
    return clusterDetails;
}

function parseNodePoolsResponse(result) {
    if (Array.isArray(result)) {
        return { nodePools: result, clusterControlPlaneNodes: [], nodeCounts: null };
    }
    return {
        nodePools: result?.nodePools ?? [],
        clusterControlPlaneNodes: result?.clusterControlPlaneNodes ?? [],
        nodeCounts: result?.nodeCounts ?? null,
    };
}

function computeNodeCounts(nodes, nodePools, clusterControlPlaneNodes) {
    // Role comes from the node pool; /nodes may omit controlPlane.
    const roleByNodeID = new Map();
    for (const nodePool of nodePools) {
        const role = nodePool.controlPlane ? 'controlPlane' : 'worker';
        for (const nodeSet of nodePool.nodeSets ?? []) {
            for (const node of nodeSet.nodes ?? []) {
                roleByNodeID.set(node.id, role);
            }
        }
    }
    for (const node of clusterControlPlaneNodes) {
        roleByNodeID.set(node.id, 'controlPlane');
    }

    const emptyBucket = () => ({ total: 0, provisioned: 0, inProgress: 0, pending: 0 });
    const controlPlane = emptyBucket();
    const worker = emptyBucket();
    let unassigned = 0;

    for (const node of nodes) {
        const role = roleByNodeID.get(node.id);
        if (!role) {
            unassigned++;
            continue;
        }
        const bucket = role === 'controlPlane' ? controlPlane : worker;
        bucket.total++;
        // Read each condition directly: a node mid-provision reports both
        // pending=False and provisioned=False.
        if (node?.conditions?.provisioned?.status === 'True') {
            bucket.provisioned++;
        } else if (node?.conditions?.pending?.status === 'True') {
            bucket.pending++;
        } else {
            bucket.inProgress++;
        }
    }

    return {
        total: nodes.length,
        provisioned: controlPlane.provisioned + worker.provisioned,
        inProgress: controlPlane.inProgress + worker.inProgress,
        pending: controlPlane.pending + worker.pending,
        unassigned,
        controlPlane,
        worker,
    };
}

async function getNodePools(clusterID) {
    // query nodePools and nodes in parallel
    const [nodePoolsResponse, nodeSetsResponse, nodesResponse] = await Promise.all([
        fetch(`${API_BASE_URL}/nodePools?clusterID=${clusterID}`),
        fetch(`${API_BASE_URL}/nodeSets?clusterID=${clusterID}`),
        fetch(`${API_BASE_URL}/nodes?clusterID=${clusterID}`)
    ]);

    if (!nodePoolsResponse.ok) {
        throw new Error(await parseErrorResponse(nodePoolsResponse));
    }
    const nodePoolsBody = await nodePoolsResponse.json();
    const nodePools = asArray(nodePoolsBody.nodePools);

    if (!nodeSetsResponse.ok) {
        throw new Error(await parseErrorResponse(nodeSetsResponse));
    }
    const nodeSetsBody = await nodeSetsResponse.json();
    const nodeSets = asArray(nodeSetsBody.nodeSets);

    if (!nodesResponse.ok) {
        throw new Error(await parseErrorResponse(nodesResponse));
    }
    const nodesBody = await nodesResponse.json();
    const nodes = asArray(nodesBody.nodes);

    const isNilUUID = (value) => !value || value === '00000000-0000-0000-0000-000000000000';
    const isControlPlaneNode = (node) => {
        if (typeof node?.controlPlane === 'boolean') {
            return node.controlPlane;
        }
        // Backward-compatible fallback for APIs that omit controlPlane.
        return typeof node?.name === 'string' && node.name.includes('-cp-');
    };

    // iterate over nodePools and match nodeSets to nodePools, nodes to nodeSets
    for (let i = 0; i < nodePools.length; i++) {
        nodePools[i].nodeSets = [];

        nodePools[i].totalNodeCount = 0;
        nodePools[i].pendingNodeCount = 0;

        for (const nodeSet of nodeSets) {
            if (nodeSet.nodePoolID === nodePools[i].id) {
                if (nodePools[i].currentNodeSetID === nodeSet.id) {
                    nodeSet.isCurrent = true;
                }

                nodeSet.nodes = [];
                nodeSet.pendingNodeCount = 0;
                nodeSet.oldestNodeTimestamp = null;
                for (const node of nodes) {
                    if (node.nodeSetID === nodeSet.id) {
                        nodeSet.nodes.push(node);
                        nodePools[i].totalNodeCount++;
                        if (node?.conditions?.pending?.status === "True") {
                            nodePools[i].pendingNodeCount++;
                            nodeSet.pendingNodeCount++;
                        }

                        // track oldest node provision time for nodeset sorting purposes
                        let provisionTime = null;
                        if (node.conditions.provisioned && node.conditions.provisioned.lastTransitionTime) {
                            provisionTime = node.conditions.provisioned.lastTransitionTime;
                        }

                        if (nodeSet.oldestNodeTimestamp === null || provisionTime < nodeSet.oldestNodeTimestamp) {
                            nodeSet.oldestNodeTimestamp = provisionTime;
                        }
                    }
                }
                nodeSet.actualNodeCount = nodeSet.nodes.length - nodeSet.pendingNodeCount;
                nodePools[i].nodeSets.push(nodeSet);
            }
        }

        nodePools[i].actualNodeCount = nodePools[i].totalNodeCount - nodePools[i].pendingNodeCount;

    }

    // Sort node pools: first by control plane (control plane first), then by nodeNamePrefix
    nodePools.sort((a, b) => {
        // First sort by control plane status
        if (a.controlPlane && !b.controlPlane) return -1;
        if (!a.controlPlane && b.controlPlane) return 1;

        // Then sort by nodeNamePrefix alphabetically
        return a.nodeNamePrefix.localeCompare(b.nodeNamePrefix);
    });

    // sort nodes in each nodeSet by name
    for (const nodePool of nodePools) {

        // sort the current node set first, then by oldest node provision time (descending)
        nodePool.nodeSets.sort((a, b) => {
            if (a.isCurrent) return -1;
            if (b.isCurrent) return 1;
            if (b.oldestNodeTimestamp === null) return -1;
            if (a.oldestNodeTimestamp === null) return 1;
            return b.oldestNodeTimestamp.localeCompare(a.oldestNodeTimestamp);
        })

        for (const nodeSet of nodePool.nodeSets) {
            nodeSet.nodes.sort((a, b) => a.name.localeCompare(b.name));
        }
    }



    const clusterControlPlaneNodes = nodes
        .filter(node => isControlPlaneNode(node) && isNilUUID(node.nodePoolID) && isNilUUID(node.nodeSetID))
        .sort((a, b) => a.name.localeCompare(b.name));

    const nodeCounts = computeNodeCounts(nodes, nodePools, clusterControlPlaneNodes);

    return {
        nodePools,
        clusterControlPlaneNodes,
        nodeCounts,
    };
}

async function getOSProfiles() {
    const response = await fetch(`${API_BASE_URL}/osProfiles`);

    if (!response.ok) {
        throw new Error(await parseErrorResponse(response));
    }

    const body = await response.json();
    return asArray(body.osProfiles);
}

async function getConfigProfiles() {
    const response = await fetch(`${API_BASE_URL}/configProfiles`);

    if (!response.ok) {
        throw new Error(await parseErrorResponse(response));
    }

    const body = await response.json();
    return asArray(body.configProfiles);
}

async function getForgeTenants() {
    const response = await fetch(`${API_BASE_URL}/forge/tenants`);

    if (!response.ok) {
        throw new Error(await parseErrorResponse(response));
    }

    const body = await response.json();
    return asArray(body.tenants);
}

async function getForgeSites(orgID) {
    const response = await fetch(`${API_BASE_URL}/forge/sites?orgID=${orgID}`);

    if (!response.ok) {
        throw new Error(await parseErrorResponse(response));
    }

    const body = await response.json();
    return asArray(body.sites);
}

async function getForgeVPCs(orgID, siteID) {
    const response = await fetch(`${API_BASE_URL}/forge/vpcs?orgID=${orgID}&siteID=${siteID}`);

    if (!response.ok) {
        throw new Error(await parseErrorResponse(response));
    }

    const body = await response.json();
    return asArray(body.vpcs);
}

async function getForgeSubnets(orgID, siteID, vpcID) {
    const response = await fetch(`${API_BASE_URL}/forge/subnets?orgID=${orgID}&siteID=${siteID}&vpcID=${vpcID}`);

    if (!response.ok) {
        throw new Error(await parseErrorResponse(response));
    }

    const body = await response.json();
    return asArray(body.subnets);
}

async function getForgeVpcPrefixes(orgID, siteID, vpcID) {
    const response = await fetch(`${API_BASE_URL}/forge/vpcPrefixes?orgID=${orgID}&siteID=${siteID}&vpcID=${vpcID}`);

    if (!response.ok) {
        throw new Error(await parseErrorResponse(response));
    }

    const body = await response.json();
    return asArray(body.vpcPrefixes);
}

async function getForgeInstanceTypes(orgID, siteID) {
    const response = await fetch(`${API_BASE_URL}/forge/instanceTypes?orgID=${orgID}&siteID=${siteID}`);

    if (!response.ok) {
        throw new Error(await parseErrorResponse(response));
    }

    const body = await response.json();
    return asArray(body.instanceTypes);
}

async function getForgeInfiniBandPartitions(orgID, siteID) {
    const response = await fetch(`${API_BASE_URL}/forge/infiniBandPartitions?orgID=${orgID}&siteID=${siteID}`);

    if (!response.ok) {
        throw new Error(await parseErrorResponse(response));
    }

    const body = await response.json();
    return asArray(body.infiniBandPartitions);
}

async function getForgeNVLinkLogicalPartitions(orgID, siteID) {
    const response = await fetch(`${API_BASE_URL}/forge/nvlinkLogicalPartitions?orgID=${orgID}&siteID=${siteID}`);

    if (!response.ok) {
        throw new Error(await parseErrorResponse(response));
    }

    const body = await response.json();
    return asArray(body.nvLinkLogicalPartitions);
}

async function getForgeSSHKeyGroups(orgID, siteID) {
    const response = await fetch(`${API_BASE_URL}/forge/sshKeyGroups?orgID=${orgID}&siteID=${siteID}`);

    if (!response.ok) {
        throw new Error(await parseErrorResponse(response));
    }

    const body = await response.json();
    return asArray(body.sshKeyGroups);
}

async function getForgeInstances(orgID, siteID, vpcID) {
    const response = await fetch(`${API_BASE_URL}/forge/instances?orgID=${orgID}&siteID=${siteID}&vpcID=${vpcID}`);

    if (!response.ok) {
        throw new Error(await parseErrorResponse(response));
    }

    const body = await response.json();
    return asArray(body.instances);
}

/**
 * POST /api/clusters. `clusterData` is passed through as the request body. In
 * cpControllerMode it additionally carries a top-level `controlPlaneConfig`
 * (CreateClusterControlPlaneConfig) and no control-plane node pool; in legacy mode the
 * control plane is a node pool with controlPlane: true and no controlPlaneConfig is sent.
 * @param {Object & { controlPlaneConfig?: CreateClusterControlPlaneConfig }} clusterData
 */
async function createCluster(clusterData) {
    const response = await fetch(`${API_BASE_URL}/clusters`, {
        method: 'POST',
        headers: {
            'Content-Type': 'application/json'
        },
        body: JSON.stringify(clusterData)
    });

    if (!response.ok) {
        throw new Error(await parseErrorResponse(response));
    }

    const newCluster = await response.json();
    return newCluster;
}

async function createNodePool(nodePoolData) {
    const response = await fetch(`${API_BASE_URL}/nodePools`, {
        method: 'POST',
        headers: {
            'Content-Type': 'application/json'
        },
        body: JSON.stringify(nodePoolData)
    });

    if (!response.ok) {
        throw new Error(await parseErrorResponse(response));
    }

    const newNodePool = await response.json();
    return newNodePool;
}

async function deleteCluster(clusterId) {
    const response = await fetch(`${API_BASE_URL}/clusters/${clusterId}`, {
        method: 'DELETE'
    });

    if (!response.ok) {
        throw new Error(await parseErrorResponse(response));
    }
}

/**
 * PATCH /api/clusters/{id}. `updates` is passed through as the request body. Its optional
 * `controlPlaneConfig` (UpdateClusterControlPlaneConfig) carries only the changed
 * cluster-level control-plane fields; expanded spec fields are only populated in
 * cpControllerMode, preserving legacy behavior otherwise.
 * @param {string} clusterId
 * @param {Object & { controlPlaneConfig?: UpdateClusterControlPlaneConfig }} updates
 */
async function updateCluster(clusterId, updates) {
    const response = await fetch(`${API_BASE_URL}/clusters/${clusterId}`, {
        method: 'PATCH',
        headers: {
            'Content-Type': 'application/json'
        },
        body: JSON.stringify(updates)
    });

    if (!response.ok) {
        throw new Error(await parseErrorResponse(response));
    }

    return await response.json();
}

async function deleteNodePool(nodePoolId) {
    const response = await fetch(`${API_BASE_URL}/nodePools/${nodePoolId}`, {
        method: 'DELETE'
    });

    if (!response.ok) {
        throw new Error(await parseErrorResponse(response));
    }
}

async function deleteNode(nodeId, requestBody = null) {
    const options = {
        method: 'DELETE'
    };
    
    if (requestBody) {
        options.headers = {
            'Content-Type': 'application/json'
        };
        options.body = JSON.stringify(requestBody);
    }
    
    const response = await fetch(`${API_BASE_URL}/nodes/${nodeId}`, options);

    if (!response.ok) {
        throw new Error(await parseErrorResponse(response));
    }
}

async function getNodeHistory(nodeId) {
    const response = await fetch(`${API_BASE_URL}/nodes/${nodeId}/history`);
    if (!response.ok) {
        throw new Error(await parseErrorResponse(response));
    }
    return await response.json();
}

async function parseErrorResponse(response) {
    const maxLength = 200;
    try {
        const errorData = await response.json();
        if (errorData.message) {
            return errorData.message;
        }
        // response was json, but no message, so fall back to showing raw JSON
        const errorMessage = truncateWithEllipsis(JSON.stringify(errorData), maxLength);
        return response.status + ' ' + response.statusText + ': ' + errorMessage;
    } catch (error) {
        // couldn't get the response body as json
    }

    try {
        const body = await response.text();
        const bodyMessage = truncateWithEllipsis(body, maxLength);
        return response.status + ' ' + response.statusText + ': ' + bodyMessage;
    } catch (error) {
        // couldn't get the response body as text
    }

    return response.status + ' ' + response.statusText;
}

function truncateWithEllipsis(str, maxLength) {
    if (str.length > maxLength) {
        return str.substring(0, maxLength) + '...';
    }
    return str;
}

async function getCNIPlugins() {
    const response = await fetch(`${API_BASE_URL}/cniPlugins`);

    if (!response.ok) {
        throw new Error(await parseErrorResponse(response));
    }

    const body = await response.json();
    return asArray(body.cniPlugins);
}

async function getSites() {
    const response = await fetch(`${API_BASE_URL}/sites`);

    if (!response.ok) {
        throw new Error(await parseErrorResponse(response));
    }

    const body = await response.json();
    return asArray(body.sites);
}

async function createSite(siteData) {
    const response = await fetch(`${API_BASE_URL}/sites`, {
        method: 'POST',
        headers: {
            'Content-Type': 'application/json'
        },
        body: JSON.stringify(siteData)
    });

    if (!response.ok) {
        throw new Error(await parseErrorResponse(response));
    }

    const newSite = await response.json();
    return newSite;
}

async function createVIPAllocation(vipAllocationData) {
    const response = await fetch(`${API_BASE_URL}/vipAllocations`, {
        method: 'POST',
        headers: {
            'Content-Type': 'application/json'
        },
        body: JSON.stringify(vipAllocationData)
    });

    if (!response.ok) {
        throw new Error(await parseErrorResponse(response));
    }

    const newVIPAllocation = await response.json();
    return newVIPAllocation;
}

async function getNodePool(nodePoolID) {
    const response = await fetch(`/api/nodePools/${nodePoolID}`);
    if (!response.ok) {
        const errorData = await response.json().catch(() => ({ message: response.statusText }));
        throw new Error(errorData.message || `HTTP error! status: ${response.status}`);
    }
    return await response.json();
}

async function updateNodePool(nodePoolID, payload) {
    const response = await fetch(`/api/nodePools/${nodePoolID}`, {
        method: 'PATCH',
        headers: {
            'Content-Type': 'application/json',
        },
        body: JSON.stringify(payload),
    });
    if (!response.ok) {
        const errorData = await response.json().catch(() => ({ message: response.statusText }));
        throw new Error(errorData.message || `HTTP error! status: ${response.status}`);
    }
    
    return await response.json();
}

async function getNodeSets(nodePoolID = null) {
    const params = new URLSearchParams();
    if (nodePoolID) {
        params.append('nodePoolID', nodePoolID);
    }
    
    const url = `${API_BASE_URL}/nodeSets${params.toString() ? '?' + params.toString() : ''}`;
    const response = await fetch(url);
    
    if (!response.ok) {
        throw new Error(await parseErrorResponse(response));
    }
    
    return await response.json();
}

async function getNodeSet(nodeSetID) {
    const response = await fetch(`${API_BASE_URL}/nodeSets/${nodeSetID}`);

    if (!response.ok) {
        throw new Error(await parseErrorResponse(response));
    }

    return await response.json();
}

async function getNodePoolConfigHistory(nodePoolID) {
    const response = await fetch(`${API_BASE_URL}/nodePools/${nodePoolID}/configHistory`);

    if (!response.ok) {
        throw new Error(await parseErrorResponse(response));
    }

    return await response.json();
}
