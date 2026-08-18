/**
 * SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

// Helper function to find name by ID
const DEFAULT_SITE_ID = '00000000-0000-0000-0000-000000000000';
const DEFAULT_SITE_NAME = 'Default Site';
const DEFAULT_CLUSTER_POD_CIDR = '192.168.0.0/16';
const POD_CIDR_NODE_PREFIX_LENGTH = 24;
const MAX_POD_CIDR_PREFIX_LENGTH = POD_CIDR_NODE_PREFIX_LENGTH - 1;
const MINIMUM_POD_CIDR_HEADROOM = 1;
const SPIRE_ATTESTATION_METHOD_TPM = 'tpm';
const SPIRE_ATTESTATION_METHOD_JOIN_TOKEN = 'joinToken';
let editClusterInstanceTypePreferences = [];
let editClusterInstanceTypeDragIndex = null;

// Ordered control-plane instance-type preference list for the create-cluster form
// (cluster-level-CP / cpControllerMode).
let createClusterCPInstanceTypePreferences = [];
let createClusterCPInstanceTypeDragIndex = null;

function moveEditClusterInstanceTypePreference(fromIndex, toIndex) {
    if (fromIndex === toIndex || fromIndex < 0 || toIndex < 0) {
        return;
    }
    if (fromIndex >= editClusterInstanceTypePreferences.length || toIndex >= editClusterInstanceTypePreferences.length) {
        return;
    }
    const [item] = editClusterInstanceTypePreferences.splice(fromIndex, 1);
    editClusterInstanceTypePreferences.splice(toIndex, 0, item);
    renderEditClusterInstanceTypePreferenceList();
}

function moveEditClusterInstanceTypePreferenceUp(index) {
    if (index <= 0) {
        return;
    }
    moveEditClusterInstanceTypePreference(index, index - 1);
}

function moveEditClusterInstanceTypePreferenceDown(index) {
    if (index >= editClusterInstanceTypePreferences.length - 1) {
        return;
    }
    moveEditClusterInstanceTypePreference(index, index + 1);
}


function formatUTCDateTime(timestamp) {
    if (!timestamp) {
        return '';
    }
    const date = new Date(timestamp);
    if (Number.isNaN(date.getTime())) {
        return '';
    }
    return `${date.toISOString().slice(0, 19).replace('T', ' ')} UTC`;
}

function normalizeSpireAttestationMethod(method) {
    return method === SPIRE_ATTESTATION_METHOD_JOIN_TOKEN
        ? SPIRE_ATTESTATION_METHOD_JOIN_TOKEN
        : SPIRE_ATTESTATION_METHOD_TPM;
}

function formatSpireAttestationMethod(method) {
    return normalizeSpireAttestationMethod(method) === SPIRE_ATTESTATION_METHOD_JOIN_TOKEN
        ? 'Join Token'
        : 'TPM';
}

function getSpireAttestationMethodFromInput(input) {
    return normalizeSpireAttestationMethod(input?.value);
}

function formatManifestForDisplay(manifest) {
    if (manifest === null || manifest === undefined) {
        return '';
    }
    if (typeof manifest === 'string') {
        return manifest;
    }
    if (window.jsyaml && typeof window.jsyaml.dump === 'function') {
        try {
            return window.jsyaml.dump(manifest, {
                noRefs: true,
                lineWidth: -1,
                sortKeys: false,
            });
        } catch (error) {
            // Fall back to JSON below if YAML serialization fails.
        }
    }
    try {
        return JSON.stringify(manifest, null, 2);
    } catch (error) {
        return String(manifest);
    }
}

function showManifestModal(manifest, title = 'Manifest') {
    const modal = document.getElementById('manifest-view-modal');
    const titleElement = document.getElementById('manifest-view-title');
    const contentElement = document.getElementById('manifest-view-content');
    const copyButton = document.getElementById('manifest-view-copy');
    if (!modal || !titleElement || !contentElement) {
        return;
    }

    const formatted = formatManifestForDisplay(manifest);
    titleElement.textContent = title;
    contentElement.textContent = formatted;
    if (copyButton) {
        copyButton.textContent = 'Copy';
        copyButton.classList.remove('copied');
        copyButton.onclick = async () => {
            try {
                await copyToClipboard(formatted);
                copyButton.textContent = 'Copied ✓';
                copyButton.classList.add('copied');
                setTimeout(() => {
                    copyButton.textContent = 'Copy';
                    copyButton.classList.remove('copied');
                }, 1200);
            } catch (err) {
                copyButton.textContent = 'Copy failed';
                setTimeout(() => { copyButton.textContent = 'Copy'; }, 1500);
            }
        };
    }
    modal.style.display = 'block';
}

function createManifestValueElement(manifest, title = 'Manifest') {
    const button = document.createElement('button');
    button.type = 'button';
    button.className = 'add-btn manifest-view-btn';
    button.textContent = 'View';
    button.onclick = () => showManifestModal(manifest, title);
    return button;
}

function hasRenderableManifest(manifest) {
    if (manifest === null || manifest === undefined) {
        return false;
    }
    if (typeof manifest === 'string') {
        return manifest.trim() !== '';
    }
    return true;
}

function createEmptyManifestObject() {
    return {};
}

function getCreateClusterSiteID() {
    return document.getElementById('hidden-site-id')?.value || '';
}

function siteSupportsForgeProvisioning(siteID = getCreateClusterSiteID()) {
    return !!siteID && siteID !== DEFAULT_SITE_ID;
}

function createClusterUsesCAPIInfrastructure(siteID = getCreateClusterSiteID()) {
    return !siteSupportsForgeProvisioning(siteID);
}

function clusterUsesCAPIInfrastructure(cluster = state.selectedClusterDetails) {
    return !!cluster && !siteSupportsForgeProvisioning(cluster.siteID);
}

function updateClusterManifestRequirement() {
    const capiInfraClusterInput = document.getElementById('capi-infra-cluster-yaml');
    if (!capiInfraClusterInput) {
        return;
    }

    if (createClusterUsesCAPIInfrastructure()) {
        capiInfraClusterInput.setAttribute('required', 'required');
    } else {
        capiInfraClusterInput.removeAttribute('required');
    }
}

function applyNodePoolInfrastructureFields(container, isCAPICluster) {
    if (!container) {
        return;
    }

    applyForgeFieldVisibility(container, !isCAPICluster);
    applyCAPIFieldVisibility(container, isCAPICluster);
    applyOSProfileFieldVisibility(container, !isCAPICluster);
    applyConfigProfileFieldBehavior(container, isCAPICluster);

    const capiTemplateInput = container.querySelector('textarea[id$="capi-infra-machine-template-yaml"]');
    if (capiTemplateInput) {
        if (isCAPICluster) {
            capiTemplateInput.setAttribute('required', 'required');
        } else {
            capiTemplateInput.removeAttribute('required');
        }
    }

    updateClusterManifestRequirement();
}

function getNameById(id, collection, idField = 'id', nameField = 'name') {
    if (!collection || !id) return id;
    const item = collection.find(item => item[idField] === id);
    return item ? item[nameField] : id;
}

async function copyToClipboard(text) {
    if (navigator.clipboard && window.isSecureContext) {
        await navigator.clipboard.writeText(text);
        return;
    }
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    document.execCommand('copy');
    document.body.removeChild(ta);
}

function createCopyButton(text, label) {
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'copy-btn';
    btn.title = `Copy ${label || 'to clipboard'}`;
    btn.setAttribute('aria-label', btn.title);
    btn.textContent = '⧉';
    btn.addEventListener('click', async (e) => {
        e.stopPropagation();
        try {
            await copyToClipboard(text);
            const prev = btn.textContent;
            btn.textContent = '✓';
            btn.classList.add('copied');
            setTimeout(() => {
                btn.textContent = prev;
                btn.classList.remove('copied');
            }, 1000);
        } catch (err) {
            btn.title = `Copy failed: ${err.message}`;
        }
    });
    return btn;
}

function createCopyableValue(text, label) {
    const wrapper = document.createElement('span');
    wrapper.className = 'copyable-value';
    const textSpan = document.createElement('span');
    textSpan.textContent = text;
    wrapper.appendChild(textSpan);
    wrapper.appendChild(createCopyButton(text, label));
    return wrapper;
}

// === NAME RESOLUTION HELPERS ===
function resolveVpcName(vpcID) {
    // uuid.Nil in golang is '00000000-0000-0000-0000-000000000000'
    if (vpcID === '00000000-0000-0000-0000-000000000000') return ''; 

    if (!vpcID || !state.selectedClusterForgeData) return vpcID;
    
    const vpc = state.selectedClusterForgeData.vpcs.find(v => v.id === vpcID);
    return vpc ? vpc.name : vpcID;
}

function resolveSubnetName(subnetID) {
    if (!subnetID || !state.selectedClusterForgeData) return subnetID;
    
    const subnet = state.selectedClusterForgeData.subnets.find(s => s.id === subnetID);
    return subnet ? subnet.name : subnetID;
}

function resolveVpcPrefixName(vpcPrefixID) {
    if (!vpcPrefixID || !state.selectedClusterForgeData) return vpcPrefixID;
    
    const vpcPrefix = state.selectedClusterForgeData.vpcPrefixes?.find(vp => vp.id === vpcPrefixID);
    return vpcPrefix ? vpcPrefix.name : vpcPrefixID;
}

function resolveInstanceTypeName(instanceTypeID) {
    if (!instanceTypeID || !state.selectedClusterForgeData) return instanceTypeID;
    const instanceType = state.selectedClusterForgeData.instanceTypes.find(it => it.id === instanceTypeID);
    return instanceType ? instanceType.name : instanceTypeID;
}

function formatInstanceTypeAvailabilitySuffix(instanceType) {
    if (!instanceType || (instanceType.unusedUsable === undefined && instanceType.total === undefined)) {
        return '';
    }
    if (instanceType.unusedUsable !== undefined && instanceType.total !== undefined) {
        return ` (${instanceType.unusedUsable}/${instanceType.total} available)`;
    }
    if (instanceType.unusedUsable !== undefined) {
        return ` (${instanceType.unusedUsable} available)`;
    }
    if (instanceType.total !== undefined) {
        return ` (${instanceType.total} total)`;
    }
    return '';
}

function formatInstanceTypeDisplay(instanceTypeID) {
    if (!instanceTypeID || instanceTypeID === '00000000-0000-0000-0000-000000000000') {
        return '';
    }
    const instanceType = getInstanceTypeAvailability(instanceTypeID);
    return resolveInstanceTypeName(instanceTypeID) + formatInstanceTypeAvailabilitySuffix(instanceType);
}

function resolveSSHKeyGroupNames(sshKeyGroupIDs) {
    if (!sshKeyGroupIDs || sshKeyGroupIDs.length === 0 || !state.selectedClusterForgeData) {
        return Array.isArray(sshKeyGroupIDs) ? sshKeyGroupIDs.join(', ') : sshKeyGroupIDs;
    }
    
    const names = sshKeyGroupIDs.map(id => {
        const group = state.selectedClusterForgeData.sshKeyGroups.find(g => g.id === id);
        return group ? group.name : id;
    });
    return names.join(', ');
}

function resolveInfiniBandPartitionName(partitionID) {
    if (!partitionID || !state.selectedClusterForgeData) return partitionID || 'None';
    
    const partition = state.selectedClusterForgeData.infiniBandPartitions.find(p => p.id === partitionID);
    return partition ? partition.name : partitionID;
}

// === NAVIGATION RENDERING ===

function toggleSiteExpansion(siteID) {
    if (state.expandedSites.has(siteID)) {
        state.expandedSites.delete(siteID);
    } else {
        state.expandedSites.add(siteID);
    }
    renderClusters();
}

function getSiteDisplayName(site) {
    if (site?.id === DEFAULT_SITE_ID) {
        return DEFAULT_SITE_NAME;
    }

    // Try to find the site name from Forge sites
    if (state.forgeSites && state.forgeSites.length > 0) {
        const forgeSite = state.forgeSites.find(fs => fs.id === site.id);
        if (forgeSite && forgeSite.name) {
            return forgeSite.name;
        }
    }
    
    // Fallback to site ID if no name found
    return site.id;
}

function formatSelectedSiteDisplay(siteID) {
    const siteName = getSiteDisplayName({ id: siteID });
    if (siteID === DEFAULT_SITE_ID || siteName === siteID) {
        return siteName;
    }

    return `${siteName} (${siteID})`;
}

function renderClusters() {
    const clustersList = document.getElementById('clusters-list');
    clustersList.innerHTML = '';

    if (state.registeredSites.length === 0) {
        clustersList.innerHTML = '<p>No sites found</p>';
        document.getElementById('right-side-container').style.display = 'none';
        return;
    }

    state.registeredSites.forEach(site => {
        // Get clusters for this site
        const siteClusters = state.clusters.filter(cluster => cluster.siteID === site.id);
        
        // Create site container
        const siteContainer = document.createElement('div');
        siteContainer.className = 'nodepool-container';
        
        // Create site header
        const siteHeader = document.createElement('div');
        siteHeader.className = 'nodepool-header';
        
        const siteTitle = document.createElement('div');
        siteTitle.className = 'nodepool-title';
        
        // Expansion arrow
        const expandIcon = document.createElement('span');
        expandIcon.className = 'site-expansion-icon';
        expandIcon.textContent = state.expandedSites.has(site.id) ? '▼' : '▶';
        expandIcon.onclick = () => toggleSiteExpansion(site.id);
        
        const siteName = document.createElement('span');
        siteName.className = 'site-name';
        siteName.textContent = getSiteDisplayName(site);
        siteName.onclick = () => toggleSiteExpansion(site.id);
        
        siteTitle.appendChild(expandIcon);
        siteTitle.appendChild(siteName);
        
        // Site actions
        const siteActions = document.createElement('div');
        siteActions.className = 'nodepool-header-actions';
        
        const addClusterBtn = document.createElement('button');
        addClusterBtn.className = 'add-btn';
        addClusterBtn.textContent = 'Add Cluster';
        addClusterBtn.title = 'Add Cluster';
        addClusterBtn.onclick = () => showCreateClusterModal(site.id);
        
        siteActions.appendChild(addClusterBtn);
        
        siteHeader.appendChild(siteTitle);
        siteHeader.appendChild(siteActions);
        siteContainer.appendChild(siteHeader);
        
        // Clusters container (only show if expanded)
        if (state.expandedSites.has(site.id)) {
            const clustersContainer = document.createElement('div');
            clustersContainer.className = 'nodes-container';
            
            if (siteClusters.length === 0) {
                const emptyState = document.createElement('div');
                emptyState.className = 'empty-state';
                emptyState.innerHTML = `<p>No clusters in this site</p>`;
                clustersContainer.appendChild(emptyState);
            } else {
                // Add clusters
                siteClusters.forEach(cluster => {
                    const clusterItem = document.createElement('div');
                    clusterItem.className = 'item';
                    if (state.selectedClusterID === cluster.id) {
                        clusterItem.classList.add('selected');
                    }

                    const itemContent = document.createElement('div');
                    itemContent.className = 'item-content';

                    const nameSpan = document.createElement('span');
                    nameSpan.textContent = cluster.name;
                    itemContent.appendChild(nameSpan);

                    clusterItem.appendChild(itemContent);
                    clusterItem.onclick = () => selectCluster(cluster.id);
                    clustersContainer.appendChild(clusterItem);
                });
            }
            
            siteContainer.appendChild(clustersContainer);
        }
        
        clustersList.appendChild(siteContainer);
    });
}

function renderClustersError(error) {
    const clustersList = document.getElementById('clusters-list');
    clustersList.innerHTML = `<p class="error">Error loading sites and clusters: ${error}</p>`;
}

// === CLUSTER AND NODE POOL RENDERING ===

function clusterAllowsForgeNodePools(cluster = state.selectedClusterDetails) {
    return !!cluster && !clusterUsesCAPIInfrastructure(cluster) && !!cluster.siteID && cluster.siteID !== DEFAULT_SITE_ID;
}

function nodePoolUsesCAPI(nodePool) {
    return hasRenderableManifest(nodePool?.capiInfraMachineTemplate);
}

function nodeSetUsesCAPI(nodeSet) {
    return hasRenderableManifest(nodeSet?.capiInfraMachineTemplate);
}

function formatNodeCount(counts) {
    if (!counts) {
        return 'Loading…';
    }

    return String(counts.total);
}

function formatNodeCountTitle(counts) {
    if (!counts) {
        return 'Node counts have not loaded yet';
    }

    const statusLine = (label, bucket) =>
        `${label}: ${bucket.provisioned} provisioned, ` +
        `${bucket.inProgress} in progress, ${bucket.pending} pending`;

    const lines = [
        `${counts.total} total — ${counts.controlPlane.total} control plane, ${counts.worker.total} workers`,
        statusLine('control plane', counts.controlPlane),
        statusLine('workers', counts.worker),
    ];
    if (counts.unassigned > 0) {
        lines.push(`unassigned: ${counts.unassigned}`);
    }

    return lines.join('\n');
}

function renderClusterDetails() {
    const clusterDetails = document.getElementById('cluster-details');
    clusterDetails.innerHTML = '';

    if (!state.selectedClusterDetails) {
        clusterDetails.innerHTML = '<p class="loading">Loading cluster details...</p>';
        return;
    }

    const details = state.selectedClusterDetails;
    const orgName = getNameById(details.orgID, state.tenants, 'orgID', 'name');
    const siteName = getSiteDisplayName({ id: details.siteID });
    const forgeNodePoolsEnabled = clusterAllowsForgeNodePools(details);

    const detailFields = [
        { label: 'Name', value: details.name, copyable: true },
        { label: 'ID', value: details.id, copyable: true },
        { label: 'Created at', value: formatUTCDateTime(details.createdAt) },
        ...(forgeNodePoolsEnabled ? [
            { label: 'Organization', value: orgName },
            { label: 'Site', value: siteName },
            { label: 'VPC', value: resolveVpcName(details.vpcID) },
        ] : []),
        { label: 'Kubernetes Version', value: details.controlPlaneKubernetesVersion },
        {
            label: 'Nodes',
            value: formatNodeCount(state.selectedClusterNodeCounts),
            title: formatNodeCountTitle(state.selectedClusterNodeCounts),
        },
        { label: 'CP Node Recycling', value: details.enableCPNodeRecycling ? 'Enabled' : 'Disabled' },
        { label: 'Control Plane VIP', value: details.controlPlaneVIP, copyable: true },
        { label: 'CNI Plugin', value: details.cniPlugin },
        { label: 'CNI Version', value: details.cniVersion },
        { label: 'Pod CIDR', value: details.network?.podCIDR, copyable: true },
        { label: 'Service CIDR', value: details.network?.serviceCIDR, copyable: true },
        {
            label: 'Etcd Encryption',
            value: isEtcdEncryptionUpdateInProgress(details) ? 'Enabling' : details.etcdEncrypted ? 'Enabled' : 'Disabled',
        },
        { label: 'Teleport Enabled', value: details.teleportEnabled ? 'Yes' : 'No' },
        ...(details.controlPlaneConfig ? [
            ...(details.controlPlaneConfig.desiredCount !== undefined ? [
                { label: 'Desired CP Count', value: details.controlPlaneConfig.desiredCount },
            ] : []),
            { label: 'Cluster Max Surge', value: details.controlPlaneConfig.maxSurge ?? 0 },
            { label: 'Control Plane Provisioning Monitor', value: details.controlPlaneConfig.provisioningMonitorDisabled ? 'Paused' : 'Active' },
            ...(details.controlPlaneConfig.teleportEnabled !== undefined ? [
                { label: 'Control Plane Teleport', value: details.controlPlaneConfig.teleportEnabled ? 'Enabled' : 'Disabled' },
            ] : []),
            ...(details.controlPlaneConfig.systemReservedMemory ? [
                { label: 'Control Plane System Reserved Memory', value: details.controlPlaneConfig.systemReservedMemory },
            ] : []),
            ...(Array.isArray(details.controlPlaneConfig.nodeLabels) && details.controlPlaneConfig.nodeLabels.length > 0 ? [
                { label: 'Control Plane Node Labels', value: details.controlPlaneConfig.nodeLabels.join(', ') },
            ] : []),
        ] : []),
        ...(forgeNodePoolsEnabled && details.controlPlaneConfig ? [
            {
                label: 'Control Plane Instance Types',
                instanceTypePreference: details.controlPlaneConfig.instanceTypePreference,
            },
            ...(details.controlPlaneConfig.osProfile ? [
                { label: 'Control Plane OS Profile', value: details.controlPlaneConfig.osProfile },
            ] : []),
            ...(details.controlPlaneConfig.configProfile ? [
                { label: 'Control Plane Config Profile', value: details.controlPlaneConfig.configProfile },
            ] : []),
            ...(Array.isArray(details.controlPlaneConfig.subnetIDs) && details.controlPlaneConfig.subnetIDs.length > 0 ? [
                { label: 'Control Plane Subnets', value: details.controlPlaneConfig.subnetIDs.map(resolveSubnetName).join(', ') },
            ] : []),
            ...(Array.isArray(details.controlPlaneConfig.vpcPrefixIDs) && details.controlPlaneConfig.vpcPrefixIDs.length > 0 ? [
                { label: 'Control Plane VPC Prefixes', value: details.controlPlaneConfig.vpcPrefixIDs.map(resolveVpcPrefixName).join(', ') },
            ] : []),
            ...(Array.isArray(details.controlPlaneConfig.sshKeyGroupIDs) && details.controlPlaneConfig.sshKeyGroupIDs.length > 0 ? [
                { label: 'Control Plane SSH Key Groups', value: resolveSSHKeyGroupNames(details.controlPlaneConfig.sshKeyGroupIDs) },
            ] : []),
        ] : []),
        ...(details.controlPlaneConfig ? [
            ...(hasRenderableManifest(details.controlPlaneConfig.capiInfraMachineTemplate) ? [
                { label: 'Control Plane Infra Machine Template', manifest: details.controlPlaneConfig.capiInfraMachineTemplate },
            ] : []),
            ...(details.controlPlaneConfig.capiSpireAttestationMethod ? [
                { label: 'Control Plane SPIRE Attestation Method', value: formatSpireAttestationMethod(details.controlPlaneConfig.capiSpireAttestationMethod) },
            ] : []),
            ...(Array.isArray(details.controlPlaneConfig.capiBootstrapFiles) && details.controlPlaneConfig.capiBootstrapFiles.length > 0 ? [
                { label: 'Control Plane Bootstrap Files', manifest: details.controlPlaneConfig.capiBootstrapFiles },
            ] : []),
            ...(Array.isArray(details.controlPlaneConfig.capiPreKubeadmCommands) && details.controlPlaneConfig.capiPreKubeadmCommands.length > 0 ? [
                { label: 'Control Plane Pre-Kubeadm Commands', manifest: details.controlPlaneConfig.capiPreKubeadmCommands },
            ] : []),
        ] : []),
        ...(hasRenderableManifest(details.capiInfraCluster) ? [
            { label: 'Infra Cluster', manifest: details.capiInfraCluster },
        ] : []),
    ];

    detailFields.forEach((field) => {
        if (field.value !== undefined || field.element !== undefined || field.manifest !== undefined || field.instanceTypePreference !== undefined) {
            const detailRow = document.createElement('div');
            detailRow.className = 'detail-row';
            if (field.title !== undefined) {
                detailRow.title = field.title;
            }

            const detailLabel = document.createElement('div');
            detailLabel.className = 'detail-label';
            detailLabel.textContent = field.label + ':';

            const detailValue = document.createElement('div');
            detailValue.className = 'detail-value';
            if (field.title !== undefined) {
                detailValue.classList.add('has-tooltip');
            }
            if (field.element !== undefined) {
                detailValue.appendChild(field.element);
            } else if (field.manifest !== undefined) {
                detailValue.appendChild(createManifestValueElement(field.manifest, field.label));
            } else if (field.instanceTypePreference !== undefined) {
                detailValue.appendChild(createInstanceTypePreferenceListElement(field.instanceTypePreference));
            } else if (field.copyable && field.value) {
                detailValue.appendChild(createCopyableValue(field.value, field.label));
            } else {
                detailValue.textContent = field.value;
            }

            detailRow.appendChild(detailLabel);
            detailRow.appendChild(detailValue);
            clusterDetails.appendChild(detailRow);
        }
    });

    const condition = getHighestPriorityCondition(details.conditions);
    if (condition) {
        const conditionContainer = document.createElement('div');
        conditionContainer.className = 'detail-row';

        const conditionLabel = document.createElement('div');
        conditionLabel.className = 'detail-label';
        conditionLabel.textContent = 'Status:';

        const conditionValue = document.createElement('div');
        conditionValue.className = 'detail-value';

        const conditionBadge = document.createElement('span');
        conditionBadge.className = `condition condition-${condition.type.toLowerCase().replace(/\s+/g, '-')}`;
        conditionBadge.textContent = condition.type;
        conditionBadge.title = condition.message || condition.type;

        conditionValue.appendChild(conditionBadge);

        if (condition.message) {
            const messageSpan = document.createElement('span');
            messageSpan.className = 'condition-message';
            messageSpan.textContent = condition.message;
            conditionValue.appendChild(messageSpan);
        }

        conditionContainer.appendChild(conditionLabel);
        conditionContainer.appendChild(conditionValue);
        clusterDetails.appendChild(conditionContainer);
    }

    renderClusterControlPlaneNodes(clusterDetails, state.selectedClusterControlPlaneNodes || []);
}

function isEtcdEncryptionUpdateInProgress(cluster) {
    return cluster?.conditions?.updateInProgress?.type === 'etcd_encryption';
}

function renderClusterDetailsError(error) {
    const clusterDetails = document.getElementById('cluster-details');
    clusterDetails.innerHTML = `<p class="error">Error loading cluster details: ${error}</p>`;
}

function renderNodePools() {
    const nodePoolsList = document.getElementById('nodepools-list');
    nodePoolsList.innerHTML = '';

    if (!Array.isArray(state.selectedClusterNodePools)) {
        nodePoolsList.innerHTML = '<p class="loading">Loading node pools and nodes ...</p>';
        return;
    }

    if (state.selectedClusterNodePools.length === 0) {
        const emptyMessage = document.createElement('p');
        emptyMessage.textContent = 'No node pools found for this cluster';
        nodePoolsList.appendChild(emptyMessage);
        return;
    }

    state.selectedClusterNodePools.forEach(nodePool => {
        const nodePoolContainer = document.createElement('div');
        nodePoolContainer.className = 'nodepool-container';
        nodePoolContainer.id = `nodepool-${nodePool.id}`;
        nodePoolsList.appendChild(nodePoolContainer);
        renderNodePool(nodePool);
    });
}

function renderNodePoolsError(error) {
    const nodePoolsList = document.getElementById('nodepools-list');
    nodePoolsList.innerHTML = `<p class="error">Error loading node pools: ${error}</p>`;
}

function renderClusterControlPlaneNodes(parentContainer, nodes) {
    if (!nodes || nodes.length === 0) {
        return;
    }

    const container = document.createElement('div');
    container.className = 'nodepool-container';
    container.id = 'cluster-control-plane-nodes';
    parentContainer.appendChild(container);

    const header = document.createElement('div');
    header.className = 'nodepool-header';

    const title = document.createElement('span');
    title.className = 'nodepool-title';

    const countBadge = document.createElement('span');
    countBadge.className = 'badge count-badge';
    countBadge.textContent = `${nodes.length} nodes`;
    countBadge.title = 'Cluster level control plane nodes (no node pool / no node set)';
    title.appendChild(countBadge);

    const label = document.createElement('span');
    label.textContent = 'Cluster level control plane nodes';
    title.appendChild(label);

    header.appendChild(title);
    container.appendChild(header);

    const nodesContainer = document.createElement('div');
    nodesContainer.className = 'nodes-container';
    container.appendChild(nodesContainer);

    const table = document.createElement('table');
    table.className = 'node-table';

    const thead = document.createElement('thead');
    const headerRow = document.createElement('tr');

    const nameHeader = document.createElement('th');
    nameHeader.textContent = 'Node Name';
    const instanceTypeHeader = document.createElement('th');
    instanceTypeHeader.textContent = 'Instance Type';
    const ipHeader = document.createElement('th');
    ipHeader.textContent = 'IP Address';
    const conditionsHeader = document.createElement('th');
    conditionsHeader.textContent = 'Conditions';
    conditionsHeader.className = 'conditions-cell';
    const actionsHeader = document.createElement('th');
    actionsHeader.className = 'actions-cell';
    actionsHeader.textContent = 'Actions';

    headerRow.appendChild(nameHeader);
    headerRow.appendChild(instanceTypeHeader);
    headerRow.appendChild(ipHeader);
    headerRow.appendChild(conditionsHeader);
    headerRow.appendChild(actionsHeader);
    thead.appendChild(headerRow);
    table.appendChild(thead);

    const tbody = document.createElement('tbody');
    table.appendChild(tbody);
    nodesContainer.appendChild(table);

    nodes.forEach(node => {
        const row = document.createElement('tr');
        row.id = `node-${node.id}`;
        row.setAttribute('data-node-id', node.id);
        tbody.appendChild(row);
        renderNode(node, { includeInstanceType: true });
    });

}

function renderNodePool(nodePool) {
    const nodePoolContainer = document.getElementById(`nodepool-${nodePool.id}`);
    if (!nodePoolContainer) {
        return;
    }

    nodePoolContainer.innerHTML = '';

    const nodePoolHeader = document.createElement('div');
    nodePoolHeader.className = 'nodepool-header';

    const nodePoolTitle = document.createElement('span');
    nodePoolTitle.className = 'nodepool-title';
    nodePoolTitle.textContent = '';
    
    // Current/Desired badge (using actual count excluding pending)
    const countBadge = document.createElement('span');
    countBadge.className = 'badge count-badge';
    countBadge.textContent = `${nodePool.actualNodeCount}/${nodePool.desiredNodeCount} nodes`;
    countBadge.title = `Desired: ${nodePool.desiredNodeCount}, Actual: ${nodePool.actualNodeCount}, Pending: ${nodePool.pendingNodeCount}`;
    nodePoolTitle.appendChild(countBadge);

    // Auto-provision status badge
    if (nodePool.autoProvisionDisabled) {
        const disabledBadge = document.createElement('span');
        disabledBadge.className = 'badge auto-provision-disabled-badge';
        disabledBadge.textContent = 'Auto-Provision Disabled';
        disabledBadge.title = 'Auto-provisioning is disabled for this node pool';
        nodePoolTitle.appendChild(disabledBadge);
    }

    // Auto-mode status badge
    if (nodePool.autoModeDisabled) {
        const autoModeDisabledBadge = document.createElement('span');
        autoModeDisabledBadge.className = 'badge auto-mode-disabled-badge';
        autoModeDisabledBadge.textContent = 'Auto-Mode Disabled';
        autoModeDisabledBadge.title = 'Auto-mode (automatic node recycling) is disabled for this node pool';
        nodePoolTitle.appendChild(autoModeDisabledBadge);
    }

    // Provisioning monitor pause badge
    if (nodePool.provisioningMonitorDisabled) {
        const monitorPausedBadge = document.createElement('span');
        monitorPausedBadge.className = 'badge provisioning-monitor-paused-badge';
        monitorPausedBadge.textContent = 'Provisioning Monitor Paused';
        monitorPausedBadge.title = 'The timeout-based provisioning monitor is paused for this node pool. Nodes stuck in provisioning will not be auto-recycled until resumed.';
        nodePoolTitle.appendChild(monitorPausedBadge);
    }

    const isForgeNodePool = clusterAllowsForgeNodePools(state.selectedClusterDetails) && !nodePoolUsesCAPI(nodePool);
    if (isForgeNodePool) {
        // Subnet/VPC Prefix Exhausted Badge
        const hasVpcPrefixes = Array.isArray(nodePool.vpcPrefixIDs) && nodePool.vpcPrefixIDs.length > 0;
        const idsToCheck = hasVpcPrefixes ? nodePool.vpcPrefixIDs : nodePool.subnetIDs;
        const safeIdsToCheck = Array.isArray(idsToCheck) ? idsToCheck : [];
        const isExhausted = safeIdsToCheck.length > 0 && safeIdsToCheck.every(id => {
            const availability = hasVpcPrefixes
                ? calculateVpcPrefixIpAvailability(id)
                : calculateSubnetAvailability(id);
            return (availability?.available ?? 0) === 0;
        });
        if (isExhausted) {
            const exhaustedBadge = document.createElement('span');
            exhaustedBadge.className = 'badge subnet-full-badge';
            exhaustedBadge.textContent = hasVpcPrefixes ? 'All VPC Prefixes Exhausted' : 'All Subnets Exhausted';
            exhaustedBadge.title = hasVpcPrefixes
                ? 'All VPC prefixes associated with this node pool have no available IP addresses. Adding new nodes will likely fail.'
                : 'All subnets associated with this node pool have no available IP addresses. Adding new nodes will likely fail.';
            nodePoolTitle.appendChild(exhaustedBadge);
        }

        // GPU Availability Badge
        if (isGPUInstanceType(nodePool.instanceTypeID)) {
            const instanceType = getInstanceTypeAvailability(nodePool.instanceTypeID);
            // Only show GPU badge if:
            // 1. Node pool is NOT deprecated
            // 2. At least one subnet/VPC prefix has available IPs
            // 3. There are usable GPU instances
            if (!nodePool.deprecated && !isExhausted) {
                if (instanceType && instanceType.unusedUsable !== undefined && instanceType.unusedUsable > 0) {
                    const gpuAvailableBadge = document.createElement('span');
                    gpuAvailableBadge.className = 'badge gpu-available-badge';
                    if (instanceType.total !== undefined) {
                        gpuAvailableBadge.textContent = `GPUs Available (${instanceType.unusedUsable}/${instanceType.total} available)`;
                        gpuAvailableBadge.title = `There are ${instanceType.unusedUsable} out of ${instanceType.total} instances of type ${instanceType.name} available.`;
                    } else {
                        gpuAvailableBadge.textContent = `GPUs Available (${instanceType.unusedUsable})`;
                        gpuAvailableBadge.title = `There are ${instanceType.unusedUsable} instances of type ${instanceType.name} available.`;
                    }
                    nodePoolTitle.appendChild(gpuAvailableBadge);
                }
            }
        }
    }

    const nodePoolActions = document.createElement('div');
    nodePoolActions.className = 'nodepool-header-actions';

    const configHistoryBtn = document.createElement('button');
    configHistoryBtn.className = 'add-btn';
    configHistoryBtn.textContent = 'Config History';
    configHistoryBtn.title = 'View config change history for this node pool';
    configHistoryBtn.onclick = () => showNodePoolConfigHistoryModal(nodePool.id, nodePool.name);

    const editNodePoolBtn = document.createElement('button');
    editNodePoolBtn.className = 'add-btn';
    editNodePoolBtn.textContent = 'Edit Node Pool';
    editNodePoolBtn.title = 'Edit this node pool\'s configuration';
    editNodePoolBtn.onclick = () => showEditNodePoolModal(nodePool.id);

    const monitorToggleBtn = document.createElement('button');
    if (nodePool.provisioningMonitorDisabled) {
        monitorToggleBtn.className = 'resume-btn';
        monitorToggleBtn.textContent = 'Resume Provisioning Monitor';
        monitorToggleBtn.title = 'Resume the timeout-based provisioning monitor for this node pool. Stuck nodes will be auto-recycled again.';
    } else {
        monitorToggleBtn.className = 'pause-btn';
        monitorToggleBtn.textContent = 'Pause Provisioning Monitor';
        monitorToggleBtn.title = 'Pause the timeout-based provisioning monitor for this node pool. Nodes stuck in provisioning will not be auto-recycled while paused.';
    }
    monitorToggleBtn.onclick = () => toggleNodePoolProvisioningMonitor(nodePool);

    const deleteNodePoolBtn = document.createElement('button');
    deleteNodePoolBtn.className = 'delete-btn';
    deleteNodePoolBtn.textContent = 'Delete Node Pool';
    deleteNodePoolBtn.title = 'Delete this node pool from the cluster';
    deleteNodePoolBtn.onclick = () => showDeleteNodePoolModal(nodePool.id, nodePool.name);

    nodePoolHeader.appendChild(nodePoolTitle);
    nodePoolActions.appendChild(configHistoryBtn);
    nodePoolActions.appendChild(editNodePoolBtn);
    nodePoolActions.appendChild(monitorToggleBtn);
    nodePoolActions.appendChild(deleteNodePoolBtn);
    nodePoolHeader.appendChild(nodePoolActions);
    nodePoolContainer.appendChild(nodePoolHeader);

    const nodePoolDetails = document.createElement('div');
    nodePoolDetails.className = 'nodepool-details';

    const nodePoolDetailsLeftColumn = document.createElement('div');
    nodePoolDetailsLeftColumn.className = 'nodepool-details-column';


    // Detail fields with resolved names and availability info
    const instanceType = getInstanceTypeAvailability(nodePool.instanceTypeID);
    const instanceTypeText = resolveInstanceTypeName(nodePool.instanceTypeID) + formatInstanceTypeAvailabilitySuffix(instanceType);
 
    let infraProviderFields = [];

    if (isForgeNodePool) {
        let subnetText = 'None';
        const hasVpcPrefixes = Array.isArray(nodePool.vpcPrefixIDs) && nodePool.vpcPrefixIDs.length > 0;
        const idsToDisplay = hasVpcPrefixes ? nodePool.vpcPrefixIDs : nodePool.subnetIDs;
        const safeIdsToDisplay = Array.isArray(idsToDisplay) ? idsToDisplay : [];
        const labelText = hasVpcPrefixes 
            ? (safeIdsToDisplay.length === 1 ? 'VPC Prefix' : 'VPC Prefixes')
            : (safeIdsToDisplay.length === 1 ? 'Subnet' : 'Subnets');
        
        if (safeIdsToDisplay.length === 1) {
            const availability = hasVpcPrefixes
                ? calculateVpcPrefixIpAvailability(safeIdsToDisplay[0])
                : calculateSubnetAvailability(safeIdsToDisplay[0]);
            subnetText = hasVpcPrefixes
                ? resolveVpcPrefixName(safeIdsToDisplay[0])
                : resolveSubnetName(safeIdsToDisplay[0]);
            if (availability?.total !== '?') {
                subnetText += ` (${availability?.available ?? 0}/${availability.total} IPs available)`;
            }
        } else {
            const names = safeIdsToDisplay.map(id => {
                const availability = hasVpcPrefixes
                    ? calculateVpcPrefixIpAvailability(id)
                    : calculateSubnetAvailability(id);
                let name = hasVpcPrefixes
                    ? resolveVpcPrefixName(id)
                    : resolveSubnetName(id);
                if (availability?.total !== '?') {
                    name += ` (${availability?.available ?? 0}/${availability.total} IPs)`;
                }
                return name;
            });
            subnetText = names.join('<br>');
        }

        infraProviderFields.push({ label: labelText, htmlValue: subnetText });
        infraProviderFields.push({ label: 'Instance Type', value: instanceTypeText });  
        infraProviderFields.push({ label: 'InfiniBand Partition', value: resolveInfiniBandPartitionName(nodePool.infinibandPartitionID) });
        infraProviderFields.push({ label: 'SSH Key Groups', value: resolveSSHKeyGroupNames(nodePool.sshKeyGroupIDs) });

    }

    if (isForgeNodePool) {
        infraProviderFields.push({ label: 'OS Profile', value: nodePool.osProfile });
    }
    infraProviderFields.push({ label: 'Teleport Enabled', value: nodePool.teleportEnabled ? 'Yes' : 'No' });
    if (nodePoolUsesCAPI(nodePool)) {
        const capiBootstrapFiles = Array.isArray(nodePool.capiBootstrapFiles) ? nodePool.capiBootstrapFiles : [];
        const capiPreKubeadmCommands = Array.isArray(nodePool.capiPreKubeadmCommands) ? nodePool.capiPreKubeadmCommands : [];
        infraProviderFields.push({ label: 'Infra Machine Template', manifest: nodePool.capiInfraMachineTemplate });
        infraProviderFields.push({ label: 'SPIRE Attestation Method', value: formatSpireAttestationMethod(nodePool.capiSpireAttestationMethod) });
        infraProviderFields.push(capiBootstrapFiles.length > 0
            ? { label: 'Bootstrap Files', manifest: capiBootstrapFiles }
            : { label: 'Bootstrap Files', value: 'None' });
        infraProviderFields.push(capiPreKubeadmCommands.length > 0
            ? { label: 'Pre-Kubeadm Commands', manifest: capiPreKubeadmCommands }
            : { label: 'Pre-Kubeadm Commands', value: 'None' });
    }

    const leftColumnFields = [
        { label: 'Name', value: nodePool.name, copyable: true },
        { label: 'ID', value: nodePool.id, copyable: true },
        { label: 'Node Name Prefix', value: nodePool.nodeNamePrefix },
        { label: 'Control Plane', value: nodePool.controlPlane ? 'Yes' : 'No' },
        { label: 'Config Profile', value: nodePool.configProfile },
        { label: 'Kubernetes Version', value: nodePool.kubernetesVersion },
        { label: 'System Reserved Memory', value: nodePool.systemReservedMemory },
        { label: 'Kube Reserved Memory', value: nodePool.kubeReservedMemory || 'Default' },
        ...infraProviderFields,
    ];

    leftColumnFields.forEach((field) => {
        const detailRow = document.createElement('div');
        detailRow.className = 'nodepool-detail-row';

        const detailLabel = document.createElement('div');
        detailLabel.className = 'nodepool-detail-label';
        detailLabel.textContent = field.label + ':';

        const detailValue = document.createElement('div');
        detailValue.className = 'nodepool-detail-value';
        if (field.manifest !== undefined) {
            detailValue.appendChild(createManifestValueElement(field.manifest, field.label));
        } else if (field.htmlValue) {
            detailValue.innerHTML = field.htmlValue;
        } else if (field.copyable && field.value) {
            detailValue.appendChild(createCopyableValue(field.value, field.label));
        } else {
            detailValue.textContent = field.value;
        }

        detailRow.appendChild(detailLabel);
        detailRow.appendChild(detailValue);
        nodePoolDetailsLeftColumn.appendChild(detailRow);
    });

    const condition = getHighestPriorityCondition(nodePool.conditions);
    if (condition) {
        const conditionContainer = document.createElement('div');
        conditionContainer.className = 'nodepool-detail-row';

        const conditionLabel = document.createElement('div');
        conditionLabel.className = 'nodepool-detail-label';
        conditionLabel.textContent = 'Status:';

        const conditionValue = document.createElement('div');
        conditionValue.className = 'nodepool-detail-value';

        const conditionBadge = document.createElement('span');
        conditionBadge.className = `condition condition-${condition.type.toLowerCase().replace(/\s+/g, '-')}`;
        conditionBadge.textContent = condition.type;
        conditionBadge.title = condition.message || condition.type;

        conditionValue.appendChild(conditionBadge);

        if (condition.message) {
            const messageSpan = document.createElement('span');
            messageSpan.className = 'condition-message';
            messageSpan.textContent = condition.message;
            conditionValue.appendChild(messageSpan);
        }

        conditionContainer.appendChild(conditionLabel);
        conditionContainer.appendChild(conditionValue);
        nodePoolDetailsLeftColumn.appendChild(conditionContainer);
    }

    nodePoolDetails.appendChild(nodePoolDetailsLeftColumn);

    const nodePoolDetailsRightColumn = document.createElement('div');
    nodePoolDetailsRightColumn.className = 'nodepool-details-column';

    const rightColumnFields = [
        { label: 'Desired Count', value: nodePool.desiredNodeCount },
        { label: 'Actual Count', value: nodePool.actualNodeCount },
        { label: 'Pending Count', value: nodePool.pendingNodeCount },
        { label: 'Recycle Budget', value: nodePool.recycleBudget },
        { label: 'Auto-Provision Enabled', value: nodePool.autoProvisionDisabled ? 'No' : 'Yes' },
        { label: 'Auto-Mode Enabled', value: nodePool.autoModeDisabled ? 'No' : 'Yes' },
        { label: 'Provisioning Monitor', value: nodePool.provisioningMonitorDisabled ? 'Paused' : 'Active' },
    ];
    if (!nodePoolUsesCAPI(nodePool)) {
        rightColumnFields.push({ label: 'Disk Selection Strategy', value: nodePool.diskSelectionStrategy });
    }
    
    // Add MaxSurge for node pools
    if (nodePool.maxSurge !== undefined && nodePool.maxSurge !== null) {
        rightColumnFields.push({ label: 'Max Surge', value: nodePool.maxSurge });
    }

    rightColumnFields.forEach((field) => {
        const detailRow = document.createElement('div');
        detailRow.className = 'nodepool-detail-row';

        const detailLabel = document.createElement('div');
        detailLabel.className = 'nodepool-detail-label';
        detailLabel.textContent = field.label + ':';

        const detailValue = document.createElement('div');
        detailValue.className = 'nodepool-detail-value';
        detailValue.textContent = field.value;

        detailRow.appendChild(detailLabel);
        detailRow.appendChild(detailValue);
        nodePoolDetailsRightColumn.appendChild(detailRow);
    });

    nodePoolDetails.appendChild(nodePoolDetailsRightColumn);

    nodePoolContainer.appendChild(nodePoolDetails);

    if (nodePool.description) {
        const notesContainer = document.createElement('div');
        notesContainer.className = 'nodepool-notes';

        const notesValue = document.createElement('div');
        notesValue.className = 'nodepool-notes-value';
        notesValue.textContent = nodePool.description;
        notesValue.style.whiteSpace = 'pre-line';

        notesContainer.appendChild(notesValue);
        nodePoolContainer.appendChild(notesContainer);
    }

    const nodeSetsContainer = document.createElement('div');
    nodeSetsContainer.className = 'nodesets-container';
    nodeSetsContainer.id = `nodepool-nodesets-${nodePool.id}`;
    nodePoolContainer.appendChild(nodeSetsContainer);

    for (const nodeSet of nodePool.nodeSets) {
        const nodeSetContainer = document.createElement('div');
        nodeSetContainer.className = 'nodeset-container';
        nodeSetContainer.id = `nodeset-${nodeSet.id}`;
        nodeSetsContainer.appendChild(nodeSetContainer);

        renderNodeSet(nodeSet);
    }
}

function renderNodeSet(nodeSet) {
    const nodeSetContainer = document.getElementById(`nodeset-${nodeSet.id}`);
    if (!nodeSetContainer) {
        return;
    }

    nodeSetContainer.innerHTML = '';

    const nodeSetHeader = document.createElement('div');
    nodeSetHeader.className = 'nodeset-header';
    nodeSetHeader.style.display = 'flex';
    nodeSetHeader.style.justifyContent = 'space-between';
    nodeSetHeader.style.alignItems = 'center';

    const nodeSetHeaderLeft = document.createElement('div');
    nodeSetHeaderLeft.style.display = 'flex';
    nodeSetHeaderLeft.style.alignItems = 'center';
    nodeSetHeaderLeft.style.gap = '10px';

    // Current badge
    if (nodeSet.isCurrent) {
        const currentBadge = document.createElement('span');
        currentBadge.className = 'badge current-badge';
        currentBadge.textContent = 'Current';
        currentBadge.title = 'This is the current node set for this node pool';
        nodeSetHeader.appendChild(currentBadge);
    }

    if (!nodeSet.isCurrent) {
        const deprecatedBadge = document.createElement('span');
        deprecatedBadge.className = 'badge deprecated-badge';
        deprecatedBadge.textContent = 'Deprecated';
        deprecatedBadge.title = 'This node set is deprecated';
        nodeSetHeader.appendChild(deprecatedBadge);
    }

    // Teleport badge
    if (nodeSet.teleportEnabled) {
        const teleportBadge = document.createElement('span');
        teleportBadge.className = 'badge teleport-badge';
        teleportBadge.textContent = 'Teleport';
        teleportBadge.title = 'Teleport SSH enrollment is enabled for this node set';
        nodeSetHeader.appendChild(teleportBadge);
    }

    const nodeSetLabel = document.createElement('span');
    nodeSetLabel.textContent = 'Node Set';
    nodeSetHeaderLeft.appendChild(nodeSetLabel);

    nodeSetHeader.appendChild(nodeSetHeaderLeft);

    nodeSetContainer.appendChild(nodeSetHeader);

    const nodeSetDetails = document.createElement('div');
    nodeSetDetails.className = 'nodeset-details';

    const nodeSetDetailsLeftColumn = document.createElement('div');
    nodeSetDetailsLeftColumn.className = 'nodeset-details-column';

    const leftColumnFields = [
        { label: 'ID', value: nodeSet.id, copyable: true },
        ...(nodeSetUsesCAPI(nodeSet) ? [] : [
            { label: 'OS Profile', value: nodeSet.osProfile },
            { label: 'Disk Selection Strategy', value: nodeSet.diskSelectionStrategy },
        ]),
        { label: 'Config Profile', value: nodeSet.configProfile },
        { label: 'Kubernetes Version', value: nodeSet.kubernetesVersion },
        { label: 'System Reserved Memory', value: nodeSet.systemReservedMemory },
        { label: 'Kube Reserved Memory', value: nodeSet.kubeReservedMemory || 'Default' },
        ...(nodeSetUsesCAPI(nodeSet) ? [
            { label: 'Infra Machine Template', manifest: nodeSet.capiInfraMachineTemplate },
            ...(Array.isArray(nodeSet.capiBootstrapFiles) && nodeSet.capiBootstrapFiles.length > 0
                ? [{ label: 'Bootstrap Files', manifest: nodeSet.capiBootstrapFiles }]
                : [{ label: 'Bootstrap Files', value: 'None' }]),
            ...(Array.isArray(nodeSet.capiPreKubeadmCommands) && nodeSet.capiPreKubeadmCommands.length > 0
                ? [{ label: 'Pre-Kubeadm Commands', manifest: nodeSet.capiPreKubeadmCommands }]
                : [{ label: 'Pre-Kubeadm Commands', value: 'None' }]),
        ] : []),
    ];

    leftColumnFields.forEach((field) => {
        const detailRow = document.createElement('div');
        detailRow.className = 'nodeset-detail-row';

        const detailLabel = document.createElement('div');
        detailLabel.className = 'nodeset-detail-label';
        detailLabel.textContent = field.label + ':';

        const detailValue = document.createElement('div');
        detailValue.className = 'nodeset-detail-value';
        if (field.manifest !== undefined) {
            detailValue.appendChild(createManifestValueElement(field.manifest, field.label));
        } else if (field.copyable && field.value) {
            detailValue.appendChild(createCopyableValue(field.value, field.label));
        } else {
            detailValue.textContent = field.value;
        }

        detailRow.appendChild(detailLabel);
        detailRow.appendChild(detailValue);
        nodeSetDetailsLeftColumn.appendChild(detailRow);
    });

    nodeSetDetails.appendChild(nodeSetDetailsLeftColumn);


    const nodeSetDetailsRightColumn = document.createElement('div');
    nodeSetDetailsRightColumn.className = 'nodeset-details-column';

    const rightColumnFields = [
        { label: 'Actual Count', value: nodeSet.actualNodeCount },
        { label: 'Pending Count', value: nodeSet.pendingNodeCount },
    ];

    rightColumnFields.forEach((field) => {
        const detailRow = document.createElement('div');
        detailRow.className = 'nodeset-detail-row';

        const detailLabel = document.createElement('div');
        detailLabel.className = 'nodeset-detail-label';
        detailLabel.textContent = field.label + ':';

        const detailValue = document.createElement('div');
        detailValue.className = 'nodeset-detail-value';
        if (field.htmlValue) {
            detailValue.innerHTML = field.htmlValue;
        } else {
            detailValue.textContent = field.value;
        }

        detailRow.appendChild(detailLabel);
        detailRow.appendChild(detailValue);
        nodeSetDetailsRightColumn.appendChild(detailRow);
    });

    nodeSetDetails.appendChild(nodeSetDetailsRightColumn);

    nodeSetContainer.appendChild(nodeSetDetails);

    const nodesContainer = document.createElement('div');
    nodesContainer.className = 'nodes-container';
    nodesContainer.id = `nodeset-nodes-${nodeSet.id}`;
    nodeSetContainer.appendChild(nodesContainer);

    const safeNodes = Array.isArray(nodeSet.nodes) ? nodeSet.nodes : [];
    if (safeNodes.length === 0) {
        const emptyStateDiv = document.createElement('div');
        emptyStateDiv.className = 'empty-state';
        emptyStateDiv.innerHTML = '<p>Node set is empty</p>';
        
        nodesContainer.appendChild(emptyStateDiv);
        return;
    }

    const table = document.createElement('table');
    table.className = 'node-table';
    table.setAttribute('data-nodeset-id', nodeSet.id);

    const thead = document.createElement('thead');
    const headerRow = document.createElement('tr');

    const nameHeader = document.createElement('th');
    nameHeader.textContent = 'Node Name';

    const ipHeader = document.createElement('th');
    ipHeader.textContent = 'IP Address';

    const conditionsHeader = document.createElement('th');
    conditionsHeader.textContent = 'Conditions';
    conditionsHeader.className = 'conditions-cell';

    const actionsHeader = document.createElement('th');
    actionsHeader.className = 'actions-cell';
    actionsHeader.textContent = 'Actions';

    headerRow.appendChild(nameHeader);
    headerRow.appendChild(ipHeader);
    headerRow.appendChild(conditionsHeader);
    headerRow.appendChild(actionsHeader);
    thead.appendChild(headerRow);
    table.appendChild(thead);

    const tbody = document.createElement('tbody');
    table.appendChild(tbody);
    nodesContainer.appendChild(table);

    safeNodes.forEach(node => {
        const row = document.createElement('tr');
        row.id = `node-${node.id}`;
        row.setAttribute('data-node-id', node.id);
        tbody.appendChild(row);
        renderNode(node);
    });

    return table;
}

function renderNode(node, options = {}) {
    const row = document.getElementById(`node-${node.id}`);
    if (!row) {
        return;
    }

    row.innerHTML = '';

    const nameCell = document.createElement('td');
    nameCell.className = 'name-cell';
    if (node.name) {
        nameCell.appendChild(createCopyableValue(node.name, 'node name'));
    }

    if (options.includeInstanceType) {
        const instanceTypeCell = document.createElement('td');
        instanceTypeCell.className = 'instance-type-cell';
        const instanceTypeText = resolveInstanceTypeName(node.instanceTypeID);
        instanceTypeCell.textContent = instanceTypeText || '—';
        row.appendChild(nameCell);
        row.appendChild(instanceTypeCell);
    }

    const ipCell = document.createElement('td');
    ipCell.className = 'ip-cell';
    if (node.ip) {
        ipCell.appendChild(createCopyableValue(node.ip, 'IP address'));
    }

    const conditionsCell = document.createElement('td');
    conditionsCell.className = 'conditions-cell';

    const condition = getHighestPriorityCondition(node.conditions);
    if (condition) {
        const conditionSpan = document.createElement('span');
        conditionSpan.className = `condition condition-${condition.type.toLowerCase().replace(/\s+/g, '-')}`;
        conditionSpan.textContent = condition.type;
        conditionSpan.title = condition.message || condition.type;
        conditionsCell.appendChild(conditionSpan);

        if (condition.message) {
            const messageSpan = document.createElement('span');
            messageSpan.className = 'condition-message';
            messageSpan.textContent = condition.message;
            conditionsCell.appendChild(messageSpan);
        }
    }

    // Append additional health conditions on provisioned nodes. Only display if unhealthy.
    if (condition && condition.type === 'Provisioned') {
        const healthConditions = [
            { condition: node.conditions.kubernetesReady, displayName: 'K8s Not Ready', healthyStatus: 'True' },
            { condition: node.conditions.kubernetesUnschedulable, displayName: 'K8s Unschedulable', healthyStatus: 'False' },
            { condition: node.conditions.nodeAgentReady, displayName: 'Node Agent Not Ready', healthyStatus: 'True' },
            { condition: node.conditions.teleportRegistered, displayName: 'Teleport Not Registered', healthyStatus: 'True' }
        ];

        healthConditions.forEach(healthComdition => {
            if (healthComdition.condition && healthComdition.condition.status !== healthComdition.healthyStatus) {
                const healthDiv = document.createElement('div');
                healthDiv.className = 'health-condition';
                
                const healthSpan = document.createElement('span');
                healthSpan.className = 'condition condition-health';
                healthSpan.textContent = healthComdition.displayName;
                healthSpan.title = healthComdition.condition.message || healthComdition.displayName;
                healthDiv.appendChild(healthSpan);

                if (healthComdition.condition.message) {
                    const messageSpan = document.createElement('span');
                    messageSpan.className = 'condition-message';
                    messageSpan.textContent = healthComdition.condition.message;
                    healthDiv.appendChild(messageSpan);
                }

                conditionsCell.appendChild(healthDiv);
            }
        });
    }

    // If the node is stuck in provisioning and its parent pool has the provisioning monitor paused,
    // surface that on the node so operators understand why it is not being auto-recycled.
    if (condition && condition.type === 'Provisioning' && node.nodePoolID) {
        const parentPool = (state.selectedClusterNodePools || []).find(np => np.id === node.nodePoolID);
        if (parentPool && parentPool.provisioningMonitorDisabled) {
            const monitorNote = document.createElement('div');
            monitorNote.className = 'node-monitor-paused-note';
            monitorNote.textContent = 'Parent pool\'s provisioning monitor is paused: this node will not be auto-recycled if provision timeout is exceeded.';
            monitorNote.title = 'The node pool that owns this node has its provisioning monitor paused. The timeout-based recycling is disabled until an operator resumes it.';
            conditionsCell.appendChild(monitorNote);
        }
    }

    const actionsCell = document.createElement('td');
    actionsCell.className = 'actions-cell';

    const historyBtn = document.createElement('button');
    historyBtn.className = 'history-btn';
    historyBtn.textContent = 'History';
    historyBtn.title = 'View workflow history for this node';
    historyBtn.onclick = () => showNodeHistoryModal(node.id, node.name);

    const deleteNodeBtn = document.createElement('button');
    deleteNodeBtn.className = 'delete-btn';
    deleteNodeBtn.textContent = 'Delete Node';
    deleteNodeBtn.title = 'Delete this node from the cluster';
    deleteNodeBtn.onclick = () => showDeleteNodeModal(node.id, node.name);

    actionsCell.appendChild(historyBtn);
    actionsCell.appendChild(deleteNodeBtn);

    if (!options.includeInstanceType) {
        row.appendChild(nameCell);
    }
    row.appendChild(ipCell);
    row.appendChild(conditionsCell);
    row.appendChild(actionsCell);

    return row;
}

// Get the highest priority condition (deleting > updateInProgress > markedForRepair > markedForRecycling > controlPlaneUpgrading > cpNodesRecycling > rebooting > provisioned > provisioning > pending)
function getHighestPriorityCondition(conditions) {
    if (!conditions) {
        return null;
    }

    if (conditions.deleting && conditions.deleting.status === "True") {
        return { type: "Deleting", message: conditions.deleting.message };
    } else if (conditions.updateInProgress) {
        return { type: "Update In Progress", message: formatUpdateInProgressMessage(conditions.updateInProgress) };
    } else if (conditions.updateFailed && conditions.updateFailed.status === "True") {
        return { type: "Update Failed", message: conditions.updateFailed.message };
    } else if (conditions.markedForRepair && conditions.markedForRepair.status === "True") {
        return { type: "Marked for Repair", message: conditions.markedForRepair.message };
    } else if (conditions.markedForRecycling && conditions.markedForRecycling.status === "True") {
        return { type: "Marked for Recycling", message: conditions.markedForRecycling.message };
    } else if (conditions.controlPlaneUpgrading && conditions.controlPlaneUpgrading.status === "True") {
        return { type: "Control Plane Upgrading", message: conditions.controlPlaneUpgrading.message };
    } else if (conditions.cpNodesRecycling && conditions.cpNodesRecycling.status === "True") {
        return { type: "CP Nodes Recycling", message: conditions.cpNodesRecycling.message };
    } else if (conditions.rebooting && conditions.rebooting.status === "True") {
        return { type: "Rebooting", message: conditions.rebooting.message };
    } else if (conditions.provisioned && conditions.provisioned.status === "True") {
        return { type: "Provisioned", message: conditions.provisioned.message };
    } else if (conditions.provisioning && conditions.provisioning.status === "True") {
        return { type: "Provisioning", message: conditions.provisioning.message };
    } else if (conditions.pending && conditions.pending.status === "True") {
        return { type: "Pending", message: conditions.pending.message };
    }
    return null;
}

function formatUpdateInProgressMessage(updateInProgress) {
    const operationName = {
        etcd_encryption: 'Etcd encryption',
        k8s_upgrade: 'Kubernetes upgrade',
        cp_nodes_recycling: 'CP nodes recycling',
    }[updateInProgress.type] || 'Cluster update';

    if (updateInProgress.message) {
        return `${operationName}: ${updateInProgress.message}`;
    }
    return `${operationName} in progress`;
}

// Helper function to count nodes with specific condition
function countNodesByCondition(nodes, conditionType) {
    if (!nodes || !Array.isArray(nodes)) return 0;
    
    return nodes.filter(node => {
        const condition = getHighestPriorityCondition(node.conditions);
        return condition && condition.type === conditionType;
    }).length;
}

function showSelectedClusterSection() {
    document.getElementById('right-side-container').style.display = 'flex';
}

// Reset all node pool selections when org or site changes
function resetNodePoolDependentFields() {
    const nodePoolConfigs = document.querySelectorAll('.nodepool-config');
    nodePoolConfigs.forEach(config => {
        const idx = config.dataset.idx;

        const instanceTypeSelect = document.getElementById(`nodepool-${idx}-instance-type-id`);
        if (instanceTypeSelect) {
            instanceTypeSelect.innerHTML = '<option value="">Select an instance type</option>';
            instanceTypeSelect.disabled = true;
            instanceTypeSelect.title = "You must select a tenant first";
        }

        const infiniBandPartitionSelect = document.getElementById(`nodepool-${idx}-infiniband-partition-id`);
        if (infiniBandPartitionSelect) {
            infiniBandPartitionSelect.innerHTML = '<option value="">Select an InfiniBand Partition</option>';
            infiniBandPartitionSelect.disabled = true;
            infiniBandPartitionSelect.title = "You must select a tenant first";
        }

        const nvLinkLogicalPartitionSelect = document.getElementById(`nodepool-${idx}-nvlink-logical-partition-id`);
        if (nvLinkLogicalPartitionSelect) {
            nvLinkLogicalPartitionSelect.innerHTML = '<option value="">Select an NVLink Logical Partition</option>';
            nvLinkLogicalPartitionSelect.disabled = true;
            nvLinkLogicalPartitionSelect.title = "You must select a tenant first";
        }

        const subnetSelect = document.getElementById(`nodepool-${idx}-subnet-id`);
        if (subnetSelect) {
            subnetSelect.innerHTML = '<option value="">Select a subnet</option>';
            subnetSelect.disabled = true;
            subnetSelect.title = "You must select a VPC first";
        }

        const sshKeyGroupsSelect = document.getElementById(`nodepool-${idx}-ssh-key-groups`);
        if (sshKeyGroupsSelect) {
            sshKeyGroupsSelect.innerHTML = '<option value="">Select SSH key groups</option>';
            sshKeyGroupsSelect.disabled = true;
            sshKeyGroupsSelect.title = "You must select a tenant first";
        }
    });
}

// Reset only subnet selections when vpc changes
function resetNodePoolSubnetFields() {
    const nodePoolConfigs = document.querySelectorAll('.nodepool-config');
    nodePoolConfigs.forEach(config => {
        const idx = config.dataset.idx;

        const subnetsContainer = document.getElementById(`nodepool-${idx}-subnets-container`);
        if (subnetsContainer) {
            subnetsContainer.innerHTML = '<div class="empty-text">Select subnets</div>';
            subnetsContainer.classList.add('disabled');
            subnetsContainer.title = "You must select a VPC first";
        }
    });
}

function hideSelectedClusterSection() {
    document.getElementById('right-side-container').style.display = 'none';
}

async function ensureCreateClusterForgeMetadata(preSelectedSiteID) {
    if (!siteSupportsForgeProvisioning(preSelectedSiteID)) {
        return;
    }

    if (!state.tenants || state.tenants.length === 0) {
        try {
            state.tenants = await getForgeTenants();
        } catch (error) {
            console.warn('Failed to fetch Forge tenants for create cluster modal:', error);
            state.tenants = [];
        }
    }

    const hasMatchingCachedSite = !!preSelectedSiteID
        && Array.isArray(state.forgeSites)
        && state.forgeSites.some((site) => site.id === preSelectedSiteID);
    if (state.tenants.length > 0 && (!state.forgeSites || state.forgeSites.length === 0 || !hasMatchingCachedSite)) {
        await loadForgeSites();
    }
}

async function showCreateClusterModal(preSelectedSiteID = null) {
    // Validate that a site ID is provided since the modal now requires it
    if (!preSelectedSiteID) {
        console.error('showCreateClusterModal requires a preSelectedSiteID parameter');
        alert('Error: No site specified for cluster creation');
        return;
    }

    const form = document.getElementById('create-cluster-form');
    if (form) {
        form.reset();
    }

    document.getElementById('create-cluster-modal').style.display = 'block';
    document.getElementById('create-cluster-error').style.display = 'none';
    document.getElementById('create-cluster-loading').style.display = 'none';

    await setupClusterOptionDropdowns(preSelectedSiteID);
    populateCNIPluginDropdown();

    setupCreateClusterCPSection();

    const nodePoolsList = document.getElementById('create-cluster-nodepools-list');
    nodePoolsList.innerHTML = '';

    if (state.cpControllerMode) {
        // Cluster-level-CP mode: the control plane is configured via the dedicated
        // Control Plane section (controlPlaneConfig); the node-pool editor only
        // produces worker pools.
        addNodePoolConfig('worker', 0, false, 'w');
    } else {
        addNodePoolConfig('control-plane', 3, true, 'cp');
        addNodePoolConfig('worker', 0, false, 'w');
    }

    handleInfrastructureProviderChange();
}

function parseYAMLManifestToJSONObject(yamlString, fieldLabel) {
    const trimmed = (yamlString || '').trim();
    if (!trimmed) {
        return undefined;
    }

    if (!window.jsyaml || typeof window.jsyaml.load !== 'function') {
        throw new Error('YAML parser is not loaded');
    }

    let parsed;
    try {
        parsed = window.jsyaml.load(trimmed);
    } catch (error) {
        throw new Error(`${fieldLabel} YAML is invalid: ${error.message}`);
    }

    if (parsed === null || Array.isArray(parsed) || typeof parsed !== 'object') {
        throw new Error(`${fieldLabel} YAML must parse to a JSON object`);
    }

    return parsed;
}

function parseYAMLToArray(yamlString, fieldLabel) {
    const trimmed = (yamlString || '').trim();
    if (!trimmed) {
        return [];
    }

    if (!window.jsyaml || typeof window.jsyaml.load !== 'function') {
        throw new Error('YAML parser is not loaded');
    }

    let parsed;
    try {
        parsed = window.jsyaml.load(trimmed);
    } catch (error) {
        throw new Error(`${fieldLabel} YAML is invalid: ${error.message}`);
    }

    if (!Array.isArray(parsed)) {
        throw new Error(`${fieldLabel} YAML must parse to an array`);
    }

    return parsed;
}

function parseCapiBootstrapFilesInput(yamlString, fieldLabel) {
    const parsed = parseYAMLToArray(yamlString, fieldLabel);

    return parsed.map((item, index) => {
        if (item === null || Array.isArray(item) || typeof item !== 'object') {
            throw new Error(`${fieldLabel} item ${index + 1} must be an object`);
        }

        const normalizedPath = typeof item.path === 'string' ? item.path.trim() : '';
        if (!normalizedPath) {
            throw new Error(`${fieldLabel} item ${index + 1} requires a non-empty string path`);
        }
        if (typeof item.content !== 'string') {
            throw new Error(`${fieldLabel} item ${index + 1} requires a string content value`);
        }
        if (item.owner !== undefined && typeof item.owner !== 'string') {
            throw new Error(`${fieldLabel} item ${index + 1} owner must be a string when provided`);
        }
        if (item.permissions !== undefined && typeof item.permissions !== 'string') {
            throw new Error(`${fieldLabel} item ${index + 1} permissions must be a string when provided`);
        }

        return {
            path: normalizedPath,
            content: item.content,
            ...(item.owner !== undefined ? { owner: item.owner } : {}),
            ...(item.permissions !== undefined ? { permissions: item.permissions } : {}),
        };
    });
}

function parseCapiPreKubeadmCommandsInput(yamlString, fieldLabel) {
    const parsed = parseYAMLToArray(yamlString, fieldLabel);

    return parsed.map((item, index) => {
        if (typeof item !== 'string') {
            throw new Error(`${fieldLabel} item ${index + 1} must be a string`);
        }

        return item;
    });
}

function applyForgeFieldVisibility(container, showForgeFields) {
    if (!container) {
        return;
    }

    const forgeFieldGroups = container.querySelectorAll('.forge-only-field');
    forgeFieldGroups.forEach((group) => {
        group.style.display = showForgeFields ? '' : 'none';
        group.querySelectorAll('input, select').forEach((input) => {
            if (showForgeFields) {
                if (input.dataset.wasRequired === 'true') {
                    input.setAttribute('required', 'required');
                }
            } else if (input.hasAttribute('required')) {
                input.dataset.wasRequired = 'true';
                input.removeAttribute('required');
            }
        });
    });
}

function applyCAPIFieldVisibility(container, showCAPIFields) {
    if (!container) {
        return;
    }

    const capiFieldGroups = container.querySelectorAll('.capi-only-field');
    capiFieldGroups.forEach((group) => {
        group.style.display = showCAPIFields ? '' : 'none';
        group.querySelectorAll('input, select, textarea').forEach((input) => {
            if (showCAPIFields) {
                if (input.dataset.wasRequired === 'true') {
                    input.setAttribute('required', 'required');
                }
            } else if (input.hasAttribute('required')) {
                input.dataset.wasRequired = 'true';
                input.removeAttribute('required');
            }
        });
    });
}

function applyOSProfileFieldVisibility(container, showOSProfileField) {
    if (!container) {
        return;
    }

    const osProfileSelects = container.querySelectorAll('select[id$="-os-profile"]');
    osProfileSelects.forEach((select) => {
        const formGroup = select.closest('.form-group');
        if (formGroup) {
            formGroup.style.display = showOSProfileField ? '' : 'none';
        }
        if (showOSProfileField) {
            select.setAttribute('required', 'required');
        } else {
            select.removeAttribute('required');
        }
    });
}

function applyConfigProfileFieldBehavior(container, isCAPICluster) {
    if (!container) {
        return;
    }

    const configProfileSelects = container.querySelectorAll('select[id$="-config-profile"]');
    configProfileSelects.forEach((select) => {
        let emptyOption = select.querySelector('option[value=""]');
        if (!emptyOption) {
            emptyOption = document.createElement('option');
            emptyOption.value = '';
            select.insertBefore(emptyOption, select.firstChild);
        }

        emptyOption.textContent = isCAPICluster ? 'None' : 'Select a config profile';

        if (isCAPICluster) {
            select.removeAttribute('required');
        } else {
            select.setAttribute('required', 'required');
        }
    });
}

function handleInfrastructureProviderChange() {
    const isCAPICluster = createClusterUsesCAPIInfrastructure();
    const showSiteDetails = !!getCreateClusterSiteID();
    const orgIDFormGroup = document.getElementById('org-id-form-group');
    const siteIDFormGroup = document.getElementById('site-id-form-group');
    const vpcIDFormGroup = document.getElementById('vpc-id-form-group');
    const capiInfraClusterFormGroup = document.getElementById('capi-infra-cluster-form-group');

    if (orgIDFormGroup) {
        orgIDFormGroup.style.display = isCAPICluster ? 'none' : '';
    }

    if (siteIDFormGroup) {
        siteIDFormGroup.style.display = showSiteDetails ? '' : 'none';
    }

    if (vpcIDFormGroup) {
        vpcIDFormGroup.style.display = isCAPICluster ? 'none' : '';
    }

    if (capiInfraClusterFormGroup) {
        capiInfraClusterFormGroup.style.display = isCAPICluster ? '' : 'none';
    }

    const nodePoolConfigs = document.querySelectorAll('.nodepool-config');
    nodePoolConfigs.forEach((config) => {
        applyNodePoolInfrastructureFields(config, isCAPICluster);

        if (!isCAPICluster) {
            const idx = Number.parseInt(config.dataset.idx, 10);
            if (!Number.isNaN(idx)) {
                updateNodePoolSelectionsFromParent(idx);
            }
        }
    });

    // Cluster-level Control Plane section (cpControllerMode only): show it and toggle
    // its Forge-only / CAPI-only fields to match the selected infrastructure provider.
    const cpSection = document.getElementById('create-cluster-cp-section');
    if (cpSection) {
        if (state.cpControllerMode) {
            cpSection.style.display = showSiteDetails ? '' : 'none';
            applyForgeFieldVisibility(cpSection, !isCAPICluster);
            applyCAPIFieldVisibility(cpSection, isCAPICluster);
        } else {
            cpSection.style.display = 'none';
        }
    }

    updateClusterManifestRequirement();
}

function populateCNIPluginDropdown() {
    const cniPluginSelect = document.getElementById('cni-plugin');
    if (!cniPluginSelect) {
        return;
    }
    cniPluginSelect.innerHTML = '';

    if (state.cniPlugins && Array.isArray(state.cniPlugins) && state.cniPlugins.length > 0) {
        state.cniPlugins.forEach(plugin => {
            const option = document.createElement('option');
            option.value = plugin;
            option.textContent = plugin;
            if (plugin.toLowerCase() === 'calico') {
                option.selected = true;
            }
            cniPluginSelect.appendChild(option);
        });
    } else {
        const option = document.createElement('option');
        option.value = "";
        option.textContent = 'No CNI plugins available';
        option.disabled = true;
        cniPluginSelect.appendChild(option);
    }

    const updateNetworkInputs = () => {
        const customCIDRsSupported = cniPluginSelect.value === 'ovn-kubernetes';
        ['pod-cidr', 'service-cidr'].forEach(id => {
            const input = document.getElementById(id);
            if (!input) {
                return;
            }
            input.disabled = !customCIDRsSupported;
            if (!customCIDRsSupported) {
                input.value = '';
            }
        });
        updatePodCIDRCapacityHelp();
    };
    const podCIDRInput = document.getElementById('pod-cidr');
    if (podCIDRInput) {
        podCIDRInput.oninput = updatePodCIDRCapacityHelp;
    }
    cniPluginSelect.onchange = updateNetworkInputs;
    updateNetworkInputs();
}

function podCIDRNodeBlockCapacity(value) {
    let capacity = 0;
    for (const cidr of value.split(',')) {
        const parts = cidr.trim().split('/');
        if (parts.length !== 2 || !/^\d+$/.test(parts[1])) {
            return null;
        }
        const prefixLength = Number.parseInt(parts[1], 10);
        if (prefixLength < 12 || prefixLength > MAX_POD_CIDR_PREFIX_LENGTH) {
            return null;
        }
        const addressParts = parts[0].split('.');
        if (addressParts.length !== 4 || addressParts.some(part => !/^\d+$/.test(part))) {
            return null;
        }
        const octets = addressParts.map(part => Number.parseInt(part, 10));
        if (octets.some(octet => octet < 0 || octet > 255)) {
            return null;
        }
        const isPrivate = octets[0] === 10 ||
            (octets[0] === 172 && octets[1] >= 16 && octets[1] <= 31) ||
            (octets[0] === 192 && octets[1] === 168 && prefixLength >= 16);
        if (!isPrivate) {
            return null;
        }
        capacity += 2 ** (POD_CIDR_NODE_PREFIX_LENGTH - prefixLength);
    }
    return capacity;
}

function updatePodCIDRCapacityHelp() {
    const input = document.getElementById('pod-cidr');
    const help = document.getElementById('pod-cidr-capacity-help');
    if (!input || !help) {
        return;
    }
    const value = input.value.trim() || DEFAULT_CLUSTER_POD_CIDR;
    const capacity = podCIDRNodeBlockCapacity(value);
    help.textContent = capacity === null
        ? 'Enter private IPv4 pod CIDRs from /12 through /23 to calculate node capacity.'
        : `${value} provides ${capacity} /24 node block${capacity === 1 ? '' : 's'}; at least one block is reserved for scaling or surge.`;
}

async function setupClusterOptionDropdowns(preSelectedSiteID = null) {
    await ensureCreateClusterForgeMetadata(preSelectedSiteID);

    // Store the preselected site ID for form submission
    const hiddenSiteInput = document.getElementById('hidden-site-id') || (() => {
        const input = document.createElement('input');
        input.type = 'hidden';
        input.id = 'hidden-site-id';
        document.getElementById('create-cluster-form').appendChild(input);
        return input;
    })();
    hiddenSiteInput.value = preSelectedSiteID || '';

    // Display site name and ID as static text instead of dropdown
    const siteIDContainer = document.getElementById('site-id-container');
    if (siteIDContainer) {
        if (preSelectedSiteID) {
            const siteLabel = formatSelectedSiteDisplay(preSelectedSiteID);
            
            siteIDContainer.innerHTML = `
                <div style="padding: 8px 12px; background: #2a2a2a; border: 1px solid #444; border-radius: 4px; color: #999; cursor: not-allowed;">
                    ${siteLabel}
                </div>
            `;
            siteIDContainer.style.display = 'block';
        } else {
            // Hide site container if no site is preselected
            siteIDContainer.style.display = 'none';
        }
    }

    if (!siteSupportsForgeProvisioning(preSelectedSiteID)) {
        const orgIDContainer = document.getElementById('org-id-container');
        const vpcIDContainer = document.getElementById('vpc-id-container');
        if (orgIDContainer) {
            orgIDContainer.innerHTML = '';
        }
        if (vpcIDContainer) {
            vpcIDContainer.innerHTML = '';
        }
        return;
    }

    // Setup tenant dropdown (filtered by site)
    const orgIDContainer = document.getElementById('org-id-container');
    orgIDContainer.innerHTML = `
        <select id="org-id-select" style="width: 100%;">
            <option value="">Select a tenant</option>
        </select>
    `;

    const vpcIDContainer = document.getElementById('vpc-id-container');
    vpcIDContainer.innerHTML = `
        <select id="vpc-id-select" disabled style="width: 100%;" title="You must first select a tenant">
            <option value="">Select a VPC</option>
        </select>
    `;

    // Filter tenants by the preselected site
    const orgSelect = document.getElementById('org-id-select');
    let tenantsForSite = [];
    
    if (preSelectedSiteID && state.forgeSites && state.forgeSites.length > 0) {
        // Find all forge sites that match the preselected site ID
        const matchingSites = state.forgeSites.filter(fs => fs.id === preSelectedSiteID);
        
        // Get unique org IDs from matching sites (some sites can have multiple tenants)
        const orgIDsForSite = [...new Set(matchingSites.map(fs => fs.orgID))];
        
        // Filter tenants to only those that have this site
        tenantsForSite = state.tenants.filter(tenant => orgIDsForSite.includes(tenant.orgID));
    }

    if (tenantsForSite.length === 0) {
        if (siteSupportsForgeProvisioning(preSelectedSiteID) && state.tenants.length > 0) {
            console.warn(`No cached Forge tenant mapping found for site ${preSelectedSiteID}; falling back to all tenants.`);
        }

        // Fallback: show all tenants if no site is preselected
        tenantsForSite = state.tenants;
    }

    // Populate the org dropdown with filtered tenants
    for (const tenant of tenantsForSite) {
                const option = document.createElement('option');
                option.value = tenant.orgID;
                option.textContent = `${tenant.name} (${tenant.orgID})`;
        orgSelect.appendChild(option);
    }

    // Add event listener for org changes
    orgSelect.addEventListener('change', async function() {
        const vpcSelect = document.getElementById('vpc-id-select');
        
        resetNodePoolDependentFields();
        if (state.cpControllerMode) {
            resetCreateClusterCPForgeFields();
            resetCreateClusterCPSubnetFields();
        }
        
        vpcSelect.innerHTML = '<option value="">Select a VPC</option>';
        vpcSelect.disabled = !this.value;

        if (this.value && preSelectedSiteID) {
            vpcSelect.innerHTML = '<option value="">Loading VPCs...</option>';
            
            try {
                const forgeData = await loadForgeData(this.value, preSelectedSiteID);
                
                // Update VPCs
                vpcSelect.innerHTML = '<option value="">Select a VPC</option>';
                for (const vpc of forgeData.vpcs) {
                    const option = document.createElement('option');
                    option.value = vpc.id;
                    option.textContent = vpc.name;
                    vpcSelect.appendChild(option);
                }
                vpcSelect.disabled = false;

                // Auto-select if there's only one VPC
                if (forgeData.vpcs.length === 1) {
                    vpcSelect.value = forgeData.vpcs[0].id;
                    // Trigger the VPC change event to load subnets
                    vpcSelect.dispatchEvent(new Event('change'));
                }

                // Update node pools with site-specific data
                updateNodePoolsWithForgeData(forgeData);
                if (state.cpControllerMode) {
                    populateCreateClusterCPForgeData(forgeData);
                }
                
            } catch (error) {
                console.error('Error fetching VPCs and site data:', error);
                vpcSelect.innerHTML = '<option value="">Error loading VPCs</option>';
            }
        }
    });

    document.getElementById('vpc-id-select').addEventListener('change', async function() {
        const orgSelect = document.getElementById('org-id-select');
        
        resetNodePoolSubnetFields();
        if (state.cpControllerMode) {
            resetCreateClusterCPSubnetFields();
        }

        if (this.value && preSelectedSiteID && orgSelect.value) {
            try {
                const forgeData = await loadForgeData(orgSelect.value, preSelectedSiteID, this.value);
                updateNodePoolsWithSubnetData(forgeData.subnets, forgeData.vpcPrefixes, forgeData.instances);
                if (state.cpControllerMode) {
                    updateCreateClusterCPSubnetData(forgeData.subnets, forgeData.vpcPrefixes, forgeData.instances);
                }
            } catch (error) {
                console.error('Error fetching subnets/VPC prefixes and instances:', error);
            }
        }
    });

    // Auto-select if there's only one tenant for this site
    // (Do this AFTER event listeners are set up so the change event is handled)
    if (tenantsForSite.length === 1) {
        orgSelect.value = tenantsForSite[0].orgID;
        // Trigger the org change event to load VPCs
        orgSelect.dispatchEvent(new Event('change'));
    }
}

function updateNodePoolsWithForgeData(forgeData) {
        const nodePoolConfigs = document.querySelectorAll('.nodepool-config');

        nodePoolConfigs.forEach(config => {
            const idx = config.dataset.idx;

        // Update instance types
        const instanceTypeSelect = document.getElementById(`nodepool-${idx}-instance-type-id`);
        if (instanceTypeSelect) {
            instanceTypeSelect.innerHTML = '<option value="">Select an instance type</option>';
            
            for (const instanceType of forgeData.instanceTypes) {
                const option = document.createElement('option');
                option.value = instanceType.id;
                
                option.textContent = instanceType.name + formatInstanceTypeAvailabilitySuffix(instanceType);
                instanceTypeSelect.appendChild(option);
            }
            instanceTypeSelect.disabled = false;
        }

        // Update InfiniBand Partitions
        const infiniBandSelect = document.getElementById(`nodepool-${idx}-infiniband-partition-id`);
        if (infiniBandSelect) {
            infiniBandSelect.innerHTML = '<option value="">None</option>';
            
            for (const partition of forgeData.infiniBandPartitions) {
                const option = document.createElement('option');
                option.value = partition.id;
                option.textContent = partition.name;
                infiniBandSelect.appendChild(option);
            }
            infiniBandSelect.disabled = false;
        }

        // Update NVLink Logical Partitions
        const nvLinkLogicalPartitionSelect = document.getElementById(`nodepool-${idx}-nvlink-logical-partition-id`);
        if (nvLinkLogicalPartitionSelect) {
            nvLinkLogicalPartitionSelect.innerHTML = '<option value="">None</option>';

            for (const partition of forgeData.nvLinkLogicalPartitions || []) {
                const option = document.createElement('option');
                option.value = partition.id;
                option.textContent = partition.name;
                nvLinkLogicalPartitionSelect.appendChild(option);
            }
            nvLinkLogicalPartitionSelect.disabled = false;
        }

        // Update SSH Key Groups
        const sshContainer = document.getElementById(`nodepool-${idx}-ssh-key-groups-container`);
        if (sshContainer) {
            sshContainer.innerHTML = '';
            
            if (forgeData.sshKeyGroups.length === 0) {
                sshContainer.innerHTML = '<div class="empty-text">No SSH key groups available</div>';
                sshContainer.classList.add('disabled');
            } else {
                // Remove disabled styling when SSH key groups are available
                sshContainer.classList.remove('disabled');
                
                forgeData.sshKeyGroups.forEach(group => {
                    const itemDiv = document.createElement('div');
                    itemDiv.className = 'ssh-key-group-item';
                    
                    const checkbox = document.createElement('input');
                    checkbox.type = 'checkbox';
                    checkbox.id = `nodepool-${idx}-ssh-key-group-${group.id}`;
                    checkbox.value = group.id;
                    checkbox.name = `nodepool-${idx}-ssh-key-groups`;
                    
                    const label = document.createElement('label');
                    label.htmlFor = checkbox.id;
                    label.textContent = group.name;
                    
                    itemDiv.appendChild(checkbox);
                    itemDiv.appendChild(label);
                    sshContainer.appendChild(itemDiv);
                });
            }
        }
    });
}

function updateNodePoolsWithSubnetData(subnets, vpcPrefixes, instances) {
    const nodePoolConfigs = document.querySelectorAll('.nodepool-config');
    const tempForgeData = { subnets, instances };
    
    // Get the selected site to check capabilities
    const hiddenSiteInput = document.getElementById('hidden-site-id');
    const siteID = hiddenSiteInput ? hiddenSiteInput.value : null;
    const forgeSite = siteID ? state.forgeSites.find(fs => fs.id === siteID) : null;
    const isNativeNetworkingEnabled = forgeSite?.capabilities?.nativeNetworking || false;
    
    const itemsToShow = isNativeNetworkingEnabled ? vpcPrefixes : subnets;
    const labelText = isNativeNetworkingEnabled ? 'VPC Prefixes' : 'Subnets';
    const emptyText = isNativeNetworkingEnabled ? 'VPC prefixes' : 'subnets';
    const checkboxNameSuffix = isNativeNetworkingEnabled ? 'vpc-prefixes' : 'subnets';

    nodePoolConfigs.forEach(config => {
        const idx = config.dataset.idx;
        const subnetsContainer = document.getElementById(`nodepool-${idx}-subnets-container`);
        const labelElement = subnetsContainer?.parentElement?.querySelector('label');
        
        if (subnetsContainer) {
            // Update label
            if (labelElement) {
                labelElement.textContent = labelText + ':';
            }
            
            subnetsContainer.innerHTML = '';
            if (itemsToShow.length === 0) {
                subnetsContainer.innerHTML = `<div class="empty-text">No ${emptyText} available</div>`;
                subnetsContainer.classList.add('disabled');
            } else {
                // Remove disabled styling when resources are available
                subnetsContainer.classList.remove('disabled');
                subnetsContainer.title = '';
                
                itemsToShow.forEach(item => {
                    const itemDiv = document.createElement('div');
                    itemDiv.className = 'subnet-item';
                    
                    const checkbox = document.createElement('input');
                    checkbox.type = 'checkbox';
                    checkbox.id = `nodepool-${idx}-${checkboxNameSuffix}-${item.id}`;
                    checkbox.value = item.id;
                    checkbox.name = `nodepool-${idx}-${checkboxNameSuffix}`;
                    
                    let availability = { available: -1, total: '?' };
                    let availabilityText = '';
                    if (isNativeNetworkingEnabled) {
                        availability = calculateVpcPrefixIpAvailability(item.id, tempForgeData);
                    } else {
                        availability = calculateSubnetAvailability(item.id, tempForgeData);
                    }
                    if (availability.total !== '?') {
                        availabilityText = ` (${availability.available}/${availability.total} IPs available)`;
                    }
                    
                    const label = document.createElement('label');
                    label.htmlFor = checkbox.id;
                    label.textContent = item.name + availabilityText;
                    
                    itemDiv.appendChild(checkbox);
                    itemDiv.appendChild(label);
                    subnetsContainer.appendChild(itemDiv);
                });
            }
        }
    });
}

function getOrgIdValue() {
    return document.getElementById('org-id-select').value;
}

function getSiteIdValue() {
    const hiddenSiteInput = document.getElementById('hidden-site-id');
    return hiddenSiteInput ? hiddenSiteInput.value : '';
}

function getVpcIdValue() {
    return document.getElementById('vpc-id-select').value;
}

// Helper function to generate HTML for node pool form fields
function _generateNodePoolFormFieldsHTML(idPrefix, namePrefixValue = '', desiredNodeCountValue = 0, isControlPlaneChecked = false, descriptionValue = '', autoProvisionDisabledValue = false, nameValue = '', systemReservedMemoryValue = '', autoModeDisabledValue = false, recycleBudgetValue = 0, diskSelectionStrategyValue = 'smallest', isCAPICluster = createClusterUsesCAPIInfrastructure(), capiSpireAttestationMethodValue = SPIRE_ATTESTATION_METHOD_TPM, capiBootstrapFilesValue = '', capiPreKubeadmCommandsValue = '', kubeReservedMemoryValue = '') {
    const controlPlaneCheckedAttr = isControlPlaneChecked ? 'checked' : '';
    const autoProvisionDisabledAttr = autoProvisionDisabledValue ? 'checked' : '';
    const autoModeDisabledAttr = autoModeDisabledValue ? 'checked' : '';
    const normalizedSpireAttestationMethod = normalizeSpireAttestationMethod(capiSpireAttestationMethodValue);
    const capiSpireAttestationMethodJoinTokenSelectedAttr = normalizedSpireAttestationMethod === SPIRE_ATTESTATION_METHOD_JOIN_TOKEN ? 'selected' : '';
    const capiSpireAttestationMethodTpmSelectedAttr = normalizedSpireAttestationMethod === SPIRE_ATTESTATION_METHOD_TPM ? 'selected' : '';
    
    // Generate OS profile options immediately
    let osProfileOptions = '<option value="">Select an OS profile</option>';
    if (state.osProfiles && state.osProfiles.length > 0) {
        for (const profile of state.osProfiles) {
            osProfileOptions += `<option value="${profile.name}">${profile.name}</option>`;
        }
    }
    
    // Generate config profile options immediately  
    let configProfileOptions = `<option value="">${isCAPICluster ? 'None' : 'Select a config profile'}</option>`;
    if (state.configProfiles && state.configProfiles.length > 0) {
        for (const profile of state.configProfiles) {
            configProfileOptions += `<option value="${profile.name}">${profile.name}</option>`;
        }
    }
    
    return `
        <div class="form-group">
            <label for="${idPrefix}name">Name:</label>
            <input type="text" id="${idPrefix}name" value="${nameValue}" placeholder="Enter a name for this node pool" required>
        </div>
        <div class="form-group control-plane-group"${state.cpControllerMode ? ' style="display: none;"' : ''}>
            <label for="${idPrefix}control-plane">Control Plane:</label>
            <div class="checkbox-input-container">
                <input type="checkbox" id="${idPrefix}control-plane" ${controlPlaneCheckedAttr}>
            </div>
        </div>
        <div class="form-group">
            <label for="${idPrefix}name-prefix">Node Name Prefix:</label>
            <input type="text" id="${idPrefix}name-prefix" value="${namePrefixValue}" placeholder="Node Name = <cluster-name>-<node-name-prefix>-<random-4-digits>" required>
        </div>
        <div class="form-group capi-only-field" style="display: none;">
            <label for="${idPrefix}capi-infra-machine-template-yaml">Infra Machine Template:</label>
            <textarea id="${idPrefix}capi-infra-machine-template-yaml" class="manifest-textarea" placeholder="Paste infra machine template manifest as YAML"></textarea>
        </div>
        <div class="form-group capi-only-field" style="display: none;">
            <label for="${idPrefix}capi-spire-attestation-method">SPIRE Attestation Method:</label>
            <div class="input-area-content">
                <select id="${idPrefix}capi-spire-attestation-method">
                    <option value="${SPIRE_ATTESTATION_METHOD_TPM}" ${capiSpireAttestationMethodTpmSelectedAttr}>TPM</option>
                    <option value="${SPIRE_ATTESTATION_METHOD_JOIN_TOKEN}" ${capiSpireAttestationMethodJoinTokenSelectedAttr}>Join Token</option>
                </select>
                <span class="help-text">Join token is insecure and intended for dev/test only.</span>
            </div>
        </div>
        <div class="form-group capi-only-field" style="display: none;">
            <label for="${idPrefix}capi-bootstrap-files-yaml">Bootstrap Files:</label>
            <div class="input-area-content">
                <textarea id="${idPrefix}capi-bootstrap-files-yaml" class="manifest-textarea" placeholder="Paste bootstrap files as a YAML array of CloudInitFile objects">${capiBootstrapFilesValue}</textarea>
                <span class="help-text">Additional cloud-init write_files entries for CAPI-backed nodes.</span>
            </div>
        </div>
        <div class="form-group capi-only-field" style="display: none;">
            <label for="${idPrefix}capi-pre-kubeadm-commands-yaml">Pre-Kubeadm Commands:</label>
            <div class="input-area-content">
                <textarea id="${idPrefix}capi-pre-kubeadm-commands-yaml" class="manifest-textarea" placeholder="Paste pre-kubeadm commands as a YAML array of strings">${capiPreKubeadmCommandsValue}</textarea>
                <span class="help-text">Additional cloud-init runcmd entries that run before kubeadm.</span>
            </div>
        </div>
        <div class="form-group subnets-form-group forge-only-field">
            <label for="${idPrefix}subnets-container">Subnets:</label>
            <div id="${idPrefix}subnets-container" class="custom-multiselect-container" title="You must select a VPC first" style="background: #2a2a2a; border-color: #444; color: #999; cursor: not-allowed;">
                <div class="empty-text">Select subnets</div>
            </div>
        </div>
        <div class="form-group forge-only-field">
            <label for="${idPrefix}instance-type-id">Instance Type:</label>
            <div class="select-input-container">
                <select id="${idPrefix}instance-type-id" title="You must select a tenant first" disabled>
                    <option value="">Select an instance type</option>
                </select>
            </div>
        </div>
        <div class="form-group forge-only-field">
            <label for="${idPrefix}infiniband-partition-id">InfiniBand Partition:</label>
            <div class="select-input-container">
                <select id="${idPrefix}infiniband-partition-id" title="You must select a tenant first" disabled>
                    <option value="">Select an InfiniBand Partition</option>
                </select>
            </div>
        </div>
        <div class="form-group forge-only-field">
            <label for="${idPrefix}nvlink-logical-partition-id">NVLink Logical Partition:</label>
            <div class="select-input-container">
                <select id="${idPrefix}nvlink-logical-partition-id" title="You must select a tenant first" disabled>
                    <option value="">Select an NVLink Logical Partition</option>
                </select>
            </div>
        </div>
        <div class="form-group">
            <label for="${idPrefix}os-profile">OS Profile:</label>
            <select id="${idPrefix}os-profile" ${isCAPICluster ? '' : 'required'}>
                ${osProfileOptions}
            </select>
        </div>
        <div class="form-group">
            <label for="${idPrefix}config-profile">Config Profile:</label>
            <select id="${idPrefix}config-profile" ${isCAPICluster ? '' : 'required'}>
                ${configProfileOptions}
            </select>
        </div>
        <div class="form-group">
            <label for="${idPrefix}system-reserved-memory">System Reserved Memory:</label>
            <input type="text" id="${idPrefix}system-reserved-memory" value="${systemReservedMemoryValue}" placeholder="Optional (default: 16Gi)" pattern="^[0-9]+(Gi|Mi|G|M)$" title="Optional. Memory value with unit (e.g., 16Gi, 128Gi, 256Gi). Defaults to 16Gi if not specified.">
        </div>
        <div class="form-group">
            <label for="${idPrefix}kube-reserved-memory">Kube Reserved Memory:</label>
            <input type="text" id="${idPrefix}kube-reserved-memory" value="${kubeReservedMemoryValue}" placeholder="Optional (default: 2Gi)" pattern="^[0-9]+(Gi|Mi|G|M)$" title="Optional. Memory value with unit (e.g., 2Gi, 250Mi). Uses the kubelet configuration patch default (2Gi) if not specified.">
        </div>
        <div class="form-group ssh-key-groups-form-group forge-only-field">
            <label for="${idPrefix}ssh-key-groups-container">SSH Key Groups:</label>
            <div id="${idPrefix}ssh-key-groups-container" class="custom-multiselect-container" title="You must select a tenant first" style="background: #2a2a2a; border-color: #444; color: #999; cursor: not-allowed;">
                <div class="empty-text">Select SSH key groups</div>
            </div>
        </div>
        <div class="form-group">
            <label for="${idPrefix}desired-node-count">Desired Node Count:</label>
            <input type="number" id="${idPrefix}desired-node-count" value="${desiredNodeCountValue}" min="0" required>
        </div>
        <div class="form-group">
            <label for="${idPrefix}recycle-budget">Recycle Budget:</label>
            <input type="number" id="${idPrefix}recycle-budget" value="${recycleBudgetValue}" min="0">
        </div>
        <div class="form-group forge-only-field">
            <label for="${idPrefix}disk-selection-strategy">Disk Selection Strategy:</label>
            <select id="${idPrefix}disk-selection-strategy">
                <option value="smallest" ${diskSelectionStrategyValue === 'smallest' ? 'selected' : ''}>Smallest</option>
                <option value="largest" ${diskSelectionStrategyValue === 'largest' ? 'selected' : ''}>Largest</option>
            </select>
        </div>
        <div class="form-group">
            <label for="${idPrefix}auto-provision-disabled">Auto-Provision Disabled:</label>
            <div class="checkbox-input-container">
                <input type="checkbox" id="${idPrefix}auto-provision-disabled" ${autoProvisionDisabledAttr}>
                <span class="help-text">Disable automatic provisioning for this node pool</span>
            </div>
        </div>
        <div class="form-group">
            <label for="${idPrefix}auto-mode-disabled">Auto-Mode Disabled:</label>
            <div class="checkbox-input-container">
                <input type="checkbox" id="${idPrefix}auto-mode-disabled" ${autoModeDisabledAttr}>
                <span class="help-text">Disable automatic node recycling for this node pool</span>
            </div>
        </div>
        <div class="form-group">
            <label for="${idPrefix}description">Description:</label>
            <textarea id="${idPrefix}description" placeholder="Optional description">${descriptionValue}</textarea>
        </div>
        <div class="validation-warnings" id="${idPrefix}warnings"></div>
    `;
}

// Add a new node pool configuration to the form
function addNodePoolConfig(name, nodeCount, isControlPlane, namePrefix = null) {
    const nodePoolsList = document.getElementById('create-cluster-nodepools-list');
    const nodePoolIdx = document.querySelectorAll('.nodepool-config').length;

    // Use provided namePrefix or fallback to name or empty string
    const defaultPrefix = namePrefix || name || '';

    const nodePoolConfig = document.createElement('div');
    nodePoolConfig.className = 'nodepool-config';
    nodePoolConfig.dataset.idx = nodePoolIdx;

    // Generate the common form fields using the helper function
    const isCAPICluster = createClusterUsesCAPIInfrastructure();
    const formFieldsHTML = _generateNodePoolFormFieldsHTML(
        `nodepool-${nodePoolIdx}-`, 
        defaultPrefix, 
        nodeCount, // desiredNodeCount
        isControlPlane, 
        '', // description
        false, // autoProvisionDisabled
        name, // name value
        '',
        false,
        0,
        'smallest',
        isCAPICluster,
        SPIRE_ATTESTATION_METHOD_TPM
    );

    nodePoolConfig.innerHTML = `
        <div class="nodepool-config-header">
            <!-- <div class="nodepool-config-title">Node Pool ${nodePoolIdx + 1}</div> -->
            <div class="nodepool-config-actions">
                <button type="button" class="delete-btn" onclick="removeNodePoolConfig(${nodePoolIdx})" title="Remove Node Pool Configuration">Remove Node Pool</button>
            </div>
        </div>
        ${formFieldsHTML}
    `;

    nodePoolsList.appendChild(nodePoolConfig);

    // Trigger any updates needed based on the current state of the org/site/vpc
    updateNodePoolSelectionsFromParent(nodePoolIdx);

    applyNodePoolInfrastructureFields(nodePoolConfig, isCAPICluster);

    // Return the new nodepool index for any additional operations
    return nodePoolIdx;
}

// Remove a node pool configuration from the create cluster form
function removeNodePoolConfig(nodePoolIdx) {
    const nodePoolConfigToRemove = document.querySelector(`.nodepool-config[data-idx="${nodePoolIdx}"]`);
    if (nodePoolConfigToRemove) {
        nodePoolConfigToRemove.remove();

        // Re-index the remaining nodepools
        const remainingNodeConfigs = document.querySelectorAll('.nodepool-config');
        remainingNodeConfigs.forEach((config, newInternalIdx) => { // newInternalIdx is the new DOM order index
            const oldInternalDataIdx = parseInt(config.dataset.idx, 10); // Get the previous data-idx

            config.dataset.idx = newInternalIdx; // Update data-idx to its new position

            // Update the remove button to point to the new index
            const removeButton = config.querySelector('.nodepool-config-actions button');
            if (removeButton) {
                removeButton.setAttribute('onclick', `removeNodePoolConfig(${newInternalIdx})`);
            }

            // List of field definitions: { baseId: "name-prefix", elementType: "input" (or "select", "textarea", "div") }
            const fieldsToUpdateConf = [
                { baseIdPart: 'name', query: 'input[type="text"]' },
                { baseIdPart: 'control-plane', query: 'input[type="checkbox"]' },
                { baseIdPart: 'name-prefix', query: 'input[type="text"]' },
                { baseIdPart: 'subnet-id', query: 'select' },
                { baseIdPart: 'instance-type-id', query: 'select' },
                { baseIdPart: 'infiniband-partition-id', query: 'select' },
                { baseIdPart: 'nvlink-logical-partition-id', query: 'select' },
                { baseIdPart: 'os-profile', query: 'select' },
                { baseIdPart: 'config-profile', query: 'select' },
                { baseIdPart: 'capi-infra-machine-template-yaml', query: 'textarea' },
                { baseIdPart: 'capi-spire-attestation-method', query: 'select' },
                { baseIdPart: 'capi-bootstrap-files-yaml', query: 'textarea' },
                { baseIdPart: 'capi-pre-kubeadm-commands-yaml', query: 'textarea' },
                { baseIdPart: 'system-reserved-memory', query: 'input[type="text"]' },
                { baseIdPart: 'desired-node-count', query: 'input[type="number"]' },
                { baseIdPart: 'description', query: 'textarea' },
                { baseIdPart: 'warnings', query: 'div.validation-warnings' } // More specific selector for warnings div
            ];

            fieldsToUpdateConf.forEach(fieldConf => {
                // Construct the old ID pattern based on oldInternalDataIdx. This assumes IDs were correctly set before.
                // However, it's safer to find the element by a more stable part of its ID or structure within `config`,
                // especially if this function is called multiple times or state is complex.
                // Let's try to find the element using a partial ID match that doesn't rely on the old index being perfect.
                const element = config.querySelector(`${fieldConf.query}[id$="-${fieldConf.baseIdPart}"]`);

                if (element) {
                    const oldElementId = element.id;
                    const newElementId = `nodepool-${newInternalIdx}-${fieldConf.baseIdPart}`;
                    element.id = newElementId;

                    // Attempt to find the label. Labels might be direct children or elsewhere, be careful.
                    // Assuming label is sibling or in a structure where `closest('.form-group').querySelector('label')` works
                    let labelForElement = null;
                    const formGroup = element.closest('.form-group');
                    if (formGroup) {
                        labelForElement = formGroup.querySelector(`label[for="${oldElementId}"]`);
                    }
                    if (!labelForElement) {
                        labelForElement = document.querySelector(`label[for="${oldElementId}"]`); 
                    }

                    if (labelForElement) {
                        labelForElement.htmlFor = newElementId;
                    }
                }
            });

            // Handle SSH Key Groups container and its contents
            const sshKeyGroupsContainer = config.querySelector(`div[id$="-ssh-key-groups-container"]`);
            if (sshKeyGroupsContainer) {
                const oldSshContainerId = sshKeyGroupsContainer.id;
                const newSshContainerId = `nodepool-${newInternalIdx}-ssh-key-groups-container`;
                sshKeyGroupsContainer.id = newSshContainerId;

                let labelForSshContainer = null;
                const formGroupSsh = sshKeyGroupsContainer.closest('.form-group');
                 if (formGroupSsh) {
                    labelForSshContainer = formGroupSsh.querySelector(`label[for="${oldSshContainerId}"]`);
                }
                if (!labelForSshContainer) {
                     labelForSshContainer = document.querySelector(`label[for="${oldSshContainerId}"]`);
                }
                if (labelForSshContainer) {
                    labelForSshContainer.htmlFor = newSshContainerId;
                }

                const checkboxes = sshKeyGroupsContainer.querySelectorAll('input[type="checkbox"]');
                checkboxes.forEach(checkbox => {
                    const oldCheckboxId = checkbox.id;
                    // oldCheckboxId is like nodepool-OLD_IDX-ssh-key-group-ACTUAL_GROUP_ID
                    // We need to preserve the ACTUAL_GROUP_ID part.
                    const idSuffix = oldCheckboxId.substring(oldCheckboxId.indexOf('-ssh-key-group-')); 
                    const newCheckboxId = `nodepool-${newInternalIdx}${idSuffix}`;
                    checkbox.id = newCheckboxId;
                    checkbox.name = `nodepool-${newInternalIdx}-ssh-key-groups`; // Update name attribute as well

                    const checkboxLabel = sshKeyGroupsContainer.querySelector(`label[for="${oldCheckboxId}"]`);
                    if (checkboxLabel) {
                        checkboxLabel.htmlFor = newCheckboxId;
                    }
                });
            }
        });

        updateClusterManifestRequirement();
    }
}

function getSelectedSiteNativeNetworking() {
    const hiddenSiteInput = document.getElementById('hidden-site-id');
    const siteID = hiddenSiteInput ? hiddenSiteInput.value : null;
    const forgeSite = siteID ? state.forgeSites.find(fs => fs.id === siteID) : null;
    return forgeSite?.capabilities?.nativeNetworking || false;
}

function getCreateClusterFormData() {
    const name = document.getElementById('cluster-name').value;

    const isCAPICluster = createClusterUsesCAPIInfrastructure();
    const orgID = isCAPICluster ? '' : getOrgIdValue();
    const siteID = getSiteIdValue();
    const vpcID = isCAPICluster ? '' : getVpcIdValue();
    const controlPlaneKubernetesVersion = document.getElementById('k8s-version').value;
    const cniPlugin = document.getElementById('cni-plugin').value || 'calico';
    const cniVersion = document.getElementById('cni-version').value.trim();
    const podCIDR = document.getElementById('pod-cidr').value.trim();
    const serviceCIDR = document.getElementById('service-cidr').value.trim();
    const controlPlaneVIP = document.getElementById('control-plane-vip').value.trim();
    const disableEtcdEncryption = document.getElementById('disable-etcd-encryption').checked;

    const nodePools = [];
    const nodePoolConfigs = document.querySelectorAll('.nodepool-config');

    nodePoolConfigs.forEach(config => {
        const idx = config.dataset.idx;
        const name = document.getElementById(`nodepool-${idx}-name`).value;
        const namePrefix = document.getElementById(`nodepool-${idx}-name-prefix`).value;
        const description = document.getElementById(`nodepool-${idx}-description`).value.trim();

        const isNativeNetworkingEnabled = !isCAPICluster && getSelectedSiteNativeNetworking();
        const checkboxes = document.querySelectorAll(`#nodepool-${idx}-subnets-container input[type="checkbox"]:checked`);
        const selectedIDs = Array.from(checkboxes).map(cb => cb.value);
        const subnetIDs = isCAPICluster ? [] : (isNativeNetworkingEnabled ? [] : selectedIDs);
        const vpcPrefixIDs = isCAPICluster ? [] : (isNativeNetworkingEnabled ? selectedIDs : []);
        
        const instanceTypeID = isCAPICluster ? '' : document.getElementById(`nodepool-${idx}-instance-type-id`).value;
        const infinibandPartitionID = isCAPICluster ? '' : document.getElementById(`nodepool-${idx}-infiniband-partition-id`).value;
        const nvLinkLogicalPartitionID = isCAPICluster ? '' : document.getElementById(`nodepool-${idx}-nvlink-logical-partition-id`).value;
        const osProfile = document.getElementById(`nodepool-${idx}-os-profile`).value;
        const configProfile = document.getElementById(`nodepool-${idx}-config-profile`).value;
        const systemReservedMemory = document.getElementById(`nodepool-${idx}-system-reserved-memory`).value;
        const kubeReservedMemory = document.getElementById(`nodepool-${idx}-kube-reserved-memory`).value;

        const sshKeyGroupCheckboxes = document.querySelectorAll(`#nodepool-${idx}-ssh-key-groups-container input[type="checkbox"]:checked`);
        const sshKeyGroups = isCAPICluster ? [] : Array.from(sshKeyGroupCheckboxes).map(cb => cb.value);
        const capiInfraMachineTemplate = isCAPICluster
            ? parseYAMLManifestToJSONObject(
                document.getElementById(`nodepool-${idx}-capi-infra-machine-template-yaml`).value,
                `Node pool "${name || `#${parseInt(idx, 10) + 1}`}" CAPI infra machine template`
            )
            : undefined;
        const capiSpireAttestationMethod = isCAPICluster
            ? getSpireAttestationMethodFromInput(
                document.getElementById(`nodepool-${idx}-capi-spire-attestation-method`)
            )
            : undefined;
        const capiBootstrapFiles = isCAPICluster
            ? parseCapiBootstrapFilesInput(
                document.getElementById(`nodepool-${idx}-capi-bootstrap-files-yaml`).value,
                `Node pool "${name || `#${parseInt(idx, 10) + 1}`}" CAPI bootstrap files`
            )
            : undefined;
        const capiPreKubeadmCommands = isCAPICluster
            ? parseCapiPreKubeadmCommandsInput(
                document.getElementById(`nodepool-${idx}-capi-pre-kubeadm-commands-yaml`).value,
                `Node pool "${name || `#${parseInt(idx, 10) + 1}`}" CAPI pre-kubeadm commands`
            )
            : undefined;
        if (isCAPICluster && !capiInfraMachineTemplate) {
            throw new Error(`Node pool "${name || `#${parseInt(idx, 10) + 1}`}" requires a CAPI infra machine template`);
        }
        
        const desiredNodeCount = parseInt(document.getElementById(`nodepool-${idx}-desired-node-count`).value, 10) || 0;
        // In cpControllerMode the create form only produces worker pools; the control
        // plane is configured via the cluster-level controlPlaneConfig instead.
        const isControlPlane = state.cpControllerMode
            ? false
            : document.getElementById(`nodepool-${idx}-control-plane`).checked;
        const autoProvisionDisabled = document.getElementById(`nodepool-${idx}-auto-provision-disabled`).checked;
        const autoModeDisabled = document.getElementById(`nodepool-${idx}-auto-mode-disabled`).checked;
        const recycleBudget = parseInt(document.getElementById(`nodepool-${idx}-recycle-budget`).value, 10) || 0;
        const diskSelectionStrategyEl = document.getElementById(`nodepool-${idx}-disk-selection-strategy`);
        const diskSelectionStrategy = diskSelectionStrategyEl ? diskSelectionStrategyEl.value : 'smallest';

        const nodePool = {
            name: name,
            nodeNamePrefix: namePrefix,
            description: description || undefined,
            configProfile: configProfile || undefined,
            systemReservedMemory: systemReservedMemory || undefined,
            kubeReservedMemory: kubeReservedMemory || undefined,
            desiredNodeCount: desiredNodeCount,
            controlPlane: isControlPlane,
            autoProvisionDisabled: autoProvisionDisabled,
            autoModeDisabled: autoModeDisabled,
            recycleBudget: recycleBudget,
        };

        if (isCAPICluster) {
            nodePool.capiInfraMachineTemplate = capiInfraMachineTemplate || undefined;
            nodePool.capiSpireAttestationMethod = capiSpireAttestationMethod || undefined;
            nodePool.capiBootstrapFiles = capiBootstrapFiles;
            nodePool.capiPreKubeadmCommands = capiPreKubeadmCommands;
        } else {
            nodePool.diskSelectionStrategy = diskSelectionStrategy;
            nodePool.subnetIDs = subnetIDs;
            nodePool.vpcPrefixIDs = vpcPrefixIDs;
            nodePool.instanceTypeID = instanceTypeID || undefined;
            nodePool.infinibandPartitionID = infinibandPartitionID || undefined;
            nodePool.nvLinkLogicalPartitionID = nvLinkLogicalPartitionID || undefined;
            nodePool.osProfile = osProfile || undefined;
            nodePool.sshKeyGroupIDs = sshKeyGroups;
        }

        nodePools.push(nodePool);
    });

    const capiInfraClusterManifest = isCAPICluster
        ? parseYAMLManifestToJSONObject(
            document.getElementById('capi-infra-cluster-yaml').value,
            'CAPI infra cluster'
        )
        : undefined;
    if (isCAPICluster && !capiInfraClusterManifest) {
        throw new Error('CAPI infra cluster is required for CAPI-backed clusters');
    }

    // In legacy mode a cluster must have a control-plane node pool; default the first
    // pool to control plane if none was flagged. In cpControllerMode the control plane
    // comes from the cluster-level controlPlaneConfig, so all node pools stay workers.
    if (!state.cpControllerMode) {
        const hasControlPlane = nodePools.some(pool => pool.controlPlane);
        if (!hasControlPlane && nodePools.length > 0) {
            nodePools[0].controlPlane = true;
        }
    }

    const effectivePodCIDR = podCIDR || DEFAULT_CLUSTER_POD_CIDR;
    const podCIDRCapacity = podCIDRNodeBlockCapacity(effectivePodCIDR);
    let desiredNodes = nodePools.reduce((total, nodePool) => total + nodePool.desiredNodeCount, 0);
    // In cpControllerMode the control-plane nodes come from controlPlaneConfig (not a node
    // pool), so include their desired count in the pod-CIDR capacity check.
    if (state.cpControllerMode) {
        const cpDesiredCount = parseInt(document.getElementById('cp-config-desired-count')?.value, 10);
        const cpMaxSurge = parseInt(document.getElementById('cp-config-max-surge')?.value, 10);
        if (!Number.isNaN(cpDesiredCount) && cpDesiredCount > 0) {
            desiredNodes += cpDesiredCount;
        }
        // CP surge nodes also consume pod-CIDR blocks, so count them here.
        if (!Number.isNaN(cpMaxSurge) && cpMaxSurge > 0) {
            desiredNodes += cpMaxSurge;
        }
    }
    const requiredNodeBlocks = desiredNodes + MINIMUM_POD_CIDR_HEADROOM;
    if (podCIDRCapacity !== null && podCIDRCapacity < requiredNodeBlocks) {
        throw new Error(
            `Pod CIDR ${effectivePodCIDR} provides ${podCIDRCapacity} /24 node blocks, ` +
            `but ${requiredNodeBlocks} are required for ${desiredNodes} desired nodes plus ` +
            `${MINIMUM_POD_CIDR_HEADROOM} surge/headroom; choose a larger Pod CIDR`
        );
    }

    const result = {
        name: name,
        controlPlaneKubernetesVersion,
        cniPlugin,
        etcdEncrypted: !disableEtcdEncryption,
        nodePools
    };

    if (cniVersion) {
        result.cniVersion = cniVersion;
    }
    if (controlPlaneVIP) {
        result.controlPlaneVIP = controlPlaneVIP;
    }

    // Only include orgID, siteID, and vpcID when they are set.
    if (orgID) {
        result.orgID = orgID;
    }
    if (siteID) {
        result.siteID = siteID;
    }
    if (vpcID) {
        result.vpcID = vpcID;
    }
    if (capiInfraClusterManifest) {
        result.capiInfraCluster = capiInfraClusterManifest;
    }
    if (podCIDR || serviceCIDR) {
        result.network = {};
        if (podCIDR) {
            result.network.podCIDR = podCIDR;
        }
        if (serviceCIDR) {
            result.network.serviceCIDR = serviceCIDR;
        }
    }

    // Cluster-level-CP mode: attach the top-level controlPlaneConfig (the server
    // provisions the control plane from this instead of a controlPlane node pool).
    if (state.cpControllerMode) {
        result.controlPlaneConfig = getCreateClusterControlPlaneConfigData(isCAPICluster);
    }

    return result;
}

function renderCreateClusterLoading() {
    document.getElementById('create-cluster-error').style.display = 'none';
    document.getElementById('create-cluster-loading').innerHTML = 'Creating cluster...';
    document.getElementById('create-cluster-loading').style.display = 'block';
}

function renderCreateClusterError(error) {
    document.getElementById('create-cluster-loading').style.display = 'none';
    document.getElementById('create-cluster-error').innerHTML = `Error creating cluster: ${error}`;
    document.getElementById('create-cluster-error').style.display = 'block';
}

function showDeleteClusterModal() {
    document.getElementById('delete-cluster-name').textContent = state.selectedClusterDetails.name;
    document.getElementById('delete-cluster-modal').style.display = 'block';
    document.getElementById('delete-cluster-error').style.display = 'none';
    document.getElementById('delete-cluster-loading').style.display = 'none';
}

function renderDeleteClusterLoading() {
    document.getElementById('delete-cluster-error').style.display = 'none';
    document.getElementById('delete-cluster-loading').innerHTML = 'Deleting cluster...';
    document.getElementById('delete-cluster-loading').style.display = 'block';
}

function renderDeleteClusterError(error) {
    document.getElementById('delete-cluster-loading').style.display = 'none';
    document.getElementById('delete-cluster-error').innerHTML = `Error deleting cluster: ${error}`;
    document.getElementById('delete-cluster-error').style.display = 'block';
}



function createInstanceTypePreferenceListElement(prefs) {
    const list = document.createElement('div');
    list.className = 'instance-type-preference-list';
    if (!prefs || prefs.length === 0) {
        const item = document.createElement('div');
        item.className = 'instance-type-preference-item';
        item.textContent = 'None';
        list.appendChild(item);
        return list;
    }
    prefs.forEach((id, index) => {
        const item = document.createElement('div');
        item.className = 'instance-type-preference-item';
        item.textContent = `${index + 1}. ${resolveInstanceTypeName(id)}${formatInstanceTypeAvailabilitySuffix(getInstanceTypeAvailability(id))}`;
        list.appendChild(item);
    });
    return list;
}
function formatClusterInstanceTypePreference(prefs) {
    if (!prefs || prefs.length === 0) {
        return 'None';
    }
    return prefs.map((id, index) => `${index + 1}. ${resolveInstanceTypeName(id)}`).join(', ');
}

function populateEditClusterInstanceTypeSelect() {
    const select = document.getElementById('edit-cluster-instance-type-add');
    if (!select) {
        return;
    }
    select.innerHTML = '<option value="">Select an instance type to add</option>';
    const instanceTypes = state.selectedClusterForgeData?.instanceTypes || [];
    for (const instanceType of instanceTypes) {
        const option = document.createElement('option');
        option.value = instanceType.id;
        option.textContent = instanceType.name + formatInstanceTypeAvailabilitySuffix(instanceType);
        select.appendChild(option);
    }
    select.disabled = instanceTypes.length === 0;
}

function renderEditClusterInstanceTypePreferenceList() {
    const list = document.getElementById('edit-cluster-instance-type-list');
    if (!list) {
        return;
    }
    list.innerHTML = '';
    if (editClusterInstanceTypePreferences.length === 0) {
        const emptyItem = document.createElement('li');
        emptyItem.textContent = 'No instance types configured';
        list.appendChild(emptyItem);
        return;
    }
    editClusterInstanceTypePreferences.forEach((instanceTypeID, index) => {
        const item = document.createElement('li');
        item.className = 'edit-cluster-instance-type-item';
        item.draggable = true;
        item.dataset.index = String(index);

        const dragHandle = document.createElement('span');
        dragHandle.className = 'edit-cluster-instance-type-drag-handle';
        dragHandle.textContent = '⋮⋮';
        dragHandle.title = 'Drag to reorder';
        dragHandle.setAttribute('aria-hidden', 'true');

        const label = document.createElement('span');
        label.className = 'edit-cluster-instance-type-label';
        label.textContent = `${index + 1}. ${resolveInstanceTypeName(instanceTypeID)}`;

        const actions = document.createElement('div');
        actions.className = 'edit-cluster-instance-type-actions';

        const upButton = document.createElement('button');
        upButton.type = 'button';
        upButton.className = 'edit-cluster-instance-type-reorder';
        upButton.textContent = '↑';
        upButton.title = 'Move up';
        upButton.disabled = index === 0;
        upButton.onclick = () => moveEditClusterInstanceTypePreferenceUp(index);

        const downButton = document.createElement('button');
        downButton.type = 'button';
        downButton.className = 'edit-cluster-instance-type-reorder';
        downButton.textContent = '↓';
        downButton.title = 'Move down';
        downButton.disabled = index === editClusterInstanceTypePreferences.length - 1;
        downButton.onclick = () => moveEditClusterInstanceTypePreferenceDown(index);

        const removeButton = document.createElement('button');
        removeButton.type = 'button';
        removeButton.textContent = 'Remove';
        removeButton.className = 'edit-cluster-instance-type-remove';
        removeButton.onclick = () => removeEditClusterInstanceTypePreference(index);

        actions.appendChild(upButton);
        actions.appendChild(downButton);
        actions.appendChild(removeButton);

        item.appendChild(dragHandle);
        item.appendChild(label);
        item.appendChild(actions);

        item.addEventListener('dragstart', (event) => {
            editClusterInstanceTypeDragIndex = index;
            item.classList.add('dragging');
            event.dataTransfer.effectAllowed = 'move';
            event.dataTransfer.setData('text/plain', String(index));
        });

        item.addEventListener('dragend', () => {
            editClusterInstanceTypeDragIndex = null;
            item.classList.remove('dragging');
            list.querySelectorAll('.edit-cluster-instance-type-item.drag-over').forEach((el) => {
                el.classList.remove('drag-over');
            });
        });

        item.addEventListener('dragover', (event) => {
            event.preventDefault();
            event.dataTransfer.dropEffect = 'move';
            if (editClusterInstanceTypeDragIndex !== null && editClusterInstanceTypeDragIndex !== index) {
                item.classList.add('drag-over');
            }
        });

        item.addEventListener('dragleave', () => {
            item.classList.remove('drag-over');
        });

        item.addEventListener('drop', (event) => {
            event.preventDefault();
            item.classList.remove('drag-over');
            const fromIndex = editClusterInstanceTypeDragIndex;
            if (fromIndex === null || fromIndex === index) {
                return;
            }
            moveEditClusterInstanceTypePreference(fromIndex, index);
            editClusterInstanceTypeDragIndex = null;
        });

        list.appendChild(item);
    });
}

function addEditClusterInstanceTypePreference() {
    const select = document.getElementById('edit-cluster-instance-type-add');
    if (!select || !select.value) {
        return;
    }
    if (editClusterInstanceTypePreferences.includes(select.value)) {
        return;
    }
    editClusterInstanceTypePreferences.push(select.value);
    select.value = '';
    renderEditClusterInstanceTypePreferenceList();
}

function removeEditClusterInstanceTypePreference(index) {
    editClusterInstanceTypePreferences.splice(index, 1);
    renderEditClusterInstanceTypePreferenceList();
}

// === CLUSTER-LEVEL CONTROL PLANE (cpControllerMode) SHARED HELPERS ===

// Parse a node-labels text input (newline and/or comma separated "key=value" entries)
// into an ordered array of strings, dropping blanks.
function parseNodeLabelsInput(text) {
    if (!text) {
        return [];
    }
    return text
        .split(/[\n,]+/)
        .map((label) => label.trim())
        .filter((label) => label.length > 0);
}

// Render a node-labels array back into the multiline text input representation.
function formatNodeLabelsForInput(labels) {
    return Array.isArray(labels) ? labels.join('\n') : '';
}

// === CREATE-CLUSTER CONTROL PLANE INSTANCE TYPE PREFERENCE ===

function moveCreateClusterCPInstanceTypePreference(fromIndex, toIndex) {
    if (fromIndex === toIndex || fromIndex < 0 || toIndex < 0) {
        return;
    }
    if (fromIndex >= createClusterCPInstanceTypePreferences.length || toIndex >= createClusterCPInstanceTypePreferences.length) {
        return;
    }
    const [item] = createClusterCPInstanceTypePreferences.splice(fromIndex, 1);
    createClusterCPInstanceTypePreferences.splice(toIndex, 0, item);
    renderCreateClusterCPInstanceTypePreferenceList();
}

function moveCreateClusterCPInstanceTypePreferenceUp(index) {
    if (index <= 0) {
        return;
    }
    moveCreateClusterCPInstanceTypePreference(index, index - 1);
}

function moveCreateClusterCPInstanceTypePreferenceDown(index) {
    if (index >= createClusterCPInstanceTypePreferences.length - 1) {
        return;
    }
    moveCreateClusterCPInstanceTypePreference(index, index + 1);
}

function addCreateClusterCPInstanceTypePreference() {
    const select = document.getElementById('cp-config-instance-type-add');
    if (!select || !select.value) {
        return;
    }
    if (createClusterCPInstanceTypePreferences.includes(select.value)) {
        return;
    }
    createClusterCPInstanceTypePreferences.push(select.value);
    select.value = '';
    renderCreateClusterCPInstanceTypePreferenceList();
}

function removeCreateClusterCPInstanceTypePreference(index) {
    createClusterCPInstanceTypePreferences.splice(index, 1);
    renderCreateClusterCPInstanceTypePreferenceList();
}

function populateCreateClusterCPInstanceTypeSelect(instanceTypes) {
    const select = document.getElementById('cp-config-instance-type-add');
    if (!select) {
        return;
    }
    select.innerHTML = '<option value="">Select an instance type to add</option>';
    const list = instanceTypes || [];
    for (const instanceType of list) {
        const option = document.createElement('option');
        option.value = instanceType.id;
        option.textContent = instanceType.name + formatInstanceTypeAvailabilitySuffix(instanceType);
        select.appendChild(option);
    }
    select.disabled = list.length === 0;
}

function renderCreateClusterCPInstanceTypePreferenceList() {
    const list = document.getElementById('cp-config-instance-type-list');
    if (!list) {
        return;
    }
    list.innerHTML = '';
    if (createClusterCPInstanceTypePreferences.length === 0) {
        const emptyItem = document.createElement('li');
        emptyItem.textContent = 'No instance types configured';
        list.appendChild(emptyItem);
        return;
    }
    createClusterCPInstanceTypePreferences.forEach((instanceTypeID, index) => {
        const item = document.createElement('li');
        item.className = 'edit-cluster-instance-type-item';
        item.draggable = true;
        item.dataset.index = String(index);

        const dragHandle = document.createElement('span');
        dragHandle.className = 'edit-cluster-instance-type-drag-handle';
        dragHandle.textContent = '⋮⋮';
        dragHandle.title = 'Drag to reorder';
        dragHandle.setAttribute('aria-hidden', 'true');

        const label = document.createElement('span');
        label.className = 'edit-cluster-instance-type-label';
        label.textContent = `${index + 1}. ${resolveInstanceTypeName(instanceTypeID)}`;

        const actions = document.createElement('div');
        actions.className = 'edit-cluster-instance-type-actions';

        const upButton = document.createElement('button');
        upButton.type = 'button';
        upButton.className = 'edit-cluster-instance-type-reorder';
        upButton.textContent = '↑';
        upButton.title = 'Move up';
        upButton.disabled = index === 0;
        upButton.onclick = () => moveCreateClusterCPInstanceTypePreferenceUp(index);

        const downButton = document.createElement('button');
        downButton.type = 'button';
        downButton.className = 'edit-cluster-instance-type-reorder';
        downButton.textContent = '↓';
        downButton.title = 'Move down';
        downButton.disabled = index === createClusterCPInstanceTypePreferences.length - 1;
        downButton.onclick = () => moveCreateClusterCPInstanceTypePreferenceDown(index);

        const removeButton = document.createElement('button');
        removeButton.type = 'button';
        removeButton.textContent = 'Remove';
        removeButton.className = 'edit-cluster-instance-type-remove';
        removeButton.onclick = () => removeCreateClusterCPInstanceTypePreference(index);

        actions.appendChild(upButton);
        actions.appendChild(downButton);
        actions.appendChild(removeButton);

        item.appendChild(dragHandle);
        item.appendChild(label);
        item.appendChild(actions);

        item.addEventListener('dragstart', (event) => {
            createClusterCPInstanceTypeDragIndex = index;
            item.classList.add('dragging');
            event.dataTransfer.effectAllowed = 'move';
            event.dataTransfer.setData('text/plain', String(index));
        });

        item.addEventListener('dragend', () => {
            createClusterCPInstanceTypeDragIndex = null;
            item.classList.remove('dragging');
            list.querySelectorAll('.edit-cluster-instance-type-item.drag-over').forEach((el) => {
                el.classList.remove('drag-over');
            });
        });

        item.addEventListener('dragover', (event) => {
            event.preventDefault();
            event.dataTransfer.dropEffect = 'move';
            if (createClusterCPInstanceTypeDragIndex !== null && createClusterCPInstanceTypeDragIndex !== index) {
                item.classList.add('drag-over');
            }
        });

        item.addEventListener('dragleave', () => {
            item.classList.remove('drag-over');
        });

        item.addEventListener('drop', (event) => {
            event.preventDefault();
            item.classList.remove('drag-over');
            const fromIndex = createClusterCPInstanceTypeDragIndex;
            if (fromIndex === null || fromIndex === index) {
                return;
            }
            moveCreateClusterCPInstanceTypePreference(fromIndex, index);
            createClusterCPInstanceTypeDragIndex = null;
        });

        list.appendChild(item);
    });
}

// === CREATE-CLUSTER CONTROL PLANE SECTION SETUP / POPULATION ===

// Reset the create-cluster Control Plane section to defaults. Called when the modal opens.
function setupCreateClusterCPSection() {
    const section = document.getElementById('create-cluster-cp-section');
    if (!section) {
        return;
    }

    createClusterCPInstanceTypePreferences = [];

    const desiredCount = document.getElementById('cp-config-desired-count');
    if (desiredCount) desiredCount.value = 3;
    const maxSurge = document.getElementById('cp-config-max-surge');
    if (maxSurge) maxSurge.value = 0;
    const teleport = document.getElementById('cp-config-teleport-enabled');
    if (teleport) teleport.checked = false;
    const provisioningMonitor = document.getElementById('cp-config-provisioning-monitor-disabled');
    if (provisioningMonitor) provisioningMonitor.checked = false;
    const systemReservedMemory = document.getElementById('cp-config-system-reserved-memory');
    if (systemReservedMemory) systemReservedMemory.value = '';
    const nodeLabels = document.getElementById('cp-config-node-labels');
    if (nodeLabels) nodeLabels.value = '';
    const spireMethod = document.getElementById('cp-config-capi-spire-attestation-method');
    if (spireMethod) spireMethod.value = SPIRE_ATTESTATION_METHOD_TPM;
    ['cp-config-capi-infra-machine-template-yaml', 'cp-config-capi-bootstrap-files-yaml', 'cp-config-capi-pre-kubeadm-commands-yaml'].forEach((id) => {
        const el = document.getElementById(id);
        if (el) el.value = '';
    });

    populateCreateClusterCPProfileSelects();
    populateCreateClusterCPInstanceTypeSelect([]);
    renderCreateClusterCPInstanceTypePreferenceList();
    resetCreateClusterCPForgeFields();
    resetCreateClusterCPSubnetFields();
}

function populateCreateClusterCPProfileSelects() {
    const osSelect = document.getElementById('cp-config-os-profile');
    if (osSelect) {
        osSelect.innerHTML = '<option value="">Select an OS profile</option>';
        for (const profile of state.osProfiles || []) {
            const option = document.createElement('option');
            option.value = profile.name;
            option.textContent = profile.name;
            osSelect.appendChild(option);
        }
    }
    const configSelect = document.getElementById('cp-config-config-profile');
    if (configSelect) {
        configSelect.innerHTML = '<option value="">Select a config profile</option>';
        for (const profile of state.configProfiles || []) {
            const option = document.createElement('option');
            option.value = profile.name;
            option.textContent = profile.name;
            configSelect.appendChild(option);
        }
    }
}

function resetCreateClusterCPForgeFields() {
    // Clear the tenant-scoped preference state so a create payload can't retain
    // stale instanceTypePreference IDs from a previously selected tenant/site.
    createClusterCPInstanceTypePreferences = [];
    createClusterCPInstanceTypeDragIndex = null;

    const sshContainer = document.getElementById('cp-config-ssh-key-groups-container');
    if (sshContainer) {
        sshContainer.innerHTML = '<div class="empty-text">Select SSH key groups</div>';
        sshContainer.classList.add('disabled');
        sshContainer.title = 'You must select a tenant first';
    }
    populateCreateClusterCPInstanceTypeSelect([]);
    renderCreateClusterCPInstanceTypePreferenceList();
}

function resetCreateClusterCPSubnetFields() {
    const subnetsContainer = document.getElementById('cp-config-subnets-container');
    if (subnetsContainer) {
        subnetsContainer.innerHTML = '<div class="empty-text">Select subnets</div>';
        subnetsContainer.classList.add('disabled');
        subnetsContainer.title = 'You must select a VPC first';
    }
}

// Populate tenant-scoped forge data (instance types for the preference picker, SSH key groups).
function populateCreateClusterCPForgeData(forgeData) {
    if (!forgeData) {
        return;
    }

    populateCreateClusterCPInstanceTypeSelect(forgeData.instanceTypes || []);

    const sshContainer = document.getElementById('cp-config-ssh-key-groups-container');
    if (sshContainer) {
        const groups = forgeData.sshKeyGroups || [];
        sshContainer.innerHTML = '';
        if (groups.length === 0) {
            sshContainer.innerHTML = '<div class="empty-text">No SSH key groups available</div>';
            sshContainer.classList.add('disabled');
        } else {
            sshContainer.classList.remove('disabled');
            sshContainer.title = '';
            groups.forEach((group) => {
                const itemDiv = document.createElement('div');
                itemDiv.className = 'ssh-key-group-item';

                const checkbox = document.createElement('input');
                checkbox.type = 'checkbox';
                checkbox.id = `cp-config-ssh-key-group-${group.id}`;
                checkbox.value = group.id;
                checkbox.name = 'cp-config-ssh-key-groups';

                const label = document.createElement('label');
                label.htmlFor = checkbox.id;
                label.textContent = group.name;

                itemDiv.appendChild(checkbox);
                itemDiv.appendChild(label);
                sshContainer.appendChild(itemDiv);
            });
        }
    }
}

// Populate VPC-scoped forge data (subnets or VPC prefixes, depending on native networking).
function updateCreateClusterCPSubnetData(subnets, vpcPrefixes, instances) {
    const subnetsContainer = document.getElementById('cp-config-subnets-container');
    if (!subnetsContainer) {
        return;
    }

    const isNativeNetworkingEnabled = getSelectedSiteNativeNetworking();
    const itemsToShow = isNativeNetworkingEnabled ? (vpcPrefixes || []) : (subnets || []);
    const labelText = isNativeNetworkingEnabled ? 'VPC Prefixes' : 'Subnets';
    const emptyText = isNativeNetworkingEnabled ? 'VPC prefixes' : 'subnets';
    const checkboxNameSuffix = isNativeNetworkingEnabled ? 'vpc-prefixes' : 'subnets';
    const tempForgeData = { subnets, instances };

    const labelElement = document.getElementById('cp-config-subnets-label');
    if (labelElement) {
        labelElement.textContent = labelText + ':';
    }

    subnetsContainer.innerHTML = '';
    if (itemsToShow.length === 0) {
        subnetsContainer.innerHTML = `<div class="empty-text">No ${emptyText} available</div>`;
        subnetsContainer.classList.add('disabled');
        return;
    }

    subnetsContainer.classList.remove('disabled');
    subnetsContainer.title = '';
    itemsToShow.forEach((item) => {
        const itemDiv = document.createElement('div');
        itemDiv.className = 'subnet-item';

        const checkbox = document.createElement('input');
        checkbox.type = 'checkbox';
        checkbox.id = `cp-config-${checkboxNameSuffix}-${item.id}`;
        checkbox.value = item.id;
        checkbox.name = `cp-config-${checkboxNameSuffix}`;

        let availability = { available: -1, total: '?' };
        let availabilityText = '';
        if (isNativeNetworkingEnabled) {
            availability = calculateVpcPrefixIpAvailability(item.id, tempForgeData);
        } else {
            availability = calculateSubnetAvailability(item.id, tempForgeData);
        }
        if (availability.total !== '?') {
            availabilityText = ` (${availability.available}/${availability.total} IPs available)`;
        }

        const label = document.createElement('label');
        label.htmlFor = checkbox.id;
        label.textContent = item.name + availabilityText;

        itemDiv.appendChild(checkbox);
        itemDiv.appendChild(label);
        subnetsContainer.appendChild(itemDiv);
    });
}

// Build the cluster-level controlPlaneConfig payload (CreateClusterControlPlaneConfig).
// Only fields the user actually set are included; blank optional fields are omitted.
function getCreateClusterControlPlaneConfigData(isCAPICluster) {
    const desiredCountRaw = document.getElementById('cp-config-desired-count').value;
    const desiredCount = parseInt(desiredCountRaw, 10);
    if (Number.isNaN(desiredCount) || desiredCount < 1) {
        throw new Error('Control plane desired count must be an integer of at least 1');
    }

    const config = { desiredCount };

    const maxSurge = parseInt(document.getElementById('cp-config-max-surge').value, 10);
    if (!Number.isNaN(maxSurge)) {
        config.maxSurge = maxSurge;
    }

    if (document.getElementById('cp-config-teleport-enabled').checked) {
        config.teleportEnabled = true;
    }
    if (document.getElementById('cp-config-provisioning-monitor-disabled').checked) {
        config.provisioningMonitorDisabled = true;
    }

    const systemReservedMemory = document.getElementById('cp-config-system-reserved-memory').value.trim();
    if (systemReservedMemory) {
        config.systemReservedMemory = systemReservedMemory;
    }

    const nodeLabels = parseNodeLabelsInput(document.getElementById('cp-config-node-labels').value);
    if (nodeLabels.length > 0) {
        config.nodeLabels = nodeLabels;
    }

    if (isCAPICluster) {
        const capiInfraMachineTemplate = parseYAMLManifestToJSONObject(
            document.getElementById('cp-config-capi-infra-machine-template-yaml').value,
            'Control plane CAPI infra machine template'
        );
        if (!capiInfraMachineTemplate) {
            throw new Error('Control plane requires a CAPI infra machine template for CAPI-backed clusters');
        }
        config.capiInfraMachineTemplate = capiInfraMachineTemplate;
        config.capiSpireAttestationMethod = getSpireAttestationMethodFromInput(
            document.getElementById('cp-config-capi-spire-attestation-method')
        );
        const capiBootstrapFiles = parseCapiBootstrapFilesInput(
            document.getElementById('cp-config-capi-bootstrap-files-yaml').value,
            'Control plane CAPI bootstrap files'
        );
        if (capiBootstrapFiles.length > 0) {
            config.capiBootstrapFiles = capiBootstrapFiles;
        }
        const capiPreKubeadmCommands = parseCapiPreKubeadmCommandsInput(
            document.getElementById('cp-config-capi-pre-kubeadm-commands-yaml').value,
            'Control plane CAPI pre-kubeadm commands'
        );
        if (capiPreKubeadmCommands.length > 0) {
            config.capiPreKubeadmCommands = capiPreKubeadmCommands;
        }
    } else {
        const osProfile = document.getElementById('cp-config-os-profile').value;
        if (osProfile) {
            config.osProfile = osProfile;
        }
        const configProfile = document.getElementById('cp-config-config-profile').value;
        if (configProfile) {
            config.configProfile = configProfile;
        }

        const isNativeNetworkingEnabled = getSelectedSiteNativeNetworking();
        const selectedNetworkIDs = Array.from(
            document.querySelectorAll('#cp-config-subnets-container input[type="checkbox"]:checked')
        ).map((cb) => cb.value);
        if (isNativeNetworkingEnabled) {
            if (selectedNetworkIDs.length > 0) {
                config.vpcPrefixIDs = selectedNetworkIDs;
            }
        } else if (selectedNetworkIDs.length > 0) {
            config.subnetIDs = selectedNetworkIDs;
        }

        const sshKeyGroupIDs = Array.from(
            document.querySelectorAll('#cp-config-ssh-key-groups-container input[type="checkbox"]:checked')
        ).map((cb) => cb.value);
        if (sshKeyGroupIDs.length > 0) {
            config.sshKeyGroupIDs = sshKeyGroupIDs;
        }

        if (createClusterCPInstanceTypePreferences.length > 0) {
            config.instanceTypePreference = [...createClusterCPInstanceTypePreferences];
        }
    }

    return config;
}

// === EDIT-CLUSTER EXPANDED CONTROL PLANE EDITORS (cpControllerMode) ===

function clusterSiteUsesNativeNetworking(details) {
    const forgeSite = (state.forgeSites || []).find((fs) => fs.id === details.siteID);
    return forgeSite?.capabilities?.nativeNetworking || false;
}

function buildEditClusterCPMultiselect(container, items, selectedIDs, idPrefix, name, emptyLabel, itemClassName) {
    if (!container) {
        return;
    }
    const selected = new Set(selectedIDs || []);
    container.innerHTML = '';
    if (!items || items.length === 0) {
        container.innerHTML = `<div class="empty-text">${emptyLabel}</div>`;
        container.classList.add('disabled');
        return;
    }
    container.classList.remove('disabled');
    container.title = '';
    items.forEach((item) => {
        const itemDiv = document.createElement('div');
        if (itemClassName) {
            itemDiv.className = itemClassName;
        }
        const checkbox = document.createElement('input');
        checkbox.type = 'checkbox';
        checkbox.id = `${idPrefix}-${item.id}`;
        checkbox.value = item.id;
        checkbox.name = name;
        checkbox.checked = selected.has(item.id);
        const label = document.createElement('label');
        label.htmlFor = checkbox.id;
        label.textContent = item.name;
        itemDiv.appendChild(checkbox);
        itemDiv.appendChild(label);
        container.appendChild(itemDiv);
    });
}

// Populate the expanded cluster-level CP editors from the cluster's controlPlaneConfig.
// Only shown/populated in cpControllerMode; legacy mode keeps just the existing
// maxSurge / instanceTypePreference / provisioningMonitorDisabled editor.
function populateEditClusterCPExpandedFields(details, cpConfig) {
    const group = document.getElementById('edit-cluster-cp-expanded-group');
    if (!group) {
        return;
    }
    if (!state.cpControllerMode) {
        group.style.display = 'none';
        return;
    }
    group.style.display = '';

    const isCAPICluster = clusterUsesCAPIInfrastructure(details);
    const forgeData = state.selectedClusterForgeData || {};

    document.getElementById('edit-cluster-cp-desired-count').value = cpConfig.desiredCount ?? '';
    document.getElementById('edit-cluster-cp-teleport-enabled').checked = !!cpConfig.teleportEnabled;
    document.getElementById('edit-cluster-cp-system-reserved-memory').value = cpConfig.systemReservedMemory || '';
    document.getElementById('edit-cluster-cp-node-labels').value = formatNodeLabelsForInput(cpConfig.nodeLabels);

    const osSelect = document.getElementById('edit-cluster-cp-os-profile');
    osSelect.innerHTML = '<option value="">Select an OS profile</option>';
    for (const profile of state.osProfiles || []) {
        const option = document.createElement('option');
        option.value = profile.name;
        option.textContent = profile.name;
        osSelect.appendChild(option);
    }
    osSelect.value = cpConfig.osProfile || '';

    const configSelect = document.getElementById('edit-cluster-cp-config-profile');
    configSelect.innerHTML = '<option value="">Select a config profile</option>';
    for (const profile of state.configProfiles || []) {
        const option = document.createElement('option');
        option.value = profile.name;
        option.textContent = profile.name;
        configSelect.appendChild(option);
    }
    configSelect.value = cpConfig.configProfile || '';

    const isNative = clusterSiteUsesNativeNetworking(details);
    const subnetsLabel = document.getElementById('edit-cluster-cp-subnets-label');
    const subnetsContainer = document.getElementById('edit-cluster-cp-subnets-container');
    if (isNative) {
        if (subnetsLabel) subnetsLabel.textContent = 'VPC Prefixes:';
        buildEditClusterCPMultiselect(subnetsContainer, forgeData.vpcPrefixes, cpConfig.vpcPrefixIDs, 'edit-cluster-cp-vpc-prefix', 'edit-cluster-cp-vpc-prefixes', 'No VPC prefixes available', 'subnet-item');
    } else {
        if (subnetsLabel) subnetsLabel.textContent = 'Subnets:';
        buildEditClusterCPMultiselect(subnetsContainer, forgeData.subnets, cpConfig.subnetIDs, 'edit-cluster-cp-subnet', 'edit-cluster-cp-subnets', 'No subnets available', 'subnet-item');
    }

    buildEditClusterCPMultiselect(
        document.getElementById('edit-cluster-cp-ssh-key-groups-container'),
        forgeData.sshKeyGroups,
        cpConfig.sshKeyGroupIDs,
        'edit-cluster-cp-ssh-key-group',
        'edit-cluster-cp-ssh-key-groups',
        'No SSH key groups available',
        'ssh-key-group-item'
    );

    document.getElementById('edit-cluster-cp-capi-infra-machine-template-yaml').value =
        hasRenderableManifest(cpConfig.capiInfraMachineTemplate) ? formatManifestForDisplay(cpConfig.capiInfraMachineTemplate) : '';
    document.getElementById('edit-cluster-cp-capi-spire-attestation-method').value =
        normalizeSpireAttestationMethod(cpConfig.capiSpireAttestationMethod);
    document.getElementById('edit-cluster-cp-capi-bootstrap-files-yaml').value =
        (Array.isArray(cpConfig.capiBootstrapFiles) && cpConfig.capiBootstrapFiles.length > 0) ? formatManifestForDisplay(cpConfig.capiBootstrapFiles) : '';
    document.getElementById('edit-cluster-cp-capi-pre-kubeadm-commands-yaml').value =
        (Array.isArray(cpConfig.capiPreKubeadmCommands) && cpConfig.capiPreKubeadmCommands.length > 0) ? formatManifestForDisplay(cpConfig.capiPreKubeadmCommands) : '';

    applyForgeFieldVisibility(group, !isCAPICluster);
    applyCAPIFieldVisibility(group, isCAPICluster);
}

// Read the expanded CP editors and return only the fields that changed relative to the
// current controlPlaneConfig, to merge into the PATCH controlPlaneConfig payload.
function getEditClusterCPExpandedUpdates(details, cpConfig) {
    const updates = {};
    if (!state.cpControllerMode) {
        return updates;
    }

    const isCAPICluster = clusterUsesCAPIInfrastructure(details);

    const desiredCountRaw = document.getElementById('edit-cluster-cp-desired-count').value;
    if (desiredCountRaw !== '' && desiredCountRaw !== null) {
        const desiredCount = parseInt(desiredCountRaw, 10);
        if (Number.isNaN(desiredCount) || desiredCount < 1) {
            throw new Error('Control plane desired count must be an integer of at least 1');
        }
        if (desiredCount !== (cpConfig.desiredCount ?? null)) {
            updates.desiredCount = desiredCount;
        }
    }

    const teleportEnabled = document.getElementById('edit-cluster-cp-teleport-enabled').checked;
    if (teleportEnabled !== !!cpConfig.teleportEnabled) {
        updates.teleportEnabled = teleportEnabled;
    }

    const systemReservedMemory = document.getElementById('edit-cluster-cp-system-reserved-memory').value.trim();
    if (systemReservedMemory !== (cpConfig.systemReservedMemory || '')) {
        updates.systemReservedMemory = systemReservedMemory;
    }

    const nodeLabels = parseNodeLabelsInput(document.getElementById('edit-cluster-cp-node-labels').value);
    if (JSON.stringify(nodeLabels) !== JSON.stringify(cpConfig.nodeLabels || [])) {
        updates.nodeLabels = nodeLabels;
    }

    if (isCAPICluster) {
        const capiInfraMachineTemplate = parseYAMLManifestToJSONObject(
            document.getElementById('edit-cluster-cp-capi-infra-machine-template-yaml').value,
            'Control plane CAPI infra machine template'
        );
        if (capiInfraMachineTemplate !== undefined &&
            JSON.stringify(capiInfraMachineTemplate) !== JSON.stringify(cpConfig.capiInfraMachineTemplate || null)) {
            updates.capiInfraMachineTemplate = capiInfraMachineTemplate;
        }
        const capiSpireAttestationMethod = getSpireAttestationMethodFromInput(
            document.getElementById('edit-cluster-cp-capi-spire-attestation-method')
        );
        if (capiSpireAttestationMethod !== normalizeSpireAttestationMethod(cpConfig.capiSpireAttestationMethod)) {
            updates.capiSpireAttestationMethod = capiSpireAttestationMethod;
        }
        const capiBootstrapFiles = parseCapiBootstrapFilesInput(
            document.getElementById('edit-cluster-cp-capi-bootstrap-files-yaml').value,
            'Control plane CAPI bootstrap files'
        );
        if (JSON.stringify(capiBootstrapFiles) !== JSON.stringify(cpConfig.capiBootstrapFiles || [])) {
            updates.capiBootstrapFiles = capiBootstrapFiles;
        }
        const capiPreKubeadmCommands = parseCapiPreKubeadmCommandsInput(
            document.getElementById('edit-cluster-cp-capi-pre-kubeadm-commands-yaml').value,
            'Control plane CAPI pre-kubeadm commands'
        );
        if (JSON.stringify(capiPreKubeadmCommands) !== JSON.stringify(cpConfig.capiPreKubeadmCommands || [])) {
            updates.capiPreKubeadmCommands = capiPreKubeadmCommands;
        }
    } else {
        const osProfile = document.getElementById('edit-cluster-cp-os-profile').value;
        if (osProfile !== (cpConfig.osProfile || '')) {
            updates.osProfile = osProfile;
        }
        const configProfile = document.getElementById('edit-cluster-cp-config-profile').value;
        if (configProfile !== (cpConfig.configProfile || '')) {
            updates.configProfile = configProfile;
        }

        const isNative = clusterSiteUsesNativeNetworking(details);
        const selectedNetworkIDs = Array.from(
            document.querySelectorAll('#edit-cluster-cp-subnets-container input[type="checkbox"]:checked')
        ).map((cb) => cb.value);
        if (isNative) {
            if (JSON.stringify(selectedNetworkIDs) !== JSON.stringify(cpConfig.vpcPrefixIDs || [])) {
                updates.vpcPrefixIDs = selectedNetworkIDs;
            }
        } else if (JSON.stringify(selectedNetworkIDs) !== JSON.stringify(cpConfig.subnetIDs || [])) {
            updates.subnetIDs = selectedNetworkIDs;
        }

        const sshKeyGroupIDs = Array.from(
            document.querySelectorAll('#edit-cluster-cp-ssh-key-groups-container input[type="checkbox"]:checked')
        ).map((cb) => cb.value);
        if (JSON.stringify(sshKeyGroupIDs) !== JSON.stringify(cpConfig.sshKeyGroupIDs || [])) {
            updates.sshKeyGroupIDs = sshKeyGroupIDs;
        }
    }

    return updates;
}

function showEditClusterModal() {
    if (!state.selectedClusterDetails) {
        alert('Please select a cluster first');
        return;
    }
    
    const modal = document.getElementById('edit-cluster-modal');
    const currentVersion = state.selectedClusterDetails.controlPlaneKubernetesVersion;
    const enableCPNodeRecycling = state.selectedClusterDetails.enableCPNodeRecycling || false;
    
    const details = state.selectedClusterDetails;
    document.getElementById('edit-cluster-current-version').textContent = currentVersion;
    document.getElementById('edit-cluster-new-version').value = '';
    document.getElementById('edit-cluster-enable-cp-node-recycling').checked = enableCPNodeRecycling;
    const etcdEncryptionCheckbox = document.getElementById('edit-cluster-enable-etcd-encryption');
    const etcdEncryptionHelp = document.getElementById('edit-cluster-etcd-encryption-help');
    const etcdEncryptionInProgress = isEtcdEncryptionUpdateInProgress(details);
    etcdEncryptionCheckbox.checked = details.etcdEncrypted || etcdEncryptionInProgress;
    etcdEncryptionCheckbox.disabled = details.etcdEncrypted || etcdEncryptionInProgress;
    etcdEncryptionHelp.textContent = details.etcdEncrypted
        ? 'Etcd encryption is enabled and cannot be disabled.'
        : etcdEncryptionInProgress
            ? 'Etcd encryption enablement is in progress.'
            : 'Etcd encryption is a one-way operation and cannot be disabled after enablement starts.';
    const cpConfig = details.controlPlaneConfig || {};
    document.getElementById('edit-cluster-max-surge').value = cpConfig.maxSurge ?? 0;
    document.getElementById('edit-cluster-provisioning-monitor-disabled').checked = cpConfig.provisioningMonitorDisabled || false;
    editClusterInstanceTypePreferences = [...(cpConfig.instanceTypePreference || [])];
    renderEditClusterInstanceTypePreferenceList();
    populateEditClusterInstanceTypeSelect();
    // instanceTypePreference is Forge-only, so hide the legacy editor for CAPI
    // clusters (matching the create flow's .forge-only-field gating).
    const instanceTypeGroup = document.getElementById('edit-cluster-instance-type-group');
    if (instanceTypeGroup) {
        instanceTypeGroup.style.display = clusterUsesCAPIInfrastructure(details) ? 'none' : '';
    }
    populateEditClusterCPExpandedFields(details, cpConfig);
    document.getElementById('edit-cluster-error').style.display = 'none';
    document.getElementById('edit-cluster-loading').style.display = 'none';
    modal.style.display = 'block';
}

function closeEditClusterModal() {
    document.getElementById('edit-cluster-modal').style.display = 'none';
}

function renderEditClusterLoading() {
    document.getElementById('edit-cluster-error').style.display = 'none';
    document.getElementById('edit-cluster-loading').innerHTML = 'Updating cluster...';
    document.getElementById('edit-cluster-loading').style.display = 'block';
}

function renderEditClusterError(error) {
    document.getElementById('edit-cluster-loading').style.display = 'none';
    document.getElementById('edit-cluster-error').innerHTML = `Error updating cluster: ${error}`;
    document.getElementById('edit-cluster-error').style.display = 'block';
}

async function submitEditCluster() {
    const newVersion = document.getElementById('edit-cluster-new-version').value.trim();
    const currentVersion = state.selectedClusterDetails.controlPlaneKubernetesVersion;
    const requestedCPNodeRecycling = document.getElementById('edit-cluster-enable-cp-node-recycling').checked;
    const currentCPNodeRecycling = state.selectedClusterDetails.enableCPNodeRecycling || false;
    const requestedEtcdEncryption = document.getElementById('edit-cluster-enable-etcd-encryption').checked;
    const etcdEncryptionInProgress = isEtcdEncryptionUpdateInProgress(state.selectedClusterDetails);
    const etcdEncryptionChanged = requestedEtcdEncryption &&
        !state.selectedClusterDetails.etcdEncrypted &&
        !etcdEncryptionInProgress;
    const versionProvided = !!newVersion;
    const versionChanged = versionProvided && newVersion !== currentVersion;
    const cpNodeRecyclingChanged = requestedCPNodeRecycling !== currentCPNodeRecycling;
    const currentCpConfig = state.selectedClusterDetails.controlPlaneConfig || {};
    const requestedMaxSurge = parseInt(document.getElementById('edit-cluster-max-surge').value, 10);
    const currentMaxSurge = currentCpConfig.maxSurge ?? 0;
    const maxSurgeChanged = !Number.isNaN(requestedMaxSurge) && requestedMaxSurge !== currentMaxSurge;
    const currentPrefs = currentCpConfig.instanceTypePreference || [];
    const instancePrefsChanged = JSON.stringify(editClusterInstanceTypePreferences) !== JSON.stringify(currentPrefs);
    const requestedProvisioningMonitorDisabled = document.getElementById('edit-cluster-provisioning-monitor-disabled').checked;
    const currentProvisioningMonitorDisabled = currentCpConfig.provisioningMonitorDisabled || false;
    const provisioningMonitorDisabledChanged = requestedProvisioningMonitorDisabled !== currentProvisioningMonitorDisabled;
    
    // The edit modal saves via a plain button, so the min/max on the max-surge
    // input is never enforced as native form validation. Reject an out-of-range
    // value here before it can be merged into the PATCH and rejected by the API.
    if (!Number.isNaN(requestedMaxSurge) && (requestedMaxSurge < 0 || requestedMaxSurge > 1)) {
        renderEditClusterError('Cluster max surge must be 0 or 1');
        return;
    }
    if (versionProvided && !/^\d+\.\d+\.\d+$/.test(newVersion)) {
        renderEditClusterError('Invalid version format. Use X.Y.Z (e.g., 1.31.9)');
        return;
    }
    if (versionProvided && etcdEncryptionChanged) {
        renderEditClusterError('Etcd encryption cannot be enabled in the same update as a Kubernetes version change.');
        return;
    }
    
    renderEditClusterLoading();
    
    try {
        const updates = {};
        if (versionProvided) {
            updates.controlPlaneKubernetesVersion = newVersion;
        }
        // Always send the checkbox value so no-op saves remain idempotent.
        updates.enableCPNodeRecycling = requestedCPNodeRecycling;
        if (etcdEncryptionChanged) {
            updates.enableEtcdEncryption = true;
        }

        const cpConfigUpdates = {};
        if (maxSurgeChanged) {
            cpConfigUpdates.maxSurge = requestedMaxSurge;
        }
        if (instancePrefsChanged) {
            cpConfigUpdates.instanceTypePreference = [...editClusterInstanceTypePreferences];
        }
        if (provisioningMonitorDisabledChanged) {
            cpConfigUpdates.provisioningMonitorDisabled = requestedProvisioningMonitorDisabled;
        }
        // Merge the expanded cluster-level CP editors (cpControllerMode only). Returns an
        // empty object in legacy mode or when nothing changed.
        Object.assign(cpConfigUpdates, getEditClusterCPExpandedUpdates(state.selectedClusterDetails, currentCpConfig));
        if (Object.keys(cpConfigUpdates).length > 0) {
            updates.controlPlaneConfig = cpConfigUpdates;
        }

        await updateCluster(state.selectedClusterDetails.id, updates);

        const cpConfigChanged = Object.keys(cpConfigUpdates).length > 0;
        const controlPlaneSettingsChanged = cpNodeRecyclingChanged || cpConfigChanged;
        if (!versionChanged && !controlPlaneSettingsChanged && !versionProvided && !etcdEncryptionChanged) {
            showToast('No changes to apply', 'success');
        } else if (etcdEncryptionChanged && controlPlaneSettingsChanged) {
            showToast('Cluster settings saved and etcd encryption enablement started', 'success');
        } else if (etcdEncryptionChanged) {
            showToast('Etcd encryption enablement started', 'success');
        } else if (versionChanged && (cpNodeRecyclingChanged || cpConfigChanged)) {
            showToast(`Cluster updated: Kubernetes ${newVersion} and control plane settings saved`, 'success');
        } else if (versionChanged) {
            showToast(`Kubernetes upgrade to ${newVersion} initiated successfully`, 'success');
        } else if (cpConfigChanged && cpNodeRecyclingChanged) {
            showToast('Control plane settings updated successfully', 'success');
        } else if (cpConfigChanged) {
            showToast('Cluster control plane config updated successfully', 'success');
        } else {
            showToast('CP node recycling setting updated successfully', 'success');
        }

        closeEditClusterModal();
        // Refresh cluster details to show updated version
        await selectCluster(state.selectedClusterDetails.id);
    } catch (error) {
        console.error('Failed to update cluster:', error);
        renderEditClusterError(error.message || 'Failed to update cluster');
    }
}

async function showCreateNodePoolModal() {
    if (!state.selectedClusterDetails || !state.selectedClusterID) {
        alert('Please select a cluster first');
        return;
    }

    const form = document.getElementById('create-nodepool-form');
    const formFieldsContainer = document.getElementById('create-nodepool-form-fields-container') || 
        (() => {
            const container = document.createElement('div');
            container.id = 'create-nodepool-form-fields-container';
        const loadingDiv = document.getElementById('create-nodepool-loading');
            form.insertBefore(container, loadingDiv);
            return container;
        })();
    
    const isCAPICluster = clusterUsesCAPIInfrastructure(state.selectedClusterDetails);
    formFieldsContainer.innerHTML = _generateNodePoolFormFieldsHTML('nodepool-', '', 0, false, '', false, '', '', false, 0, 'smallest', isCAPICluster);
    
    if (form) {
        form.reset(); 
    }
    document.getElementById('nodepool-cluster-id').value = state.selectedClusterID;
    
    applyNodePoolInfrastructureFields(formFieldsContainer, isCAPICluster);

    // Use cached forge data if available, otherwise load it
    const cluster = state.selectedClusterDetails;
    let forgeData = state.selectedClusterForgeData;
    
    if (!isCAPICluster && !forgeData && cluster.orgID && cluster.siteID) {
        try {
            forgeData = await loadForgeData(cluster.orgID, cluster.siteID, cluster.vpcID);
        } catch (error) {
            console.error('Error loading forge data for node pool modal:', error);
            forgeData = null;
        }
    }
    
    if (!isCAPICluster && forgeData) {
        // Populate instance types
    const instanceTypeSelect = document.getElementById('nodepool-instance-type-id');
        instanceTypeSelect.innerHTML = '<option value="">Select an instance type</option>';
        for (const instanceType of forgeData.instanceTypes) {
            const option = document.createElement('option');
            option.value = instanceType.id;
            
            let availabilityText = '';
            if (instanceType.unusedUsable !== undefined && instanceType.total !== undefined) {
                availabilityText = ` (${instanceType.unusedUsable}/${instanceType.total} available)`;
            } else if (instanceType.unusedUsable !== undefined) {
                availabilityText = ` (${instanceType.unusedUsable} available)`;
            } else if (instanceType.total !== undefined) {
                availabilityText = ` (${instanceType.total} total)`;
            }
            
            option.textContent = instanceType.name + availabilityText;
            instanceTypeSelect.appendChild(option);
        }
        instanceTypeSelect.disabled = false;

        // Populate InfiniBand partitions
        const infiniBandSelect = document.getElementById('nodepool-infiniband-partition-id');
        infiniBandSelect.innerHTML = '<option value="">None</option>';
        for (const partition of forgeData.infiniBandPartitions) {
            const option = document.createElement('option');
            option.value = partition.id;
            option.textContent = partition.name;
            infiniBandSelect.appendChild(option);
        }
        infiniBandSelect.disabled = false;

        // Populate NVLink logical partitions
        const nvLinkLogicalPartitionSelect = document.getElementById('nodepool-nvlink-logical-partition-id');
        nvLinkLogicalPartitionSelect.innerHTML = '<option value="">None</option>';
        for (const partition of forgeData.nvLinkLogicalPartitions || []) {
            const option = document.createElement('option');
            option.value = partition.id;
            option.textContent = partition.name;
            nvLinkLogicalPartitionSelect.appendChild(option);
        }
        nvLinkLogicalPartitionSelect.disabled = false;

        // Populate SSH key groups
        const sshContainer = document.getElementById('nodepool-ssh-key-groups-container');
        sshContainer.innerHTML = '';
        if (forgeData.sshKeyGroups.length === 0) {
            sshContainer.innerHTML = '<div class="empty-text">No SSH key groups available</div>';
            sshContainer.classList.add('disabled');
        } else {
            // Remove disabled styling when SSH key groups are available
            sshContainer.classList.remove('disabled');
            
            forgeData.sshKeyGroups.forEach(group => {
                const itemDiv = document.createElement('div');
                itemDiv.className = 'ssh-key-group-item';
                
                const checkbox = document.createElement('input');
                checkbox.type = 'checkbox';
                checkbox.id = `nodepool-ssh-key-group-${group.id}`;
                checkbox.value = group.id;
                checkbox.name = 'nodepool-ssh-key-groups';
                
                const label = document.createElement('label');
                label.htmlFor = checkbox.id;
                label.textContent = group.name;
                
                itemDiv.appendChild(checkbox);
                itemDiv.appendChild(label);
                sshContainer.appendChild(itemDiv);
            });
        }

        // Populate subnets or VPC prefixes based on site capabilities
        const forgeSite = cluster?.siteID ? state.forgeSites.find(fs => fs.id === cluster.siteID) : null;
        const isNativeNetworkingEnabled = forgeSite?.capabilities?.nativeNetworking || false;
        const subnets = forgeData.subnets || [];
        const vpcPrefixes = forgeData.vpcPrefixes || [];
        const itemsToShow = isNativeNetworkingEnabled ? vpcPrefixes : subnets;
        const labelText = isNativeNetworkingEnabled ? 'VPC Prefixes' : 'Subnets';
        const emptyText = isNativeNetworkingEnabled ? 'VPC prefixes' : 'subnets';
        const checkboxNameSuffix = isNativeNetworkingEnabled ? 'vpc-prefixes' : 'subnets';
        
        const subnetsContainer = document.getElementById('nodepool-subnets-container');
        const labelElement = subnetsContainer?.parentElement?.querySelector('label');
        
        if (labelElement) {
            labelElement.textContent = labelText + ':';
        }
        
        subnetsContainer.innerHTML = '';
        if (itemsToShow.length === 0) {
            subnetsContainer.innerHTML = `<div class="empty-text">No ${emptyText} available</div>`;
            subnetsContainer.classList.add('disabled');
        } else {
            // Remove disabled styling when resources are available
            subnetsContainer.classList.remove('disabled');
            
            itemsToShow.forEach(item => {
                const itemDiv = document.createElement('div');
                itemDiv.className = 'subnet-item';
                
                const checkbox = document.createElement('input');
                checkbox.type = 'checkbox';
                checkbox.id = `nodepool-${checkboxNameSuffix}-${item.id}`;
                checkbox.value = item.id;
                checkbox.name = `nodepool-${checkboxNameSuffix}`;
                
                let availability = { available: -1, total: '?' };
                let availabilityText = '';
                if (isNativeNetworkingEnabled) {
                    availability = calculateVpcPrefixIpAvailability(item.id, forgeData);
                } else {
                    availability = calculateSubnetAvailability(item.id, forgeData);
                }
                if (availability.total !== '?') {
                    availabilityText = ` (${availability.available}/${availability.total} IPs available)`;
                }
                
                const label = document.createElement('label');
                label.htmlFor = checkbox.id;
                label.textContent = item.name + availabilityText;
                
                itemDiv.appendChild(checkbox);
                itemDiv.appendChild(label);
                subnetsContainer.appendChild(itemDiv);
            });
        }
    }

    document.getElementById('create-nodepool-error').style.display = 'none';
    document.getElementById('create-nodepool-loading').style.display = 'none';
    document.getElementById('create-nodepool-modal').style.display = 'block';
}

function renderCreateNodePoolLoading() {
    document.getElementById('create-nodepool-error').style.display = 'none';
    document.getElementById('create-nodepool-loading').innerHTML = 'Creating node pool...';
    document.getElementById('create-nodepool-loading').style.display = 'block';
}

function renderCreateNodePoolError(error) {
    document.getElementById('create-nodepool-loading').style.display = 'none';
    document.getElementById('create-nodepool-error').innerHTML = `Error creating node pool: ${error}`;
    document.getElementById('create-nodepool-error').style.display = 'block';
}

async function toggleNodePoolProvisioningMonitor(nodePool) {
    const nextState = !nodePool.provisioningMonitorDisabled;
    const verb = nextState ? 'pause' : 'resume';
    try {
        await updateNodePool(nodePool.id, { provisioningMonitorDisabled: nextState });
        refreshData();
    } catch (error) {
        console.error(`Failed to ${verb} provisioning monitor for node pool:`, error);
        alert(`Failed to ${verb} provisioning monitor: ${error.message || 'Unknown error'}`);
    }
}

function showDeleteNodePoolModal(nodePoolID, nodePoolName) {
    document.getElementById('delete-nodepool-modal').setAttribute('data-nodepool-id', nodePoolID);
    const nodePool = state.selectedClusterNodePools.find(np => np.id === nodePoolID);
    const nameForDisplay = nodePool ? (nodePool.nodeNamePrefix || nodePool.name) : nodePoolName; // Use prefix, fallback to name or passed arg
    document.getElementById('delete-nodepool-name').textContent = nameForDisplay;
    document.getElementById('delete-nodepool-modal').style.display = 'block';
    document.getElementById('delete-nodepool-loading').style.display = 'none';
    document.getElementById('delete-nodepool-error').style.display = 'none';
}

function renderDeleteNodePoolLoading() {
    document.getElementById('delete-nodepool-error').style.display = 'none';
    document.getElementById('delete-nodepool-loading').innerHTML = 'Deleting node pool...';
    document.getElementById('delete-nodepool-loading').style.display = 'block';
}

function renderDeleteNodePoolError(error) {
    document.getElementById('delete-nodepool-loading').style.display = 'none';
    document.getElementById('delete-nodepool-error').innerHTML = `Error deleting node pool: ${error}`;
    document.getElementById('delete-nodepool-error').style.display = 'block';
}

async function showEditNodePoolModal(nodePoolID) {
    const nodePool = state.selectedClusterNodePools.find(np => np.id === nodePoolID);
    if (!nodePool) {
        console.error("Node pool not found for editing:", nodePoolID);
        alert("Node pool not found");
        return;
    }

    document.getElementById('edit-nodepool-id').value = nodePool.id;
    document.getElementById('edit-nodepool-description').value = nodePool.description || '';
    document.getElementById('edit-nodepool-desired-count').value = nodePool.desiredNodeCount || 0;
    document.getElementById('edit-nodepool-auto-provision-disabled').checked = nodePool.autoProvisionDisabled || false;
    document.getElementById('edit-nodepool-system-reserved-memory').value = nodePool.systemReservedMemory;
    document.getElementById('edit-nodepool-kube-reserved-memory').value = nodePool.kubeReservedMemory || '';
    document.getElementById('edit-nodepool-auto-mode-disabled').checked = nodePool.autoModeDisabled || false;
    document.getElementById('edit-nodepool-recycle-budget').value = nodePool.recycleBudget || 0;
    const isCAPICluster = clusterUsesCAPIInfrastructure(state.selectedClusterDetails);
    document.getElementById('edit-nodepool-disk-selection-strategy').value = nodePool.diskSelectionStrategy || 'smallest';

    // Show/hide and populate MaxSurge
    const maxSurgeGroup = document.getElementById('edit-nodepool-max-surge-group');
    const maxSurgeInput = document.getElementById('edit-nodepool-max-surge');

    maxSurgeGroup.style.display = 'block';
    maxSurgeInput.value = nodePool.maxSurge !== undefined && nodePool.maxSurge !== null ? nodePool.maxSurge : 0;

    // Always show kubernetesVersion field; disable and grey out for control plane pools
    const k8sVersionInput = document.getElementById('edit-nodepool-kubernetes-version');
    const k8sVersionHelpCP = document.getElementById('edit-nodepool-kubernetes-version-help-cp');
    k8sVersionInput.value = nodePool.kubernetesVersion || '';
    if (nodePool.controlPlane) {
        k8sVersionInput.disabled = true;
        k8sVersionInput.style.opacity = '0.5';
        k8sVersionInput.style.cursor = 'not-allowed';
        k8sVersionHelpCP.style.display = '';
    } else {
        k8sVersionInput.disabled = false;
        k8sVersionInput.style.opacity = '';
        k8sVersionInput.style.cursor = '';
        k8sVersionHelpCP.style.display = 'none';
    }

    // Populate subnet / VPC prefix selection (for editing subnetIDs or vpcPrefixIDs)
    const cluster = state.selectedClusterDetails;
    const forgeSite = cluster?.siteID ? state.forgeSites.find(fs => fs.id === cluster.siteID) : null;
    const isNativeNetworking = forgeSite?.capabilities?.nativeNetworking || false;
    let forgeData = state.selectedClusterForgeData;
    // Load Forge data if it isn't cached yet (e.g. the cluster-select fetch failed or hasn't finished).
    // Without it the subnet/VPC-prefix checkboxes can't render, and submitting an empty selection is
    // read by the API as "clear all" — wiping the pool's networking and forking a new node set.
    if (!isCAPICluster && !forgeData && cluster?.orgID && cluster?.siteID) {
        try {
            forgeData = await loadForgeData(cluster.orgID, cluster.siteID, cluster.vpcID);
            state.selectedClusterForgeData = forgeData;
        } catch (error) {
            console.error('Error loading Forge data for edit node pool modal:', error);
            forgeData = null;
        }
    }
    const subnetsLabel = document.getElementById('edit-nodepool-subnets-label');
    const subnetsContainer = document.getElementById('edit-nodepool-subnets-container');
    const subnetsFormGroup = subnetsContainer ? subnetsContainer.closest('.form-group') : null;

    if (subnetsLabel && !isCAPICluster) {
        subnetsLabel.textContent = isNativeNetworking ? 'VPC Prefixes:' : 'Subnets:';
    }
    if (subnetsContainer && !isCAPICluster) {
        subnetsContainer.innerHTML = '';
        subnetsContainer.classList.remove('disabled');
        subnetsContainer.title = '';

        // Pre-check checkboxes for the node pool's existing subnets or VPC prefixes
        const currentIDs = isNativeNetworking
            ? (nodePool.vpcPrefixIDs || [])
            : (nodePool.subnetIDs || []);

        if (!forgeData) {
            subnetsContainer.innerHTML = '<div class="empty-text">Cluster Forge data not loaded. Ensure cluster details are loaded.</div>';
            subnetsContainer.classList.add('disabled');
        } else {
             const itemsToShow = isNativeNetworking
                ? (forgeData.vpcPrefixes || [])
                : (forgeData.subnets || []);
            const emptyText = isNativeNetworking ? 'VPC prefixes' : 'subnets';

            if (itemsToShow.length === 0) {
                subnetsContainer.innerHTML = `<div class="empty-text">No ${emptyText} available</div>`;
                subnetsContainer.classList.add('disabled');
            } else {
                itemsToShow.forEach(item => {
                    const itemDiv = document.createElement('div');
                    itemDiv.className = 'subnet-item';

                    const checkbox = document.createElement('input');
                    checkbox.type = 'checkbox';
                    checkbox.id = `edit-nodepool-subnet-${item.id}`;
                    checkbox.value = item.id;
                    checkbox.checked = currentIDs.includes(item.id);

                    let availability = { available: -1, total: '?' };
                    let availabilityText = '';
                    if (isNativeNetworking) {
                        availability = calculateVpcPrefixIpAvailability(item.id, forgeData);
                    } else {
                        availability = calculateSubnetAvailability(item.id, forgeData);
                    }
                    if (availability.total !== '?') {
                        availabilityText = ` (${availability.available}/${availability.total} IPs available)`;
                    }

                    const label = document.createElement('label');
                    label.htmlFor = checkbox.id;
                    label.textContent = item.name + availabilityText;

                    itemDiv.appendChild(checkbox);
                    itemDiv.appendChild(label);
                    subnetsContainer.appendChild(itemDiv);
                });
            }
        }
    }

    // Populate OS profiles
    const osProfileSelect = document.getElementById('edit-nodepool-os-profile');
    if (osProfileSelect) {
        osProfileSelect.innerHTML = '<option value="">Select an OS profile</option>';
        const osProfileNames = new Set();
        if (state.osProfiles && state.osProfiles.length > 0) {
            for (const profile of state.osProfiles) {
                const option = document.createElement('option');
                option.value = profile.name;
                option.textContent = profile.name;
                osProfileSelect.appendChild(option);
                osProfileNames.add(profile.name);
            }
        }
        // If the current profile is deprecated (not in the list), add it with a deprecated indicator
        if (nodePool.osProfile && !osProfileNames.has(nodePool.osProfile)) {
            const deprecatedOption = document.createElement('option');
            deprecatedOption.value = nodePool.osProfile;
            deprecatedOption.textContent = `${nodePool.osProfile} (Deprecated)`;
            osProfileSelect.appendChild(deprecatedOption);
        }
        osProfileSelect.value = nodePool.osProfile || '';
    }
    const capiInfraTemplateInput = document.getElementById('edit-nodepool-capi-infra-machine-template-yaml');
    if (capiInfraTemplateInput) {
        capiInfraTemplateInput.value = hasRenderableManifest(nodePool.capiInfraMachineTemplate)
            ? formatManifestForDisplay(nodePool.capiInfraMachineTemplate)
            : '';
    }
    const capiSpireAttestationMethodInput = document.getElementById('edit-nodepool-capi-spire-attestation-method');
    if (capiSpireAttestationMethodInput) {
        capiSpireAttestationMethodInput.value = normalizeSpireAttestationMethod(nodePool.capiSpireAttestationMethod);
    }
    const capiBootstrapFilesInput = document.getElementById('edit-nodepool-capi-bootstrap-files-yaml');
    if (capiBootstrapFilesInput) {
        capiBootstrapFilesInput.value = Array.isArray(nodePool.capiBootstrapFiles) && nodePool.capiBootstrapFiles.length > 0
            ? formatManifestForDisplay(nodePool.capiBootstrapFiles)
            : '';
    }
    const capiPreKubeadmCommandsInput = document.getElementById('edit-nodepool-capi-pre-kubeadm-commands-yaml');
    if (capiPreKubeadmCommandsInput) {
        capiPreKubeadmCommandsInput.value = Array.isArray(nodePool.capiPreKubeadmCommands) && nodePool.capiPreKubeadmCommands.length > 0
            ? formatManifestForDisplay(nodePool.capiPreKubeadmCommands)
            : '';
    }

    // Populate config profiles
    const configProfileSelect = document.getElementById('edit-nodepool-config-profile');
    if (configProfileSelect) {
        configProfileSelect.innerHTML = `<option value="">${isCAPICluster ? 'None' : 'Select a config profile'}</option>`;
        const configProfileNames = new Set();
        if (state.configProfiles && state.configProfiles.length > 0) {
            for (const profile of state.configProfiles) {
                const option = document.createElement('option');
                option.value = profile.name;
                option.textContent = profile.name;
                configProfileSelect.appendChild(option);
                configProfileNames.add(profile.name);
            }
        }
        // If the current profile is deprecated (not in the list), add it with a deprecated indicator
        if (nodePool.configProfile && !configProfileNames.has(nodePool.configProfile)) {
            const deprecatedOption = document.createElement('option');
            deprecatedOption.value = nodePool.configProfile;
            deprecatedOption.textContent = `${nodePool.configProfile} (Deprecated)`;
            configProfileSelect.appendChild(deprecatedOption);
        }
        configProfileSelect.value = nodePool.configProfile || '';
    }

    // Reset submit button state
    const submitButton = document.querySelector('#edit-nodepool-modal button[onclick="handleEditNodePoolFormSubmit()"]');
    if (submitButton) {
        submitButton.disabled = false;
    }

    document.getElementById('edit-nodepool-error').style.display = 'none';
    document.getElementById('edit-nodepool-loading').style.display = 'none';
    applyNodePoolInfrastructureFields(document.getElementById('edit-nodepool-modal'), isCAPICluster);
    document.getElementById('edit-nodepool-modal').style.display = 'block';
}

function renderEditNodePoolLoading() {
    document.getElementById('edit-nodepool-error').style.display = 'none';
    document.getElementById('edit-nodepool-loading').textContent = 'Saving node pool...';
    document.getElementById('edit-nodepool-loading').style.display = 'block';
}

function renderEditNodePoolError(error) {
    document.getElementById('edit-nodepool-loading').style.display = 'none';
    document.getElementById('edit-nodepool-error').textContent = `Error saving node pool: ${error}`;
    document.getElementById('edit-nodepool-error').style.display = 'block';
}

async function handleEditNodePoolFormSubmit() {
    // Prevent multiple simultaneous submissions
    const submitButton = document.querySelector('#edit-nodepool-modal button[onclick="handleEditNodePoolFormSubmit()"]');
    if (submitButton && submitButton.disabled) {
        return;
    }
    
    if (submitButton) {
        submitButton.disabled = true;
    }

    const nodePoolID = document.getElementById('edit-nodepool-id').value;
    const description = document.getElementById('edit-nodepool-description').value;
    const desiredNodeCount = parseInt(document.getElementById('edit-nodepool-desired-count').value, 10) || 0;
    const autoProvisionDisabled = document.getElementById('edit-nodepool-auto-provision-disabled').checked;
    const autoModeDisabled = document.getElementById('edit-nodepool-auto-mode-disabled').checked;
    const recycleBudget = parseInt(document.getElementById('edit-nodepool-recycle-budget').value, 10) || 0;
    const diskSelectionStrategy = document.getElementById('edit-nodepool-disk-selection-strategy').value;
    const osProfile = document.getElementById('edit-nodepool-os-profile').value;
    const configProfile = document.getElementById('edit-nodepool-config-profile').value;
    const systemReservedMemory = document.getElementById('edit-nodepool-system-reserved-memory').value;
    const kubeReservedMemory = document.getElementById('edit-nodepool-kube-reserved-memory').value;
    const isCAPICluster = clusterUsesCAPIInfrastructure(state.selectedClusterDetails);
    let capiInfraMachineTemplate;
    let capiSpireAttestationMethod;
    let capiBootstrapFiles;
    let capiPreKubeadmCommands;
    try {
        if (isCAPICluster) {
            capiInfraMachineTemplate = parseYAMLManifestToJSONObject(
                document.getElementById('edit-nodepool-capi-infra-machine-template-yaml').value,
                'CAPI infra machine template'
            );
            capiSpireAttestationMethod = getSpireAttestationMethodFromInput(
                document.getElementById('edit-nodepool-capi-spire-attestation-method')
            );
            capiBootstrapFiles = parseCapiBootstrapFilesInput(
                document.getElementById('edit-nodepool-capi-bootstrap-files-yaml').value,
                'CAPI bootstrap files'
            );
            capiPreKubeadmCommands = parseCapiPreKubeadmCommandsInput(
                document.getElementById('edit-nodepool-capi-pre-kubeadm-commands-yaml').value,
                'CAPI pre-kubeadm commands'
            );
            if (!capiInfraMachineTemplate) {
                throw new Error('CAPI infra machine template is required for CAPI-backed node pools');
            }
        }
    } catch (error) {
        renderEditNodePoolError(error.message || 'Invalid CAPI infra machine template YAML');
        if (submitButton) {
            submitButton.disabled = false;
        }
        return;
    }
    
    const payload = {
        description: description,
        desiredNodeCount: desiredNodeCount,
        autoProvisionDisabled: autoProvisionDisabled,
        autoModeDisabled: autoModeDisabled,
        recycleBudget: recycleBudget,
        configProfile: configProfile,
        systemReservedMemory: systemReservedMemory || undefined,
        kubeReservedMemory: kubeReservedMemory || undefined,
    };
    if (isCAPICluster) {
        payload.capiInfraMachineTemplate = capiInfraMachineTemplate;
        payload.capiSpireAttestationMethod = capiSpireAttestationMethod || undefined;
        payload.capiBootstrapFiles = capiBootstrapFiles;
        payload.capiPreKubeadmCommands = capiPreKubeadmCommands;
    }
    if (!isCAPICluster) {
        payload.diskSelectionStrategy = diskSelectionStrategy;
        payload.osProfile = osProfile;
    }

    // Include kubernetesVersion for worker node pools (disabled = control plane, skip)
    const k8sVersionInput = document.getElementById('edit-nodepool-kubernetes-version');
    if (!k8sVersionInput.disabled) {
        const k8sVersion = k8sVersionInput.value.trim();
        if (k8sVersion) {
            payload.kubernetesVersion = k8sVersion;
        }
    }

    // Include maxSurge
    const maxSurgeGroup = document.getElementById('edit-nodepool-max-surge-group');
    if (maxSurgeGroup.style.display !== 'none') {
        const maxSurge = parseInt(document.getElementById('edit-nodepool-max-surge').value, 10);
        if (!isNaN(maxSurge)) {
            payload.maxSurge = maxSurge;
        }
    }

    // Include subnetIDs or vpcPrefixIDs from the subnet/VPC prefix checkboxes (only one, per API).
    // API: omit/null = no change; [] = replace with empty (delete all). By default existing are pre-checked; if user removes them we send [].
    if (!isCAPICluster) {
        // Only transmit the selection when the checkboxes actually rendered. If the container is empty
        // because Forge data couldn't load (not because the user cleared the selection), sending [] would
        // be read by the API as "clear all" and silently wipe the pool's networking. Omit = no change.
        const renderedBoxes = document.querySelectorAll('#edit-nodepool-subnets-container input[type="checkbox"]');
        if (renderedBoxes.length > 0) {
            const selectedIDs = Array.from(renderedBoxes).filter(cb => cb.checked).map(cb => cb.value);
            const cluster = state.selectedClusterDetails;
            const forgeSite = cluster?.siteID ? state.forgeSites.find(fs => fs.id === cluster.siteID) : null;
            const isNativeNetworking = forgeSite?.capabilities?.nativeNetworking || false;
            if (isNativeNetworking) {
                payload.vpcPrefixIDs = selectedIDs;
            } else {
                payload.subnetIDs = selectedIDs;
            }
        }
    }

    renderEditNodePoolLoading();

    try {
        await updateNodePool(nodePoolID, payload);
        closeModal('edit-nodepool-modal');
        refreshData();
    } catch (error) {
        console.error("Failed to update node pool:", error);
        renderEditNodePoolError(error.message || 'An unknown error occurred');
        if (submitButton) {
            submitButton.disabled = false;
        }
    }
}

async function showNodeHistoryModal(nodeID, nodeName) {
    document.getElementById('node-history-node-name').textContent = nodeName || nodeID;
    document.getElementById('node-history-content').innerHTML = '';
    document.getElementById('node-history-error').style.display = 'none';
    document.getElementById('node-history-loading').style.display = 'block';
    document.getElementById('node-history-loading').textContent = 'Loading workflow history...';
    document.getElementById('node-history-modal').style.display = 'flex';

    try {
        const history = await getNodeHistory(nodeID);
        document.getElementById('node-history-loading').style.display = 'none';
        renderNodeHistory(history);
    } catch (err) {
        document.getElementById('node-history-loading').style.display = 'none';
        const errorEl = document.getElementById('node-history-error');
        errorEl.textContent = `Failed to load history: ${err.message}`;
        errorEl.style.display = 'block';
    }
}

function renderNodeHistory(history) {
    const container = document.getElementById('node-history-content');
    container.innerHTML = '';

    if (!history || !history.workflows || history.workflows.length === 0) {
        const empty = document.createElement('p');
        empty.className = 'node-history-empty';
        empty.textContent = 'No workflow history found for this node (history is retained for 30 days).';
        container.appendChild(empty);
        return;
    }

    history.workflows.forEach(wf => {
        const section = document.createElement('div');
        section.className = 'node-history-workflow';

        const header = document.createElement('div');
        header.className = `node-history-workflow-header node-history-status-${wf.status.toLowerCase()}`;

        const titleSpan = state.temporalUIURL
            ? document.createElement('a')
            : document.createElement('span');
        titleSpan.className = 'node-history-workflow-title';
        titleSpan.textContent = wf.workflowType;
        if (state.temporalUIURL) {
            titleSpan.href = `${state.temporalUIURL}/namespaces/${state.temporalNamespace}/workflows/${encodeURIComponent(wf.workflowID)}/${encodeURIComponent(wf.runID)}`;
            titleSpan.target = '_blank';
            titleSpan.rel = 'noopener noreferrer';
        }

        const metaSpan = document.createElement('span');
        metaSpan.className = 'node-history-workflow-meta';
        const start = new Date(wf.startTime);
        const durationStr = formatDurationMs(wf.durationMs);
        metaSpan.textContent = `${start.toLocaleString()} — ${durationStr}`;

        const statusBadge = document.createElement('span');
        statusBadge.className = `node-history-badge node-history-badge-${wf.status.toLowerCase()}`;
        statusBadge.textContent = wf.status;

        header.appendChild(titleSpan);
        header.appendChild(metaSpan);
        header.appendChild(statusBadge);

        const toggle = document.createElement('button');
        toggle.className = 'node-history-toggle';
        toggle.textContent = '▶';
        toggle.title = 'Show activity breakdown';
        header.appendChild(toggle);

        section.appendChild(header);

        const details = document.createElement('div');
        details.className = 'node-history-activities';
        details.style.display = 'none';

        if (wf.activities && wf.activities.length > 0) {
            const table = document.createElement('table');
            table.className = 'node-history-activity-table';

            const thead = document.createElement('thead');
            thead.innerHTML = '<tr><th>Activity</th><th>Started</th><th>Duration</th><th>Status</th></tr>';
            table.appendChild(thead);

            const tbody = document.createElement('tbody');
            wf.activities.forEach(act => {
                const tr = document.createElement('tr');
                tr.className = `node-history-activity-row node-history-activity-${act.status.toLowerCase()}`;

                const nameCell = document.createElement('td');
                nameCell.textContent = act.name;
                if (act.failureMessage) {
                    nameCell.title = act.failureMessage;
                    nameCell.className = 'node-history-activity-name-failed';
                }

                const timeCell = document.createElement('td');
                timeCell.textContent = new Date(act.scheduledTime).toLocaleTimeString();
                timeCell.title = new Date(act.scheduledTime).toLocaleString();

                const durCell = document.createElement('td');
                durCell.textContent = formatDurationMs(act.durationMs);
                durCell.className = durationClass(act.durationMs);

                const statusCell = document.createElement('td');
                statusCell.textContent = act.status;
                statusCell.className = `node-history-badge node-history-badge-${act.status.toLowerCase()}`;

                tr.appendChild(nameCell);
                tr.appendChild(timeCell);
                tr.appendChild(durCell);
                tr.appendChild(statusCell);
                tbody.appendChild(tr);
            });
            table.appendChild(tbody);
            details.appendChild(table);
        } else {
            const none = document.createElement('p');
            none.className = 'node-history-empty';
            none.textContent = 'No activity detail available.';
            details.appendChild(none);
        }

        section.appendChild(details);
        container.appendChild(section);

        toggle.addEventListener('click', () => {
            const open = details.style.display !== 'none';
            details.style.display = open ? 'none' : 'block';
            toggle.textContent = open ? '▶' : '▼';
            toggle.title = open ? 'Show activity breakdown' : 'Hide activity breakdown';
        });
    });
}

function formatDurationMs(ms) {
    if (ms < 1000) return `${ms}ms`;
    const totalSec = Math.floor(ms / 1000);
    if (totalSec < 60) return `${totalSec}s`;
    const mins = Math.floor(totalSec / 60);
    const secs = totalSec % 60;
    if (mins < 60) return secs > 0 ? `${mins}m ${secs}s` : `${mins}m`;
    const hrs = Math.floor(mins / 60);
    const remMins = mins % 60;
    return remMins > 0 ? `${hrs}h ${remMins}m` : `${hrs}h`;
}

function durationClass(ms) {
    if (ms > 30 * 60 * 1000) return 'node-history-duration-long';
    if (ms > 5 * 60 * 1000) return 'node-history-duration-medium';
    return '';
}

function showDeleteNodeModal(nodeID, nodeName) {
    document.getElementById('delete-node-modal').setAttribute('data-node-id', nodeID);
    document.getElementById('delete-node-name').textContent = nodeName;
    
    document.getElementById('delete-node-flag-for-repair').checked = false;
    document.getElementById('delete-node-category').value = '';
    document.getElementById('delete-node-summary').value = '';
    document.getElementById('delete-node-details').value = '';
    document.getElementById('machine-health-issue-fields').style.display = 'none';

    document.getElementById('delete-node-force-unreachable').checked = false;
    document.getElementById('delete-node-allow-single-voter').checked = false;
    document.getElementById('delete-node-allow-quorum-loss').checked = false;
    document.getElementById('force-unreachable-fields').style.display = 'none';

    document.getElementById('delete-node-modal').style.display = 'block';
    document.getElementById('delete-node-error').style.display = 'none';
    document.getElementById('delete-node-loading').style.display = 'none';
}

function toggleMachineHealthIssueFields() {
    const checkbox = document.getElementById('delete-node-flag-for-repair');
    const fieldsContainer = document.getElementById('machine-health-issue-fields');
    
    if (checkbox.checked) {
        fieldsContainer.style.display = 'block';
    } else {
        fieldsContainer.style.display = 'none';
        document.getElementById('delete-node-category').value = '';
        document.getElementById('delete-node-summary').value = '';
        document.getElementById('delete-node-details').value = '';
    }
}

function toggleForceUnreachableFields() {
    const checkbox = document.getElementById('delete-node-force-unreachable');
    const fieldsContainer = document.getElementById('force-unreachable-fields');

    if (checkbox.checked) {
        fieldsContainer.style.display = 'block';
    } else {
        fieldsContainer.style.display = 'none';
        document.getElementById('delete-node-allow-single-voter').checked = false;
        document.getElementById('delete-node-allow-quorum-loss').checked = false;
    }
}

function renderDeleteNodeLoading() {
    document.getElementById('delete-node-error').style.display = 'none';
    document.getElementById('delete-node-loading').innerHTML = 'Deleting node...';
    document.getElementById('delete-node-loading').style.display = 'block';
}

function renderDeleteNodeError(error) {
    document.getElementById('delete-node-loading').style.display = 'none';
    document.getElementById('delete-node-error').innerHTML = `Error deleting node: ${error}`;
    document.getElementById('delete-node-error').style.display = 'block';
}

function getCreateNodePoolFormData() {
    const clusterID = document.getElementById('nodepool-cluster-id').value;
    const name = document.getElementById('nodepool-name').value;
    const namePrefix = document.getElementById('nodepool-name-prefix').value;
    const description = document.getElementById('nodepool-description').value.trim(); 

    // Get site capabilities from the selected cluster
    const cluster = state.selectedClusterDetails;
    const isCAPICluster = clusterUsesCAPIInfrastructure(cluster);
    const forgeSite = cluster?.siteID ? state.forgeSites.find(fs => fs.id === cluster.siteID) : null;
    const capiInfraMachineTemplate = isCAPICluster
        ? parseYAMLManifestToJSONObject(
            document.getElementById('nodepool-capi-infra-machine-template-yaml').value,
            'CAPI infra machine template'
        )
        : undefined;
    const capiSpireAttestationMethod = isCAPICluster
        ? getSpireAttestationMethodFromInput(
            document.getElementById('nodepool-capi-spire-attestation-method')
        )
        : undefined;
    const capiBootstrapFiles = isCAPICluster
        ? parseCapiBootstrapFilesInput(
            document.getElementById('nodepool-capi-bootstrap-files-yaml').value,
            'CAPI bootstrap files'
        )
        : undefined;
    const capiPreKubeadmCommands = isCAPICluster
        ? parseCapiPreKubeadmCommandsInput(
            document.getElementById('nodepool-capi-pre-kubeadm-commands-yaml').value,
            'CAPI pre-kubeadm commands'
        )
        : undefined;
    if (isCAPICluster && !capiInfraMachineTemplate) {
        throw new Error('CAPI infra machine template is required when the node pool uses CAPI infrastructure');
    }
    const isNativeNetworkingEnabled = !isCAPICluster && (forgeSite?.capabilities?.nativeNetworking || false);
    
    const checkboxes = document.querySelectorAll('#nodepool-subnets-container input[type="checkbox"]:checked');
    const selectedIDs = Array.from(checkboxes).map(cb => cb.value);
    const subnetIDs = isCAPICluster ? [] : (isNativeNetworkingEnabled ? [] : selectedIDs);
    const vpcPrefixIDs = isCAPICluster ? [] : (isNativeNetworkingEnabled ? selectedIDs : []);
    
    const instanceTypeID = isCAPICluster ? '' : document.getElementById('nodepool-instance-type-id').value;
    const infinibandPartitionID = isCAPICluster ? '' : document.getElementById('nodepool-infiniband-partition-id').value;
    const nvLinkLogicalPartitionID = isCAPICluster ? '' : document.getElementById('nodepool-nvlink-logical-partition-id').value;
    const osProfile = document.getElementById('nodepool-os-profile').value;
    const configProfile = document.getElementById('nodepool-config-profile').value;
    const systemReservedMemory = document.getElementById('nodepool-system-reserved-memory').value;
    const kubeReservedMemory = document.getElementById('nodepool-kube-reserved-memory').value;

    const sshKeyGroupCheckboxes = document.querySelectorAll('#nodepool-ssh-key-groups-container input[type="checkbox"]:checked');
    const sshKeyGroups = isCAPICluster ? [] : Array.from(sshKeyGroupCheckboxes).map(cb => cb.value);

    const desiredNodeCount = parseInt(document.getElementById('nodepool-desired-node-count').value, 10) || 0;
    const isControlPlane = document.getElementById('nodepool-control-plane').checked;
    const autoProvisionDisabled = document.getElementById('nodepool-auto-provision-disabled').checked;
    const autoModeDisabled = document.getElementById('nodepool-auto-mode-disabled').checked;
    const recycleBudget = parseInt(document.getElementById('nodepool-recycle-budget').value, 10) || 0;
    const diskSelectionStrategyEl = document.getElementById('nodepool-disk-selection-strategy');
    const diskSelectionStrategy = diskSelectionStrategyEl ? diskSelectionStrategyEl.value : 'smallest';

    const result = {
        clusterID: clusterID,
        name: name,
        nodeNamePrefix: namePrefix,
        description: description || undefined,
        configProfile: configProfile,
        systemReservedMemory: systemReservedMemory || undefined,
        kubeReservedMemory: kubeReservedMemory || undefined,
        desiredNodeCount: desiredNodeCount,
        controlPlane: isControlPlane,
        autoProvisionDisabled: autoProvisionDisabled,
        autoModeDisabled: autoModeDisabled,
        recycleBudget: recycleBudget,
    };

    if (isCAPICluster) {
        result.capiInfraMachineTemplate = capiInfraMachineTemplate || undefined;
        result.capiSpireAttestationMethod = capiSpireAttestationMethod || undefined;
        result.capiBootstrapFiles = capiBootstrapFiles;
        result.capiPreKubeadmCommands = capiPreKubeadmCommands;
    } else {
        result.subnetIDs = subnetIDs;
        result.vpcPrefixIDs = vpcPrefixIDs;
        result.instanceTypeID = instanceTypeID || undefined;
        result.infinibandPartitionID = infinibandPartitionID || undefined;
        result.osProfile = osProfile || undefined;
        result.nvLinkLogicalPartitionID = nvLinkLogicalPartitionID || undefined;
        result.sshKeyGroupIDs = sshKeyGroups;
        result.diskSelectionStrategy = diskSelectionStrategy || undefined;
    }

    return result;
}

function closeModal(modalId) {
    document.getElementById(modalId).style.display = 'none';
}

// Helper function to update node pool selections based on parent selectors (org, site, vpc)
function updateNodePoolSelectionsFromParent(nodePoolIdx) {
    if (createClusterUsesCAPIInfrastructure()) {
        return;
    }

    const orgIdSelect = document.getElementById('org-id-select');
    const hiddenSiteInput = document.getElementById('hidden-site-id');
    const vpcIdSelect = document.getElementById('vpc-id-select');

    if (orgIdSelect && orgIdSelect.value && hiddenSiteInput && hiddenSiteInput.value) {
        if (vpcIdSelect && vpcIdSelect.value) {
            loadForgeData(orgIdSelect.value, hiddenSiteInput.value, vpcIdSelect.value).then(forgeData => {
                updateNodePoolsWithForgeData(forgeData);
                updateNodePoolsWithSubnetData(forgeData.subnets, forgeData.vpcPrefixes, forgeData.instances);
            });
        } else {
            loadForgeData(orgIdSelect.value, hiddenSiteInput.value).then(forgeData => {
                updateNodePoolsWithForgeData(forgeData);
            });
        }
    }
}

function showCreateSiteModal() {
    const form = document.getElementById('create-site-form');
    if (form) {
        form.reset();
    }

    document.getElementById('create-site-modal').style.display = 'block';
    document.getElementById('create-site-error').style.display = 'none';
    document.getElementById('create-site-loading').style.display = 'none';
    
    populateAvailableForgeSites();
}

function getCreateSiteFormData() {
    const vipRangesInput = document.getElementById('site-vip-ranges').value.trim();
    const vipRanges = vipRangesInput.split(',').map(range => range.trim()).filter(range => range.length > 0);
    
    return {
        siteId: document.getElementById('site-id').value.trim(),
        siteControllerVip: document.getElementById('site-controller-vip').value.trim(),
        vipRanges: vipRanges
    };
}

function renderCreateSiteLoading() {
    document.getElementById('create-site-error').style.display = 'none';
    document.getElementById('create-site-loading').style.display = 'block';
    document.getElementById('create-site-loading').textContent = 'Registering site and allocating VIP...';
}

function renderCreateSiteError(error) {
    document.getElementById('create-site-loading').style.display = 'none';
    document.getElementById('create-site-error').innerHTML = `Error registering site: ${error}`;
    document.getElementById('create-site-error').style.display = 'block';
}

async function populateAvailableForgeSites() {
    const siteIdContainer = document.getElementById('site-id').parentNode;
    
    const existingInput = document.getElementById('site-id');
    const existingDatalist = document.getElementById('forge-sites-datalist');
    if (existingDatalist) {
        existingDatalist.remove();
    }
    
    const selectElement = document.createElement('select');
    selectElement.id = 'site-id';
    selectElement.required = true;
    
    const defaultOption = document.createElement('option');
    defaultOption.value = '';
    defaultOption.textContent = 'Loading available sites...';
    defaultOption.disabled = true;
    defaultOption.selected = true;
    selectElement.appendChild(defaultOption);
    
    existingInput.replaceWith(selectElement);

    try {
        const registeredSiteIds = new Set((state.registeredSites || []).map(site => site.id));
        const availableSites = [];

        if (!registeredSiteIds.has(DEFAULT_SITE_ID)) {
            availableSites.push({ id: DEFAULT_SITE_ID, name: DEFAULT_SITE_NAME });
        }

        // Deduplicate forge sites from pre-fetched data because some sites belong to multiple tenants.
        const uniqueForgeSites = (state.forgeSites || []).filter((site, index, self) =>
            index === self.findIndex(s => s.id === site.id)
        );

        const unregisteredForgeSites = uniqueForgeSites.filter(site => !registeredSiteIds.has(site.id));
        availableSites.push(...unregisteredForgeSites);

        const sortedUnregisteredSites = availableSites.sort((a, b) => {
            if (a.id === DEFAULT_SITE_ID) {
                return -1;
            }
            if (b.id === DEFAULT_SITE_ID) {
                return 1;
            }
            return getSiteDisplayName(a).localeCompare(getSiteDisplayName(b));
        });

        selectElement.innerHTML = '';

        if (sortedUnregisteredSites.length > 0) {
            const defaultOption = document.createElement('option');
            defaultOption.value = '';
            defaultOption.textContent = 'Select a site to register...';
            defaultOption.disabled = true;
            defaultOption.selected = true;
            selectElement.appendChild(defaultOption);

            sortedUnregisteredSites.forEach(site => {
                const option = document.createElement('option');
                option.value = site.id;
                option.textContent = formatSelectedSiteDisplay(site.id);
                selectElement.appendChild(option);
            });

            selectElement.title = 'Select a site to register';
            selectElement.disabled = false;
        } else {
            const noSitesOption = document.createElement('option');
            noSitesOption.value = '';
            noSitesOption.textContent = 'No unregistered sites available';
            noSitesOption.disabled = true;
            selectElement.appendChild(noSitesOption);
            selectElement.disabled = true;
        }
    } catch (error) {
        console.warn('Error processing available sites:', error);
        selectElement.innerHTML = '';

        if ((state.registeredSites || []).some(site => site.id === DEFAULT_SITE_ID)) {
            const errorOption = document.createElement('option');
            errorOption.value = '';
            errorOption.textContent = 'Error loading sites';
            errorOption.disabled = true;
            selectElement.appendChild(errorOption);
            selectElement.disabled = true;
            return;
        }

        const defaultOption = document.createElement('option');
        defaultOption.value = '';
        defaultOption.textContent = 'Select a site to register...';
        defaultOption.disabled = true;
        defaultOption.selected = true;
        selectElement.appendChild(defaultOption);

        const siteOption = document.createElement('option');
        siteOption.value = DEFAULT_SITE_ID;
        siteOption.textContent = DEFAULT_SITE_NAME;
        selectElement.appendChild(siteOption);
        selectElement.title = 'Select a site to register';
        selectElement.disabled = false;
    }
}

// === FORM HANDLERS ===

async function handleCreateSiteSubmit(e) {
    e.preventDefault();
    
    // Prevent multiple simultaneous submissions
    const submitButton = e.target.querySelector('button[type="submit"]');
    if (submitButton && submitButton.disabled) {
        return;
    }
    
    if (submitButton) {
        submitButton.disabled = true;
    }
    
    try {
        renderCreateSiteLoading();
        const formData = getCreateSiteFormData();
        
        const newSite = await createSite({
            id: formData.siteId,
            vipRanges: formData.vipRanges
        });
        
        // Only create VIP allocation if Site Controller VIP is provided
        if (formData.siteControllerVip && formData.siteControllerVip.trim()) {
            await createVIPAllocation({
                siteID: formData.siteId,
                vip: formData.siteControllerVip,
                allocationType: 'siteController'
            });
        }
        
        await loadClustersAndRegisteredSites();
        closeModal('create-site-modal');
        
        state.expandedSites.add(newSite.id);
        renderClusters();
    } catch (error) {
        renderCreateSiteError(error.message);
        if (submitButton) {
            submitButton.disabled = false;
        }
    }
}

async function handleCreateClusterSubmit(e) {
    e.preventDefault();
    
    // Prevent multiple simultaneous submissions
    const submitButton = e.target.querySelector('button[type="submit"]');
    if (submitButton && submitButton.disabled) {
        return;
    }
    
    if (submitButton) {
        submitButton.disabled = true;
    }
    
    try {
        renderCreateClusterLoading();
        const clusterData = getCreateClusterFormData();
        const newCluster = await createCluster(clusterData);
        await loadClustersAndRegisteredSites();
        closeModal('create-cluster-modal');
        selectCluster(newCluster.id);
    } catch (error) {
        renderCreateClusterError(error.message);
        if (submitButton) {
            submitButton.disabled = false;
        }
    }
}

async function handleCreateNodePoolSubmit(e) {
    e.preventDefault();
    
    // Prevent multiple simultaneous submissions
    const submitButton = e.target.querySelector('button[type="submit"]');
    if (submitButton && submitButton.disabled) {
        return;
    }
    
    if (submitButton) {
        submitButton.disabled = true;
    }
    
    try {
        renderCreateNodePoolLoading();
        const nodePoolData = getCreateNodePoolFormData();
        await createNodePool(nodePoolData);
        closeModal('create-nodepool-modal');
        await loadNodePools();
    } catch (error) {
        renderCreateNodePoolError(error.message);
        if (submitButton) {
            submitButton.disabled = false;
        }
    }
}

async function handleDeleteSelectedCluster() {
    if (!state.selectedClusterDetails) return;
    
    try {
        renderDeleteClusterLoading();
        await deleteCluster(state.selectedClusterDetails.id);
        closeModal('delete-cluster-modal');
        refreshData();
    } catch (error) {
        renderDeleteClusterError(error.message);
    }
}

async function handleDeleteNodePool() {
    const nodePoolId = document.getElementById('delete-nodepool-modal').getAttribute('data-nodepool-id');
    if (!nodePoolId) return;
    
    try {
        renderDeleteNodePoolLoading();
        await deleteNodePool(nodePoolId);
        closeModal('delete-nodepool-modal');
        refreshData();
    } catch (error) {
        renderDeleteNodePoolError(error.message);
    }
}

async function handleDeleteNode() {
    const nodeId = document.getElementById('delete-node-modal').getAttribute('data-node-id');
    if (!nodeId) return;
    
    const flagForRepair = document.getElementById('delete-node-flag-for-repair').checked;
    const forceUnreachable = document.getElementById('delete-node-force-unreachable').checked;
    const allowSingleVoter = forceUnreachable && document.getElementById('delete-node-allow-single-voter').checked;
    const allowQuorumLoss = forceUnreachable && document.getElementById('delete-node-allow-quorum-loss').checked;
    let requestBody = null;

    if (flagForRepair) {
        const category = document.getElementById('delete-node-category').value;
        const summary = document.getElementById('delete-node-summary').value.trim();
        const details = document.getElementById('delete-node-details').value.trim();

        if (!category) {
            renderDeleteNodeError('Please select an issue category');
            return;
        }

        if (!summary) {
            renderDeleteNodeError('Please provide an issue summary');
            return;
        }

        requestBody = {
            machineHealthIssue: {
                category: category,
                summary: summary
            }
        };

        if (details) {
            requestBody.machineHealthIssue.details = details;
        }
    }

    if (forceUnreachable || allowSingleVoter || allowQuorumLoss) {
        requestBody = requestBody || {};
        requestBody.forceUnreachable = forceUnreachable;
        if (allowSingleVoter) {
            requestBody.allowSingleVoter = true;
        }
        if (allowQuorumLoss) {
            requestBody.allowQuorumLoss = true;
        }
    }

    try {
        renderDeleteNodeLoading();
        await deleteNode(nodeId, requestBody);
        closeModal('delete-node-modal');
        refreshData();
    } catch (error) {
        renderDeleteNodeError(error.message);
    }
}

// === AVAILABILITY CALCULATION HELPERS ===

function calculateSubnetAvailability(subnetID, forgeData = null) {
    const data = forgeData || state.selectedClusterForgeData;
    if (!data || !subnetID) return { available: -1, total: '?' };
    
    const subnet = data.subnets.find(s => s.id === subnetID);
    if (!subnet) return { available: -1, total: '?' };


    const totalIPs = calculateSubnetAvailableIPs(subnet.prefixLength);
    if (totalIPs === '?') return { available: -1, total: '?' };
    
    let usedIPs = 0;
    if (data.instances) {
        data.instances.forEach(instance => {
            if (instance.subnetId === subnetID) {
                usedIPs++;
            }
        });
    }
    
    const available = Math.max(0, totalIPs - usedIPs);
    return { available, total: totalIPs };
}

function calculateSubnetAvailableIPs(prefixLength) {
    if (prefixLength === undefined || prefixLength === null) {
        return '?';
    }
    const totalIPs = Math.pow(2, 32 - prefixLength);
    return Math.max(0, totalIPs - 3);
}

function calculateVpcPrefixIpAvailability(vpcPrefixID, forgeData = null) {
    const data = forgeData || state.selectedClusterForgeData;
    if (!data || !vpcPrefixID) return { available: -1, total: '?' };
    
    const vpcPrefix = data.vpcPrefixes?.find(vp => vp.id === vpcPrefixID);
    if (!vpcPrefix) return { available: -1, total: '?' };
    
    if (vpcPrefix.prefixLength === undefined || vpcPrefix.prefixLength === null) {
        return { available: -1, total: '?' };
    }
    const totalIPs = Math.pow(2, 32 - vpcPrefix.prefixLength);
    
    let usedIPs = 0;
    if (data.instances) {
        data.instances.forEach(instance => {
            if (instance.vpcPrefixId === vpcPrefixID) {
                usedIPs += 2; // 2 IPs per instance in FNN
            }
        });
    }
    
    const available = Math.max(0, totalIPs - usedIPs); // 2 IPs per instance in FNN
    return { available, total: totalIPs };
}

function getInstanceTypeAvailability(instanceTypeID) {
    if (!state.selectedClusterForgeData || !instanceTypeID) return null;
    
    const instanceType = state.selectedClusterForgeData.instanceTypes.find(it => it.id === instanceTypeID);
    return instanceType || null;
}

function isGPUInstanceType(instanceTypeID) {
    const instanceType = getInstanceTypeAvailability(instanceTypeID);
    return instanceType && instanceType.name && instanceType.name.toLowerCase().includes('gpu');
}

// === CLUSTER UPDATE HELPERS ===

function showToast(message, type = 'success') {
    // Create toast container if it doesn't exist
    let toastContainer = document.getElementById('toast-container');
    if (!toastContainer) {
        toastContainer = document.createElement('div');
        toastContainer.id = 'toast-container';
        toastContainer.style.cssText = 'position: fixed; top: 20px; right: 20px; z-index: 10000;';
        document.body.appendChild(toastContainer);
    }

    // Create toast element
    const toast = document.createElement('div');
    toast.className = `toast toast-${type}`;
    toast.style.cssText = `
        padding: 12px 20px;
        margin-bottom: 10px;
        border-radius: 4px;
        background-color: ${type === 'error' ? '#f44336' : '#4CAF50'};
        color: white;
        box-shadow: 0 2px 5px rgba(0,0,0,0.2);
        animation: slideIn 0.3s ease-out;
    `;
    toast.textContent = message;

    toastContainer.appendChild(toast);

    // Auto-remove after 3 seconds
    setTimeout(() => {
        toast.style.animation = 'slideOut 0.3s ease-out';
        setTimeout(() => toast.remove(), 300);
    }, 3000);
}

const CONFIG_FIELD_LABELS = {
    osProfile:                  'OS Profile',
    configProfile:              'Config Profile',
    kubernetesVersion:          'Kubernetes Version',
    systemReservedMemory:       'System Reserved Memory',
    kubeReservedMemory:         'Kube Reserved Memory',
    diskSelectionStrategy:      'Disk Selection Strategy',
    teleportEnabled:            'Teleport',
    subnetIDs:                  'Subnets',
    vpcPrefixIDs:               'VPC Prefixes',
    capiInfraMachineTemplate:   'CAPI Machine Template',
    capiBootstrapFiles:         'CAPI Bootstrap Files',
    capiPreKubeadmCommands:     'CAPI Pre-kubeadm Commands',
    capiSpireAttestationMethod: 'SPIRE Attestation Method',
};

function formatConfigFieldValue(rawJSON, fieldName) {
    let parsed;
    try { parsed = JSON.parse(rawJSON); } catch { return rawJSON; }

    if (typeof parsed === 'boolean') {
        return parsed ? 'enabled' : 'disabled';
    }
    if (parsed === null || parsed === undefined) return '—';
    if (Array.isArray(parsed)) {
        if (parsed.length === 0) return '(none)';
        if (fieldName === 'subnetIDs' || fieldName === 'vpcPrefixIDs') {
            return parsed.length === 1 ? '1 item' : `${parsed.length} items`;
        }
        if (fieldName === 'capiPreKubeadmCommands') {
            return parsed.length === 1 ? '1 command' : `${parsed.length} commands`;
        }
        if (fieldName === 'capiBootstrapFiles') {
            return parsed.length === 1 ? '1 file' : `${parsed.length} files`;
        }
        return JSON.stringify(parsed);
    }
    if (typeof parsed === 'object') {
        const keys = Object.keys(parsed);
        if (keys.length === 0) return '(empty)';
        const preview = JSON.stringify(parsed);
        if (preview.length <= 80) return preview;
        return `${preview.slice(0, 77)}…`;
    }
    return String(parsed);
}

async function showNodePoolConfigHistoryModal(nodePoolID, nodePoolName) {
    const modal = document.getElementById('nodepool-config-history-modal');
    const nameEl = document.getElementById('nodepool-config-history-name');
    const loadingEl = document.getElementById('nodepool-config-history-loading');
    const errorEl = document.getElementById('nodepool-config-history-error');
    const contentEl = document.getElementById('nodepool-config-history-content');

    nameEl.textContent = nodePoolName || nodePoolID;
    loadingEl.textContent = 'Loading...';
    loadingEl.style.display = 'block';
    errorEl.style.display = 'none';
    contentEl.innerHTML = '';
    modal.style.display = 'block';

    let history;
    try {
        history = await getNodePoolConfigHistory(nodePoolID);
    } catch (err) {
        loadingEl.style.display = 'none';
        errorEl.textContent = `Failed to load config history: ${err.message}`;
        errorEl.style.display = 'block';
        return;
    }

    loadingEl.style.display = 'none';

    const changes = history.changes || [];
    if (changes.length === 0) {
        contentEl.innerHTML = '<p>No config history found for this node pool.</p>';
        return;
    }

    const table = document.createElement('table');
    table.className = 'node-table';
    table.style.width = '100%';

    const thead = document.createElement('thead');
    thead.innerHTML = '<tr><th>Date</th><th>Status</th><th>Changed Fields</th></tr>';
    table.appendChild(thead);

    const tbody = document.createElement('tbody');

    for (const change of changes) {
        const tr = document.createElement('tr');

        // Date cell
        const dateCell = document.createElement('td');
        dateCell.style.whiteSpace = 'nowrap';
        dateCell.style.verticalAlign = 'top';
        dateCell.style.paddingRight = '12px';
        const d = new Date(change.createdAt);
        dateCell.textContent = d.toLocaleString();
        tr.appendChild(dateCell);

        // Status cell
        const statusCell = document.createElement('td');
        statusCell.style.verticalAlign = 'top';
        statusCell.style.paddingRight = '12px';
        if (change.isCurrent) {
            const badge = document.createElement('span');
            badge.className = 'badge current-badge';
            badge.textContent = 'Current';
            statusCell.appendChild(badge);
        }
        tr.appendChild(statusCell);

        // Changed fields cell
        const fieldsCell = document.createElement('td');
        fieldsCell.style.verticalAlign = 'top';

        const fields = change.changedFields || [];
        const isFirst = changes.indexOf(change) === 0;
        if (fields.length === 0) {
            const note = document.createElement('span');
            note.style.color = '#888';
            note.textContent = isFirst ? 'Initial snapshot' : 'No config changes';
            fieldsCell.appendChild(note);
        } else {
            const dl = document.createElement('dl');
            for (const f of fields) {
                const dt = document.createElement('dt');
                dt.textContent = CONFIG_FIELD_LABELS[f.field] || f.field;
                dl.appendChild(dt);

                const dd = document.createElement('dd');
                const toFmt = formatConfigFieldValue(f.to, f.field);
                if (f.from !== undefined && f.from !== null) {
                    const fromFmt = formatConfigFieldValue(f.from, f.field);
                    dd.textContent = `${fromFmt} → ${toFmt}`;
                } else {
                    dd.textContent = toFmt;
                }
                dl.appendChild(dd);
            }
            fieldsCell.appendChild(dl);
        }
        tr.appendChild(fieldsCell);

        tbody.appendChild(tr);
    }

    table.appendChild(tbody);
    contentEl.appendChild(table);
}
