// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clients

import (
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// ProviderClient composes every settled per-provider capability client for a
// cluster, the way client-go's Clientset composes per-resource clients.
// Resolve it once, as early in a request flow as the cluster is known, and
// pass the result through to whatever needs a capability from it — not a
// fresh GetXxx call at each spot that needs one.
type ProviderClient interface {
	ClusterScaffold() ClusterScaffold
	MachineMaterialization() MachineMaterialization
	NodePoolUpgradeConfig() NodePoolUpgradeConfig
	KubeVIPConfig() KubeVIPConfig
	NodeSetProvider() NodeSetProvider
	ControlPlaneConfigNodeProvider() ControlPlaneConfigNodeProvider
	NewNodeProvider() NewNodeProvider
	NodePoolPayloadValidator() NodePoolPayloadValidator
	ControlPlaneConfigPayloadValidator() ControlPlaneConfigPayloadValidator
}

type providerClient struct {
	scaffold              ClusterScaffold
	machine               MachineMaterialization
	nodePoolCfg           NodePoolUpgradeConfig
	kubeVIP               KubeVIPConfig
	nodeSet               NodeSetProvider
	controlPlaneNode      ControlPlaneConfigNodeProvider
	newNode               NewNodeProvider
	nodePoolValidator     NodePoolPayloadValidator
	controlPlaneValidator ControlPlaneConfigPayloadValidator
}

func (p *providerClient) ClusterScaffold() ClusterScaffold               { return p.scaffold }
func (p *providerClient) MachineMaterialization() MachineMaterialization { return p.machine }
func (p *providerClient) NodePoolUpgradeConfig() NodePoolUpgradeConfig   { return p.nodePoolCfg }
func (p *providerClient) KubeVIPConfig() KubeVIPConfig                   { return p.kubeVIP }
func (p *providerClient) NodeSetProvider() NodeSetProvider               { return p.nodeSet }
func (p *providerClient) ControlPlaneConfigNodeProvider() ControlPlaneConfigNodeProvider {
	return p.controlPlaneNode
}
func (p *providerClient) NewNodeProvider() NewNodeProvider { return p.newNode }
func (p *providerClient) NodePoolPayloadValidator() NodePoolPayloadValidator {
	return p.nodePoolValidator
}

func (p *providerClient) ControlPlaneConfigPayloadValidator() ControlPlaneConfigPayloadValidator {
	return p.controlPlaneValidator
}

// GetProviderClient resolves every settled capability for cluster's
// provider type by calling each capability's own factory. Adding a provider
// needs no change here — it is already covered once each capability's own
// factory has a case for it.
func GetProviderClient(cluster db.Cluster) (ProviderClient, error) {
	scaffold, err := GetClusterScaffold(cluster)
	if err != nil {
		return nil, err
	}
	machine, err := GetMachineMaterialization(cluster)
	if err != nil {
		return nil, err
	}
	nodePoolCfg, err := GetNodePoolUpgradeConfig(cluster)
	if err != nil {
		return nil, err
	}
	kubeVIP, err := GetKubeVIPConfig(cluster)
	if err != nil {
		return nil, err
	}
	nodeSet, err := GetNodeSetProvider(cluster)
	if err != nil {
		return nil, err
	}
	controlPlaneNode, err := GetControlPlaneConfigNodeProvider(cluster)
	if err != nil {
		return nil, err
	}
	newNode, err := GetNewNodeProvider(cluster)
	if err != nil {
		return nil, err
	}
	nodePoolValidator, err := GetNodePoolPayloadValidator(cluster)
	if err != nil {
		return nil, err
	}
	controlPlaneValidator, err := GetControlPlaneConfigPayloadValidator(cluster)
	if err != nil {
		return nil, err
	}
	return &providerClient{
		scaffold:              scaffold,
		machine:               machine,
		nodePoolCfg:           nodePoolCfg,
		kubeVIP:               kubeVIP,
		nodeSet:               nodeSet,
		controlPlaneNode:      controlPlaneNode,
		newNode:               newNode,
		nodePoolValidator:     nodePoolValidator,
		controlPlaneValidator: controlPlaneValidator,
	}, nil
}
