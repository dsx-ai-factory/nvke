// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clients

import (
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

// ProviderClient composes the settled per-provider capability clients for a
// cluster, the way client-go's Clientset composes per-resource clients. A
// caller that needs more than one capability for the same cluster resolves
// the provider once via GetProviderClient instead of once per capability;
// callers that need only one capability can keep calling that capability's
// own factory (GetClusterScaffold, GetMachineMaterialization, ...) directly
// — both are equally correct.
type ProviderClient interface {
	ClusterScaffold() ClusterScaffold
	MachineMaterialization() MachineMaterialization
	NodePoolUpgradeConfig() NodePoolUpgradeConfig
	KubeVIPConfig() KubeVIPConfig
}

type providerClient struct {
	scaffold    ClusterScaffold
	machine     MachineMaterialization
	nodePoolCfg NodePoolUpgradeConfig
	kubeVIP     KubeVIPConfig
}

func (p *providerClient) ClusterScaffold() ClusterScaffold               { return p.scaffold }
func (p *providerClient) MachineMaterialization() MachineMaterialization { return p.machine }
func (p *providerClient) NodePoolUpgradeConfig() NodePoolUpgradeConfig   { return p.nodePoolCfg }
func (p *providerClient) KubeVIPConfig() KubeVIPConfig                   { return p.kubeVIP }

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
	return &providerClient{
		scaffold:    scaffold,
		machine:     machine,
		nodePoolCfg: nodePoolCfg,
		kubeVIP:     kubeVIP,
	}, nil
}
