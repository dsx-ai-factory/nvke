// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nke

import (
	"fmt"

	"github.com/google/uuid"

	"github.com/NVIDIA/nke/controller/api"
)

// PreprovisionedHost is one host of the inventory that a preprovisioned cluster
// is built on. The host already exists: it is on, it runs the node agent, and
// the network has already given it this address.
type PreprovisionedHost struct {
	// Name identifies the host inside the inventory. It is not the Kubernetes
	// node name; CWE generates that.
	Name string

	// IP is the address the network already assigned.
	IP string

	// Role restricts the host to control-plane or worker machines. Empty allows
	// any machine to take it.
	Role string
}

// buildPreprovisionedRequest builds a generic-CAPI create request whose
// infrastructure manifests are consumed by the preprovisioned provider.
//
// Everything the provider needs travels in capiInfraCluster: the whole point of
// this path is that the inventory rides inside the manifest the API already
// accepts, so no request field describes a host. The machine template is
// therefore empty — there is nothing to create, so there is nothing to shape.
func buildPreprovisionedRequest(spec ClusterSpec) (api.CreateClusterRequest, error) {
	if len(spec.PreprovisionedHosts) == 0 {
		return api.CreateClusterRequest{}, fmt.Errorf("at least one preprovisioned host is required")
	}

	name := timestampedName(spec.Name)

	hosts := make([]map[string]any, 0, len(spec.PreprovisionedHosts))
	for _, host := range spec.PreprovisionedHosts {
		if host.Name == "" || host.IP == "" {
			return api.CreateClusterRequest{}, fmt.Errorf(
				"preprovisioned host needs a name and an address, got name=%q ip=%q", host.Name, host.IP)
		}
		entry := map[string]any{
			"name": host.Name,
			"addresses": []map[string]any{
				{"type": "InternalIP", "address": host.IP},
			},
		}
		if host.Role != "" {
			entry["role"] = host.Role
		}
		hosts = append(hosts, entry)
	}

	req := api.CreateClusterRequest{
		Name:                          name,
		SiteID:                        uuid.Nil,
		ControlPlaneKubernetesVersion: spec.KubernetesVersion,
		CniPlugin:                     spec.CNIPlugin,
		EtcdEncrypted:                 spec.ControlPlane.EtcdEncrypted,
		// Empty means no kube-vip: CWE skips configuring it and the API server
		// is reached on the node address instead.
		ControlPlaneVIP: spec.ControlPlane.ControlPlaneVIP,
		CapiInfraCluster: map[string]any{
			"apiVersion": "infrastructure.cluster.x-k8s.io/v1alpha1",
			"kind":       "PreprovisionedCluster",
			"metadata": map[string]any{
				"name": name,
			},
			"spec": map[string]any{
				"hosts": hosts,
			},
		},
	}
	applyNetworkConfig(&req, spec.Network)

	for _, ps := range orderedPools(spec) {
		pool := buildPreprovisionedNodePool(ps)
		if req.NodePools == nil {
			req.NodePools = &[]api.NodePoolCreateConfig{}
		}
		*req.NodePools = append(*req.NodePools, pool)
	}

	return req, nil
}

// buildPreprovisionedNodePool builds a pool whose machine template is empty.
//
// capiSpireAttestationMethod is "none" because these hosts booted long before
// CWE learned they existed: no cloud-init ran, so nothing consumed a bootstrap
// secret and no SPIRE material reached the agent. "none" makes CWE write none
// and dial the node agent unauthenticated.
func buildPreprovisionedNodePool(spec NodePoolSpec) api.NodePoolCreateConfig {
	pool := api.NodePoolCreateConfig{
		Name:                       spec.Name,
		NodeNamePrefix:             spec.NodeNamePrefix,
		ControlPlane:               spec.ControlPlane,
		DesiredNodeCount:           spec.DesiredNodeCount,
		CapiSpireAttestationMethod: api.SpireAttestationMethodCreateNone,
		CapiInfraMachineTemplate: map[string]any{
			"apiVersion": "infrastructure.cluster.x-k8s.io/v1alpha1",
			"kind":       "PreprovisionedMachineTemplate",
			"metadata": map[string]any{
				"name": fmt.Sprintf("%s-template", spec.Name),
			},
			"spec": map[string]any{
				"template": map[string]any{
					"spec": map[string]any{},
				},
			},
		},
	}
	if spec.SystemReservedMemory != "" {
		pool.SystemReservedMemory = spec.SystemReservedMemory
	}
	if spec.KubeReservedMemory != "" {
		pool.KubeReservedMemory = spec.KubeReservedMemory
	}
	return pool
}
