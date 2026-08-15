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

const state = {
    // Static data
    osProfiles: [],
    configProfiles: [],
    cniPlugins: [],
    tenants: [],

    // Pre-fetched on page load
    forgeSites: [],
    registeredSites: [],
    clusters: [],
    
    // Selected cluster state
    selectedClusterID: null,
    selectedClusterDetails: null,
    selectedClusterNodePools: null,
    selectedClusterControlPlaneNodes: [],
    selectedClusterNodeCounts: null,
    
    // Cached forge data for selected cluster (for name resolution)
    selectedClusterForgeData: null,
    
    refreshIntervalId: null,
    expandedSites: new Set(),
    temporalUIURL: '',
    temporalNamespace: 'cloud-workflow-engine',

    // Server feature flag: when true, the cluster control plane is provisioned by the
    // control-plane controller via a cluster-level controlPlaneConfig (no control-plane
    // node pool). When false (default / legacy), the control plane is a node pool with
    // controlPlane: true. Sourced once from GET /api/version.
    cpControllerMode: false,
};

// === PAGE LOAD ===
async function loadInitialData() {
    try {
        // Load the core UI data in parallel. Forge tenant discovery is optional;
        // the UI should still work for default-site registration when Forge is unavailable.
        const [osProfiles, configProfiles, cniPlugins, tenantsResult] = await Promise.all([
            getOSProfiles(),
            getConfigProfiles(),
            getCNIPlugins(),
            getForgeTenants().catch((error) => {
                console.warn('Failed to fetch Forge tenants:', error);
                return [];
            })
        ]);
        
        state.osProfiles = osProfiles;
        state.configProfiles = configProfiles;
        state.cniPlugins = cniPlugins;
        state.tenants = tenantsResult;
        
        // Pre-fetch sites for each tenant from forge (for name lookup only)
        await loadForgeSites();
        
        // Load clusters and registered sites (for navigation)
        await loadClustersAndRegisteredSites();
        
    } catch (error) {
        console.error('Error loading initial data:', error);
    }
}

async function loadForgeSites() {
    if (!state.tenants || state.tenants.length === 0) {
        state.forgeSites = [];
        return;
    }
    
    const forgeSitePromises = state.tenants.map(async (tenant) => {
        try {
            const sites = await getForgeSites(tenant.orgID);
            // Attach the orgID and tenantID to each site for filtering
            return sites.map(site => ({
                ...site,
                orgID: tenant.orgID,
                tenantID: tenant.id
            }));
        } catch (error) {
            console.warn(`Failed to fetch Forge sites for tenant ${tenant.id}:`, error);
            return [];
        }
    });
    
    try {
        const forgeSiteArrays = await Promise.all(forgeSitePromises);
        const allForgeSites = forgeSiteArrays.flat();
        state.forgeSites = allForgeSites;
    } catch (error) {
        console.warn('Error fetching Forge sites:', error);
        state.forgeSites = [];
    }
}

async function loadClustersAndRegisteredSites() {
    try {
        const [sites, clusters] = await Promise.all([
            getSites(),
            getClusters()
        ]);
        
        state.registeredSites = sites;
        state.clusters = clusters;
        
        // Auto-expand all sites if no sites are currently expanded
        if (state.expandedSites.size === 0 && sites.length > 0) {
            sites.forEach(site => state.expandedSites.add(site.id));
        }
        
        // If selected cluster no longer exists, deselect
        if (state.selectedClusterID && !clusters.some(cluster => cluster.id === state.selectedClusterID)) {
            deselectCluster();
        }
        
        renderClusters();
    } catch (error) {
        state.registeredSites = [];
        state.clusters = [];
        renderClustersError(error.message);
    }
}

// === CLUSTER SELECTION ===
function deselectCluster() {
    clearRefreshInterval();

    state.selectedClusterID = null;
    state.selectedClusterDetails = null;
    state.selectedClusterNodePools = null;
    state.selectedClusterControlPlaneNodes = [];
    state.selectedClusterNodeCounts = null;
    state.selectedClusterForgeData = null;

    renderClusters();
    hideSelectedClusterSection();
}

