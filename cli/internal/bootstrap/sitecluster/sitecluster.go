// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package sitecluster turns a bootstrap site spec into the controller request that creates
// the cluster from it.
//
// It is the whole of the translation between the two vocabularies. The
// site spec describes hosts an operator already owns; the controller API describes a
// Cluster API cluster. Keeping the mapping in one function, away from the
// command that runs it, means it can be read and tested as what it is: a
// statement of how a site cluster is expressed to the controller.
package sitecluster

import (
	"fmt"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/dsx-ai-factory/nke/cli/internal/bootstrap/sitespec"
	api "github.com/dsx-ai-factory/nke/controller/api"
)

const (
	// provisionedAPIVersion is the API version of the infrastructure provider
	// for hosts that already exist (cluster-api-provider-provisioned).
	provisionedAPIVersion = "infrastructure.cluster.x-k8s.io/v1alpha1"

	provisionedClusterKind         = "ProvisionedCluster"
	provisionedMachineTemplateKind = "ProvisionedMachineTemplate"
)

// ClusterRequest builds the controller CreateCluster request that turns inv's
// pre-provisioned hosts into a Kubernetes cluster.
//
// The shape of the request is what makes it a site cluster:
//
//   - the request carries an infra payload and no nico payload. That is how the controller
//     selects CAPI mode over Forge mode.
//   - infra.cluster is a ProvisionedCluster whose site spec is the hosts
//     from the file. The provider hands one host to each machine; it creates
//     nothing.
//   - the control plane is a cluster-level controlPlaneConfig, one node per
//     host.
//   - controlPlaneConfig.infra.machineTemplate is a ProvisionedMachineTemplate
//     with an empty spec. Everything a machine needs is already in the
//     ProvisionedCluster's site spec, so there is nothing to say per machine.
func ClusterRequest(inv *sitespec.SiteSpec) (api.CreateClusterJSONRequestBody, error) {
	if inv == nil {
		return api.CreateClusterJSONRequestBody{}, fmt.Errorf("sitecluster: site spec is nil")
	}

	if inv.IsNico() {
		return nicoClusterRequest(inv)
	}

	hosts, err := provisionedHosts(inv)
	if err != nil {
		return api.CreateClusterJSONRequestBody{}, err
	}

	req := api.CreateClusterJSONRequestBody{
		Name:                          inv.ClusterName,
		ControlPlaneKubernetesVersion: inv.Cluster.K8sVersion,
		CniPlugin:                     api.CreateClusterRequestCniPlugin(inv.Cluster.CniName),
		EtcdEncrypted:                 inv.Cluster.EtcdEncryptionEnabled(),
		// A plain string with omitempty: an unset VIP is left out of the body.
		ControlPlaneVIP: inv.Cluster.ControlPlaneVIP,
		ControlPlaneConfig: &api.CreateClusterControlPlaneConfig{
			DesiredCount:         len(hosts),
			SystemReservedMemory: inv.Cluster.KubeletConfig.SystemReservedMemory,
			Infra: &api.InfraControlPlaneProvider{
				MachineTemplate: provisionedMachineTemplate(),
			},
		},
		Infra: &api.InfraClusterProvider{
			Cluster: provisionedCluster(inv.ClusterName, hosts),
		},
	}

	applySharedSettings(&req, inv)
	return req, nil
}

// nicoClusterRequest builds the CreateCluster request for a site cluster whose
// machines CAPNICo allocates from a NICo site.
//
// It differs from the provisioned request only in where the machines come
// from: the request carries a nico payload and no infra payload, which is how
// CWE selects Forge mode, and the control-plane count is the spec's nodeCount
// instead of the length of a list.
func nicoClusterRequest(inv *sitespec.SiteSpec) (api.CreateClusterJSONRequestBody, error) {
	n := inv.Cluster.Nico
	if n == nil {
		return api.CreateClusterJSONRequestBody{}, fmt.Errorf("sitecluster: site spec selects infrastructure %q but has no %s block",
			sitespec.InfrastructureNico, "cluster.nico")
	}

	siteID, err := parseUUID("cluster.nico.siteID", n.SiteID)
	if err != nil {
		return api.CreateClusterJSONRequestBody{}, err
	}
	vpcID, err := parseUUID("cluster.nico.vpcID", n.VpcID)
	if err != nil {
		return api.CreateClusterJSONRequestBody{}, err
	}
	instanceTypes, err := parseUUIDs("cluster.nico.instanceTypePreference", n.InstanceTypePreference)
	if err != nil {
		return api.CreateClusterJSONRequestBody{}, err
	}
	vpcPrefixes, err := parseUUIDs("cluster.nico.vpcPrefixIDs", n.VpcPrefixIDs)
	if err != nil {
		return api.CreateClusterJSONRequestBody{}, err
	}
	subnets, err := parseUUIDs("cluster.nico.subnetIDs", n.SubnetIDs)
	if err != nil {
		return api.CreateClusterJSONRequestBody{}, err
	}
	sshKeyGroups, err := parseUUIDs("cluster.nico.sshKeyGroupIDs", n.SSHKeyGroupIDs)
	if err != nil {
		return api.CreateClusterJSONRequestBody{}, err
	}

	req := api.CreateClusterJSONRequestBody{
		Name:                          inv.ClusterName,
		ControlPlaneKubernetesVersion: inv.Cluster.K8sVersion,
		CniPlugin:                     api.CreateClusterRequestCniPlugin(inv.Cluster.CniName),
		EtcdEncrypted:                 inv.Cluster.EtcdEncryptionEnabled(),
		ControlPlaneVIP:               inv.Cluster.ControlPlaneVIP,
		Nico: &api.NicoClusterProvider{
			OrgID:  n.OrgID,
			SiteID: siteID,
			VpcID:  vpcID,
		},
		ControlPlaneConfig: &api.CreateClusterControlPlaneConfig{
			DesiredCount:         n.NodeCount,
			SystemReservedMemory: inv.Cluster.KubeletConfig.SystemReservedMemory,
			Nico: &api.NicoControlPlaneProviderCreate{
				ConfigProfile:          n.ConfigProfile,
				InstanceTypePreference: instanceTypes,
				OSProfile:              n.OSProfile,
				SSHKeyGroupIDs:         sshKeyGroups,
				SubnetIDs:              subnets,
				VpcPrefixIDs:           vpcPrefixes,
			},
		},
	}
	applySharedSettings(&req, inv)
	return req, nil
}

