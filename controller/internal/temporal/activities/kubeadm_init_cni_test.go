// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent/agenttest"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

type inInitCNIFields struct {
	pluginName    string
	ovnVersion    string
	tigeraVersion string
	useOvnK8s     bool
}

func TestKubeadmInitCNIInstall(t *testing.T) {
	const registry = "registry.example.com/cni"

	tests := []struct {
		name                string
		plugin              db.CniPlugin
		installCNIAfterInit bool
		cniImageRegistry    string
		wantNoCNIInstall    bool
		wantInInit          inInitCNIFields
		wantSkipKubeProxy   bool
		wantRegistry        bool
	}{
		{
			name:              "ovn-kubernetes installs in init and skips kube-proxy",
			plugin:            db.CNIOvnKubernetes,
			wantInInit:        inInitCNIFields{pluginName: "ovn", ovnVersion: db.DefaultOvnKubernetesCNIVersion, useOvnK8s: true},
			wantSkipKubeProxy: true,
		},
		{
			name:                "ovn-kubernetes installed afterwards still skips kube-proxy",
			plugin:              db.CNIOvnKubernetes,
			installCNIAfterInit: true,
			wantNoCNIInstall:    true,
			wantInInit:          inInitCNIFields{pluginName: "ovn", ovnVersion: db.DefaultOvnKubernetesCNIVersion, useOvnK8s: true},
			wantSkipKubeProxy:   true,
		},
		{
			name:       "calico keeps kube-proxy",
			plugin:     db.CNICalico,
			wantInInit: inInitCNIFields{pluginName: "calico", tigeraVersion: db.DefaultCalicoCNIVersion},
		},
		{
			name:                "calico installed afterwards keeps kube-proxy",
			plugin:              db.CNICalico,
			installCNIAfterInit: true,
			wantNoCNIInstall:    true,
			wantInInit:          inInitCNIFields{pluginName: "calico", tigeraVersion: db.DefaultCalicoCNIVersion},
		},
		{
			name:              "ovn-kubernetes passes the CNI image registry to the agent",
			plugin:            db.CNIOvnKubernetes,
			cniImageRegistry:  registry,
			wantInInit:        inInitCNIFields{pluginName: "ovn", ovnVersion: db.DefaultOvnKubernetesCNIVersion, useOvnK8s: true},
			wantSkipKubeProxy: true,
			wantRegistry:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster, initialNode, nodeSet, nodePool := snapshotInitRecords()
			cluster.CniPlugin = tt.plugin
			database := snapshotInitDatabase(cluster, initialNode, nodeSet, nodePool)
			agent := &snapshotWiringAgent{FakeClient: &agenttest.FakeClient{}, initStatus: true}
			cfg := snapshotSharedConfig()
			cfg.CNIImageRegistry = tt.cniImageRegistry
			a := snapshotActivities(database, agent, cfg)

			input := KubeadmInitInput{ClusterID: cluster.ID, InitialNodeID: initialNode.ID, InstallCNIAfterInit: tt.installCNIAfterInit}
			require.NoError(t, executeSnapshotInit(t, a, input))

			req := requireSingleInitRequest(t, agent)
			assert.Equal(t, tt.wantNoCNIInstall, req.GetNoCniInstall())
			assert.Equal(t, tt.wantSkipKubeProxy, strings.Contains(req.GetInitConfig(), "addon/kube-proxy"))

			gotInInit := inInitCNIFields{req.GetCniPluginName(), req.GetOvnVersion(), req.GetTigeraVersion(), req.GetUseOvnK8SCni()} //nolint:staticcheck // Deprecated fields that the in-init CNI install still reads.
			assert.Equal(t, tt.wantInInit, gotInInit)

			cniConfig := req.GetCniConfig() //nolint:staticcheck // Deprecated field that the in-init CNI install still reads.
			if tt.wantRegistry {
				assert.Equal(t, map[string]string{cniRegistryHelmValue: registry}, cniConfig)
			} else {
				assert.Empty(t, cniConfig)
			}
		})
	}
}

func TestKubeadmInitRefusesAnUnsupportedCNIPlugin(t *testing.T) {
	cluster, initialNode, nodeSet, nodePool := snapshotInitRecords()
	cluster.CniPlugin = db.CniPlugin("flannel")
	database := snapshotInitDatabase(cluster, initialNode, nodeSet, nodePool)
	agent := &snapshotWiringAgent{FakeClient: &agenttest.FakeClient{}, initStatus: true}
	a := snapshotActivities(database, agent, snapshotSharedConfig())

	err := executeSnapshotInit(t, a, KubeadmInitInput{ClusterID: cluster.ID, InitialNodeID: initialNode.ID})

	require.ErrorContains(t, err, "unsupported CNI plugin: flannel")
	assert.Empty(t, agent.initRequests, "nothing is sent to the agent for a plugin that cannot be installed")
}