async function selectCluster(clusterID) {
    clearRefreshInterval();

    state.selectedClusterID = clusterID;
    state.selectedClusterDetails = null;
    state.selectedClusterNodePools = null;
    state.selectedClusterControlPlaneNodes = [];
    state.selectedClusterNodeCounts = null;
    state.selectedClusterForgeData = null;

    renderClusters();
    renderClusterDetails();
    renderNodePools();
    showSelectedClusterSection();

    // Only load cluster details and node pools
    await loadClusterDetails();
    if (state.selectedClusterDetails) {
        await loadNodePools();
        await loadSelectedClusterForgeData();
        startRefreshInterval();
    }
}

async function loadClusterDetails() {
    try {
        const clusterID = state.selectedClusterID;
        const clusterDetails = await getClusterDetails(clusterID);
        
        // Check if same cluster still selected
        if (clusterID !== state.selectedClusterID) {
            return;
        }
        
        if (clusterDetails) {
            state.selectedClusterDetails = clusterDetails;
            renderClusterDetails();
        }
    } catch (error) {
        state.selectedClusterDetails = null;
        renderClusterDetailsError(error.message);
    }
}

async function loadNodePools() {
    try {
        const clusterID = state.selectedClusterID;
        const { nodePools, clusterControlPlaneNodes, nodeCounts } = parseNodePoolsResponse(await getNodePools(clusterID));

        // Check if same cluster still selected
        if (clusterID !== state.selectedClusterID) {
            return;
        }

        state.selectedClusterNodePools = nodePools;
        state.selectedClusterControlPlaneNodes = clusterControlPlaneNodes;
        state.selectedClusterNodeCounts = nodeCounts;
        renderNodePools();
        renderClusterDetails();
    } catch (error) {
        console.error('Error loading node pools:', error);
        state.selectedClusterNodePools = null;
        state.selectedClusterControlPlaneNodes = [];
        state.selectedClusterNodeCounts = null;
        renderClusterDetails();
        renderNodePoolsError(error.message);
    }
}

async function loadSelectedClusterForgeData() {
    if (!state.selectedClusterDetails) return;
    
    const cluster = state.selectedClusterDetails;
    if (!cluster.orgID || !cluster.siteID) return;
    
    try {
        const forgeData = await loadForgeData(cluster.orgID, cluster.siteID, cluster.vpcID);
        
        // Check if cluster still selected
        if (state.selectedClusterID && cluster.id === state.selectedClusterID) {
            state.selectedClusterForgeData = forgeData;
            // Re-render to show resolved names
            renderClusterDetails();
            renderNodePools();
        }
    } catch (error) {
        console.warn('Failed to load forge data for selected cluster:', error);
        state.selectedClusterForgeData = null;
    }
}

async function loadForgeData(orgID, siteID, vpcID = null) {
    const promises = [];
    
    if (orgID && siteID) {
        promises.push(
            getForgeInstanceTypes(orgID, siteID),
            getForgeSSHKeyGroups(orgID, siteID),
            getForgeInfiniBandPartitions(orgID, siteID),
            getForgeNVLinkLogicalPartitions(orgID, siteID),
            getForgeVPCs(orgID, siteID)
        );
        
        if (vpcID) {
            promises.push(
                getForgeSubnets(orgID, siteID, vpcID),
                getForgeVpcPrefixes(orgID, siteID, vpcID),
                getForgeInstances(orgID, siteID, vpcID)
            );
        }
    }
    
    try {
        const results = await Promise.allSettled(promises);
        const [instanceTypes, sshKeyGroups, infiniBandPartitions, nvLinkLogicalPartitions, vpcs, subnets, vpcPrefixes, instances] = results.map(r =>
            r.status === 'fulfilled' ? r.value : []
        );
        
        return {
            instanceTypes: instanceTypes || [],
            sshKeyGroups: sshKeyGroups || [],
            infiniBandPartitions: infiniBandPartitions || [],
            nvLinkLogicalPartitions: nvLinkLogicalPartitions || [],
            vpcs: vpcs || [],
            subnets: subnets || [],
            vpcPrefixes: vpcPrefixes || [],
            instances: instances || []
        };
    } catch (error) {
        console.error('Error loading forge data:', error);
        return {
            instanceTypes: [],
            sshKeyGroups: [],
            infiniBandPartitions: [],
            nvLinkLogicalPartitions: [],
            vpcs: [],
            subnets: [],
            vpcPrefixes: [],
            instances: []
        };
    }
}

