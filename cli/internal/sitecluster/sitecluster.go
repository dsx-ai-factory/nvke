// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package sitecluster turns a bootstrap inventory into the CWE request that creates
// the cluster from it.
//
// It is the whole of the translation between the two vocabularies. The
// inventory describes hosts an operator already owns; CWE's API describes a
// Cluster API cluster. Keeping the mapping in one function, away from the
// command that runs it, means it can be read and tested as what it is: a
// statement of how a site cluster is expressed to CWE.
package sitecluster

import (
	"fmt"

	"github.com/google/uuid"

	"github.com/NVIDIA/nke/cli/internal/inventory"
	api "github.com/NVIDIA/nke/controller/api"
)

const (
	// provisionedAPIVersion is the API version of the infrastructure provider
	// for hosts that already exist (cluster-api-provider-provisioned).
	provisionedAPIVersion = "infrastructure.cluster.x-k8s.io/v1alpha1"

	provisionedClusterKind         = "ProvisionedCluster"
	provisionedMachineTemplateKind = "ProvisionedMachineTemplate"

	// controlPlanePoolName/controlPlaneNodeNamePrefix name the single node pool
	// a site cluster has. Every inventory node joins as a control-plane node,
	// which is what the inventory itself describes (see inventory.Cluster's
	// JoinNodes) — so there is one pool and it is the control plane.
	//
	// The prefix is short because CWE builds node names as
	// <cluster>-<prefix>-<4 digits> and holds the result to 63 characters.
	controlPlanePoolName       = "control-plane"
	controlPlaneNodeNamePrefix = "cp"
)

// ClusterRequest builds the CWE CreateCluster request that turns inv's
// pre-provisioned hosts into a Kubernetes cluster.
//
// The shape of the request is what makes it a site cluster:
//
//   - siteID is the nil UUID. That is how CWE selects CAPI mode over
//     Forge mode, and it is why orgID and vpcID must stay empty.
//   - capiInfraCluster is a ProvisionedCluster whose inventory is the hosts
//     from the file. The provider hands one host to each machine; it creates
//     nothing.
//   - the node pool's capiInfraMachineTemplate is a ProvisionedMachineTemplate
//     with an empty spec. Everything a machine needs is already in the
//     ProvisionedCluster's inventory, so there is nothing to say per machine.
//
// The init node is first in the host list because kubeadm init runs on the
// first control-plane node CWE provisions, and the inventory names which host
// that is.
func ClusterRequest(inv *inventory.Inventory) (api.CreateClusterJSONRequestBody, error) {
	if inv == nil {
		return api.CreateClusterJSONRequestBody{}, fmt.Errorf("sitecluster: inventory is nil")
	}

	hosts, err := provisionedHosts(inv)
	if err != nil {
		return api.CreateClusterJSONRequestBody{}, err
	}

	req := api.CreateClusterJSONRequestBody{
		Name: inv.ClusterName,
		// The nil UUID default site: CAPI-backed, not Forge-backed.
		SiteID:                        uuid.Nil,
		ControlPlaneKubernetesVersion: inv.Cluster.K8sVersion,
		CniPlugin:                     inv.Cluster.CniName,
		EtcdEncrypted:                 false,
		NodePools: &[]api.NodePoolCreateConfig{{
			Name:             controlPlanePoolName,
			NodeNamePrefix:   controlPlaneNodeNamePrefix,
			DesiredNodeCount: len(hosts),
			ControlPlane:     true,
			// joinToken, not tpm: CWE only derives a node's TPM EK pubhash from
			// a NicoMachine (see controller/internal/controllers/nodetpmekpub),
			// so tpm attestation on a provisioned machine would write no SPIRE
			// entry and the node would never get an identity. The inventory
			// already carries the thumbprints, so teaching CWE to read them off
			// the ProvisionedCluster is the fix; until then this is the method
			// that works.
			CapiSpireAttestationMethod: api.SpireAttestationMethodCreateJoinToken,
			SystemReservedMemory:       inv.Cluster.KubeletConfig.SystemReservedMemory,
			CapiInfraMachineTemplate:   provisionedMachineTemplate(),
		}},
		CapiInfraCluster: provisionedCluster(inv.ClusterName, inv.Cluster.InitNode.Name, hosts),
	}

	if inv.Cluster.CniVersion != "" {
		req.CniVersion = &inv.Cluster.CniVersion
	}
	if network := clusterNetwork(inv); network != nil {
		req.Network = network
	}
	return req, nil
}

// provisionedHosts renders the inventory's nodes as ProvisionedCluster hosts,
// init node first.
//
// A duplicate hostname or IP is rejected here rather than left to the provider.
// The provider keys an allocation on the host's IP address, so two entries
// sharing one would be two machines pointed at one running node — which it
// cannot detect and this can.
func provisionedHosts(inv *inventory.Inventory) ([]map[string]interface{}, error) {
	nodes := append([]inventory.Node{inv.Cluster.InitNode}, inv.Cluster.JoinNodes...)
	hosts := make([]map[string]interface{}, 0, len(nodes))
	seenName := make(map[string]bool, len(nodes))
	seenIP := make(map[string]bool, len(nodes))

	for _, node := range nodes {
		if seenName[node.Name] {
			return nil, fmt.Errorf("sitecluster: node name %q appears more than once in the inventory", node.Name)
		}
		if seenIP[node.IP] {
			return nil, fmt.Errorf("sitecluster: node IP %q appears more than once in the inventory", node.IP)
		}
		seenName[node.Name], seenIP[node.IP] = true, true
		hosts = append(hosts, map[string]interface{}{
			"hostname":  node.Name,
			"ipAddress": node.IP,
		})
	}
	return hosts, nil
}

// provisionedCluster is the infrastructure side of the CAPI Cluster: the
// inventory and the host to allocate first, and nothing else. The provider owns
// no endpoint — CWE owns the control-plane VIP through its own API.
//
// initNode names the host kubeadm init runs on. The provider reserves it for
// the first eligible control-plane machine and blocks every other allocation
// until that binding holds, so a spec without it is rejected by the API server.
func provisionedCluster(name, initNode string, hosts []map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": provisionedAPIVersion,
		"kind":       provisionedClusterKind,
		"metadata": map[string]interface{}{
			"name": name,
		},
		"spec": map[string]interface{}{
			"initNode": initNode,
			"hosts":    hosts,
		},
	}
}

// provisionedMachineTemplate is deliberately empty below the required
// structure. A machine of this provider is a claim on a host, so there is
// nothing to configure per machine.
func provisionedMachineTemplate() map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": provisionedAPIVersion,
		"kind":       provisionedMachineTemplateKind,
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{},
			},
		},
	}
}

// clusterNetwork passes the inventory's CIDRs through, or nil when it named
// neither and CWE's own defaults apply.
//
// CWE rejects non-default CIDRs for any CNI plugin other than ovn-kubernetes,
// so an inventory that sets them with calico fails at create time with that
// message. That check belongs to CWE, which owns the rule; restating it here
// would be a second copy to keep in step.
func clusterNetwork(inv *inventory.Inventory) *api.ClusterNetworkCreateConfig {
	if inv.Cluster.PodCidr == "" && inv.Cluster.ServiceCidr == "" {
		return nil
	}
	return &api.ClusterNetworkCreateConfig{
		PodCIDR:     inv.Cluster.PodCidr,
		ServiceCIDR: inv.Cluster.ServiceCidr,
	}
}
