// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Cluster intent resolution combines the spec with selected configuration for reconciliation.
// CWE REST API specific calls will be replaced with the CAPI objects, but this is a temporary placeholder.

package controllers

import (
	"encoding/json"
	"fmt"
	"net/netip"

	"github.com/dsx-ai-factory/nvke/controller/api"
	internalv1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
)

// InvalidSpecError identifies a request that needs corrected configuration.
type InvalidSpecError struct{ Message string }

func (e *InvalidSpecError) Error() string { return e.Message }

// ResolveCWERequest translates supported cluster intent using bindings validated by LoadCWEBindings.
func ResolveCWERequest(cluster *internalv1.NKECluster, bindings []CWEBinding) (api.CreateClusterRequest, error) {
	var empty api.CreateClusterRequest
	if err := validateCWESpec(cluster); err != nil {
		return empty, err
	}
	var selected *CWEBinding
	for i := range bindings {
		binding := &bindings[i]
		if binding.Namespace == cluster.Namespace && binding.Channel == string(cluster.Spec.Release.Channel.Name) && binding.Version == cluster.Spec.Release.Channel.Version && binding.ControlPlaneClass == string(cluster.Spec.ControlPlaneClass) {
			selected = binding
			break
		}
	}
	if selected == nil {
		return empty, invalidCWERequest("The selected release and control-plane class are not available for this cluster")
	}
	// The REST types have no generated DeepCopy. JSON copying isolates nested provider data.
	data, err := json.Marshal(selected.Template)
	if err != nil {
		return empty, fmt.Errorf("encode CWE request template: %w", err)
	}
	var request api.CreateClusterRequest
	if err := json.Unmarshal(data, &request); err != nil {
		return empty, fmt.Errorf("copy CWE request template: %w", err)
	}
	request.Name = clusterRemoteName(cluster)
	if cluster.UID == "" {
		return empty, invalidCWERequest("A persisted cluster UID is required before provisioning")
	}
	request.CniPlugin = api.CreateClusterRequestCniPluginOvnKubernetes
	request.EtcdEncrypted = true
	pod, err := cweNetworkPrefix(cluster.Spec.ClusterNetwork.Pods.CIDRBlocks)
	if err != nil {
		return empty, err
	}
	service, err := cweNetworkPrefix(cluster.Spec.ClusterNetwork.Services.CIDRBlocks)
	if err != nil {
		return empty, err
	}
	request.Network = &api.ClusterNetworkCreateConfig{PodCIDR: pod, ServiceCIDR: service}
	return request, nil
}

func invalidCWERequest(message string) error { return &InvalidSpecError{Message: message} }

func validateCWESpec(cluster *internalv1.NKECluster) error {
	if len(cluster.Spec.NodePools) != 0 {
		return invalidCWERequest("Worker node pools are not supported")
	}
	if cluster.Spec.Network != nil {
		return invalidCWERequest("Worker network selection is not supported")
	}
	if cluster.Spec.Authentication != nil {
		return invalidCWERequest("Custom authentication is not supported")
	}
	if cluster.Spec.APIServerAccess.PublicAccessEnabled || len(cluster.Spec.APIServerAccess.PublicAccessCIDRs) != 0 {
		return invalidCWERequest("Public API access configuration is not supported")
	}
	if runtime := cluster.Spec.AcceleratedRuntime; runtime != nil && (runtime.Name != "none" || runtime.Version != "") {
		return invalidCWERequest("Accelerated runtime configuration is not supported")
	}
	if cluster.Spec.ClusterNetwork.ServiceDomain != "cluster.local" {
		return invalidCWERequest("Only the cluster.local service domain is supported")
	}
	return nil
}

func cweNetworkPrefix(blocks []internalv1.NetworkCIDR) (string, error) {
	if len(blocks) != 1 {
		return "", invalidCWERequest("Exactly one IPv4 pod range and one IPv4 service range are required")
	}
	prefix, err := netip.ParsePrefix(string(blocks[0]))
	if err != nil || !prefix.Addr().Is4() {
		return "", invalidCWERequest("Pod and service ranges must be valid IPv4 CIDRs")
	}
	// CWE remains authoritative for private ranges, overlap, size, and capacity validation.
	return prefix.Masked().String(), nil
}