// applySharedSettings sets the request fields that mean the same thing whichever
// infrastructure supplies the machines.
func applySharedSettings(req *api.CreateClusterJSONRequestBody, inv *sitespec.SiteSpec) {
	if inv.Cluster.CniVersion != "" {
		req.CniVersion = &inv.Cluster.CniVersion
	}
	if network := clusterNetwork(inv); network != nil {
		req.Network = network
	}
}

func parseUUID(path, value string) (openapi_types.UUID, error) {
	var id openapi_types.UUID
	if err := id.UnmarshalText([]byte(value)); err != nil {
		return id, fmt.Errorf("sitecluster: %s %q is not a UUID: %w", path, value, err)
	}
	return id, nil
}

func parseUUIDs(path string, values []string) ([]openapi_types.UUID, error) {
	if len(values) == 0 {
		return nil, nil
	}
	ids := make([]openapi_types.UUID, 0, len(values))
	for i, value := range values {
		id, err := parseUUID(fmt.Sprintf("%s[%d]", path, i), value)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// provisionedHosts renders the site spec's nodes as ProvisionedCluster hosts.
//
// A duplicate hostname or IP is rejected here rather than left to the provider.
// The provider keys an allocation on the host's IP address, so two entries
// sharing one would be two machines pointed at one running node — which it
// cannot detect and this can.
func provisionedHosts(inv *sitespec.SiteSpec) ([]map[string]interface{}, error) {
	nodes := inv.Cluster.Nodes
	if len(nodes) == 0 {
		return nil, fmt.Errorf("sitecluster: site spec must contain at least one node")
	}
	hosts := make([]map[string]interface{}, 0, len(nodes))
	seenName := make(map[string]bool, len(nodes))
	seenIP := make(map[string]bool, len(nodes))

	for _, node := range nodes {
		if seenName[node.Name] {
			return nil, fmt.Errorf("sitecluster: node name %q appears more than once in the site spec", node.Name)
		}
		if seenIP[node.IP] {
			return nil, fmt.Errorf("sitecluster: node IP %q appears more than once in the site spec", node.IP)
		}
		seenName[node.Name], seenIP[node.IP] = true, true
		host := map[string]interface{}{
			"hostname":  node.Name,
			"ipAddress": node.IP,
		}
		if node.TPMEKThumbprint != "" {
			host["tpmEkPubHash"] = node.TPMEKThumbprint
		}
		hosts = append(hosts, host)
	}
	return hosts, nil
}

// provisionedCluster builds the infrastructure resource from the site hosts.
// The controller receives the VIP separately in the create request.
func provisionedCluster(name string, hosts []map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": provisionedAPIVersion,
		"kind":       provisionedClusterKind,
		"metadata": map[string]interface{}{
			"name": name,
		},
		"spec": map[string]interface{}{
			"hosts": hosts,
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

// clusterNetwork passes the site spec's CIDRs through, or nil when it named
// neither and the controller's own defaults apply.
//
// Site-spec validation permits only OVN-Kubernetes. The controller still owns
// the CIDR rules; restating them here would be a second copy to keep in step.
func clusterNetwork(inv *sitespec.SiteSpec) *api.ClusterNetworkCreateConfig {
	if inv.Cluster.PodCidr == "" && inv.Cluster.ServiceCidr == "" {
		return nil
	}
	return &api.ClusterNetworkCreateConfig{
		PodCIDR:     inv.Cluster.PodCidr,
		ServiceCIDR: inv.Cluster.ServiceCidr,
	}
}