// === AUTO-REFRESH ===
function startRefreshInterval() {
    if (!state.selectedClusterID) return;
    
    const REFRESH_INTERVAL = 5000; // 5 seconds
    
    const scheduleNextRefresh = () => {
        state.refreshIntervalId = setTimeout(async () => {
            if (!state.selectedClusterID) {
                clearRefreshInterval();
                return;
            }
            
            await refreshData();
            
            // Schedule the next refresh only after this one completes
            if (state.selectedClusterID) {
                scheduleNextRefresh();
            }
        }, REFRESH_INTERVAL);
    };
    
    scheduleNextRefresh();
}

function clearRefreshInterval() {
    if (state.refreshIntervalId) {
        clearTimeout(state.refreshIntervalId);
        state.refreshIntervalId = null;
    }
}

async function refreshData() {
    try {
        const clusterID = state.selectedClusterID;
        
        // Refresh cluster details and node pools in parallel
        const [newClusterDetails, newNodePoolsRaw] = await Promise.all([
            getClusterDetails(clusterID),
            getNodePools(clusterID)
        ]);
        const { nodePools: newNodePools, clusterControlPlaneNodes: newClusterControlPlaneNodes, nodeCounts: newNodeCounts } =
            parseNodePoolsResponse(newNodePoolsRaw);

        // Confirm same cluster still selected
        if (clusterID !== state.selectedClusterID) {
            return;
        }

        // Check if cluster details changed
        if (state.selectedClusterDetails) {
            const wasEtcdEncryptionRunning = isEtcdEncryptionUpdateInProgress(state.selectedClusterDetails);
            const currentClusterDetails = JSON.stringify(state.selectedClusterDetails);
            const newClusterDetailsStr = JSON.stringify(newClusterDetails);
            if (currentClusterDetails !== newClusterDetailsStr) {
                state.selectedClusterDetails = newClusterDetails;
                renderClusterDetails();
                if (
                    wasEtcdEncryptionRunning &&
                    !isEtcdEncryptionUpdateInProgress(newClusterDetails) &&
                    newClusterDetails.etcdEncrypted
                ) {
                    showToast('Cluster was just upgraded to have etcd encryption enabled', 'success');
                }
            }
        }

        const currentClusterControlPlaneNodes = JSON.stringify(state.selectedClusterControlPlaneNodes || []);
        const newClusterControlPlaneNodesStr = JSON.stringify(newClusterControlPlaneNodes);
        if (currentClusterControlPlaneNodes !== newClusterControlPlaneNodesStr) {
            state.selectedClusterControlPlaneNodes = newClusterControlPlaneNodes;
            renderClusterDetails();
        }

        if (JSON.stringify(state.selectedClusterNodeCounts) !== JSON.stringify(newNodeCounts)) {
            state.selectedClusterNodeCounts = newNodeCounts;
            renderClusterDetails();
        }

        if (!Array.isArray(state.selectedClusterNodePools)) {
            return;
        }

        // Check if node pool IDs changed (need full re-render)
        const currentIDs = JSON.stringify(state.selectedClusterNodePools.map(np => np.id));
        const newIDs = JSON.stringify(newNodePools.map(np => np.id));
        if (currentIDs !== newIDs) {
            state.selectedClusterNodePools = newNodePools;
            renderNodePools();
            return;
        }

        // Check each node pool for changes
        for (let i = 0; i < newNodePools.length; i++) {
            const currentNodePool = state.selectedClusterNodePools[i];
            const newNodePool = newNodePools[i];

            // Check node pool details (without nodeSets)
            const currentDetails = JSON.stringify({ ...(currentNodePool || {}), nodeSets: undefined });
            const newDetails = JSON.stringify({ ...newNodePool, nodeSets: undefined });
            if (currentDetails !== newDetails) {
                state.selectedClusterNodePools[i] = newNodePool;
                renderNodePool(newNodePool);
                continue;
            }

            // Check node set IDs
            const currentNodeSets = Array.isArray(currentNodePool?.nodeSets) ? currentNodePool.nodeSets : [];
            const newNodeSets = Array.isArray(newNodePool?.nodeSets) ? newNodePool.nodeSets : [];
            const currentNodeSetIDs = JSON.stringify(currentNodeSets.map(nodeSet => nodeSet.id));
            const newNodeSetIDs = JSON.stringify(newNodeSets.map(nodeSet => nodeSet.id));
            if (currentNodeSetIDs !== newNodeSetIDs) {
                state.selectedClusterNodePools[i].nodeSets = newNodeSets;
                renderNodePool(newNodePool);
                continue;
            }

            // Check each node set for changes
            for (let j = 0; j < newNodeSets.length; j++) {
                const currentNodeSet = currentNodeSets[j];
                const newNodeSet = newNodeSets[j];

                // Check node set details (without nodes)
                const currentNodeSetDetails = JSON.stringify({ ...(currentNodeSet || {}), nodes: undefined });
                const newNodeSetDetails = JSON.stringify({ ...(newNodeSet || {}), nodes: undefined });
                if (currentNodeSetDetails !== newNodeSetDetails) {
                    state.selectedClusterNodePools[i].nodeSets[j] = newNodeSet;
                    renderNodeSet(newNodeSet);
                    continue;
                }

                // Check node changes
                const currentNodes = Array.isArray(currentNodeSet?.nodes) ? currentNodeSet.nodes : [];
                const newNodes = Array.isArray(newNodeSet?.nodes) ? newNodeSet.nodes : [];
                const currentNodeIDs = JSON.stringify(currentNodes.map(node => node.id));
                const newNodeIDs = JSON.stringify(newNodes.map(node => node.id));
                if (currentNodeIDs !== newNodeIDs) {
                    state.selectedClusterNodePools[i].nodeSets[j] = newNodeSet;
                    renderNodeSet(newNodeSet);
                    continue;
                }

                // Check individual node changes
                for (let k = 0; k < newNodes.length; k++) {
                    const currentNode = currentNodes[k];
                    const newNode = newNodes[k];

                    const currentNodeDetails = JSON.stringify(currentNode);
                    const newNodeDetails = JSON.stringify(newNode);
                    if (currentNodeDetails !== newNodeDetails) {
                        state.selectedClusterNodePools[i].nodeSets[j].nodes[k] = newNode;
                        renderNode(newNode);
                    }
                }
            }
        }
    } catch (error) {
        console.error('Error refreshing data:', error);
    }
}

// Helper function to calculate node pool counts (duplicate logic from view.js for comparison)
function calculateNodePoolCounts(nodePool) {
    if (!nodePool.nodes) return { total: 0, pending: 0, actual: 0 };
    
    const totalNodeCount = nodePool.nodes.length;
    const pendingNodeCount = nodePool.nodes.filter(node => {
        // Simple check for pending condition - duplicate the core logic
        return node.conditions.pending && node.conditions.pending.status === "True";
    }).length;
    const actualNodeCount = totalNodeCount - pendingNodeCount;
    
    return {
        total: totalNodeCount,
        pending: pendingNodeCount,
        actual: actualNodeCount
    };
}

async function loadInfo() {
    try {
        const info = await getVersion();
        const versionDisplay = document.getElementById('version-display');
        if (versionDisplay) {
            versionDisplay.textContent = info.version;
        }
        state.temporalUIURL = info.temporalUIURL || '';
        state.temporalNamespace = info.temporalNamespace || 'cloud-workflow-engine';
        state.cpControllerMode = info.provisionControlPlaneWithControlPlaneController === true;
    } catch (error) {
        console.warn('Failed to load server info:', error);
    }
}

window.onload = async () => {
    await loadInitialData();
    await loadInfo();
};
