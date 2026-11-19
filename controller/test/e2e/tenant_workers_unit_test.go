// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"flag"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/infra"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/infra/dsx"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/infra/forge"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/nvke"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/site"
)

// forgePoolsWithoutMonitoring is a three worker Forge tenant's node pools, its cluster name replaced by CLUSTER.
const forgePoolsWithoutMonitoring = `[{"name":"worker","systemReservedMemory":"16Gi","desiredNodeCount":3,"infra":{"machineTemplate":{"apiVersion":"infrastructure.cluster.x-k8s.io/v1alpha1","kind":"NicoMachineTemplate","metadata":{"name":"CLUSTER-worker-template"},"spec":{"template":{"spec":{"cloudInitInjectHostname":true,"instanceTypeID":"55555555-5555-5555-5555-555555555555","interfaces":[{"subnetID":"33333333-3333-3333-3333-333333333333"}],"ipxeScript":"#!ipxe","sshKeyGroupIDs":["44444444-4444-4444-4444-444444444444"],"vpcID":"22222222-2222-2222-2222-222222222222"}}}},"preKubeadmCommands":["true"]},"nodeNamePrefix":"w"}]`

// capacityRecorder keeps the counts requireRunCapacity asks the provider for.
type capacityRecorder struct {
	infra.Provider
	cpu, workers int
}

func (c *capacityRecorder) RequireCapacity(_ context.Context, _ map[string]string, cpu, workers int) error {
	c.cpu, c.workers = cpu, workers
	return nil
}

func TestTenantWorkers(t *testing.T) {
	t.Log("Testing: -e2e.tenant-workers defaults to the one worker the lanes always created")
	require.Equal(t, "1", flag.Lookup("e2e.tenant-workers").DefValue)

	for _, workers := range []int{1, 3} {
		t.Run(strconv.Itoa(workers), func(t *testing.T) {
			t.Log("Testing: the tenant spec carries one worker pool of that many nodes")
			pools, err := tenantWorkerPools(workers, 0)
			require.NoError(t, err)
			require.Len(t, pools, 1)
			require.Equal(t, workers, pools[0].DesiredNodeCount)

			t.Log("Testing: the capacity check asks for the same workers beside the two CPU machines")
			var checker capacityRecorder
			require.NoError(t, requireRunCapacity(t.Context(), &checker, siteStageFull, workers, 0))
			require.Equal(t, 2, checker.cpu)
			require.Equal(t, workers, checker.workers)
		})
	}

	for _, workers := range []int{0, -1} {
		t.Run(strconv.Itoa(workers), func(t *testing.T) {
			t.Log("Testing: a tenant of fewer than one worker is refused before the provider is asked")
			_, err := tenantWorkerPools(workers, 0)
			require.ErrorContains(t, err, "-e2e.tenant-workers")
			var checker capacityRecorder
			require.ErrorContains(t, requireRunCapacity(t.Context(), &checker, siteStageFull, workers, 0), "-e2e.tenant-workers")
			require.Zero(t, checker.cpu, "the provider should not have been asked for capacity")

			t.Log("Testing: a deployment run builds no tenant, so the worker count does not stop it")
			require.NoError(t, requireRunCapacity(t.Context(), &checker, siteStageDeployment, workers, 0))
		})
	}
}

func TestTenantMonitoringWorkers(t *testing.T) {
	t.Log("Testing: -e2e.monitoring-workers defaults to no monitoring node")
	require.Equal(t, "0", flag.Lookup("e2e.monitoring-workers").DefValue)

	forgeSite := forge.Site{
		ID:            "11111111-1111-1111-1111-111111111111",
		VpcID:         "22222222-2222-2222-2222-222222222222",
		SubnetID:      "33333333-3333-3333-3333-333333333333",
		SSHKeyGroupID: "44444444-4444-4444-4444-444444444444",
	}
	cpu := forge.InstanceType{Name: "cpu", ID: "55555555-5555-5555-5555-555555555555"}
	forgePools := func(t *testing.T, pools []nvke.NodePoolSpec) (string, []api.NodePoolCreateConfig) {
		t.Helper()
		payloads := nvke.NodePayloads{IPXEScript: "#!ipxe", PreKubeadmCommands: []string{"true"}}
		req, err := nvke.BuildCreateRequest(nvke.ClusterSpec{
			Name:         "forge-test",
			Backend:      nvke.BackendNicoForge,
			SiteID:       forgeSite.ID,
			VpcID:        forgeSite.VpcID,
			NodePayloads: &nvke.RolePayloads{ControlPlane: payloads, Worker: payloads},
			ControlPlane: nvke.ControlPlane{Config: forgeControlPlaneConfig(&nvke.ControlPlaneConfigSpec{DesiredCount: 1}, forgeSite, cpu)},
			Workers:      forgeNodePoolSpecs(pools, "worker", "w", forgeSite, cpu),
		})
		require.NoError(t, err)
		b, err := json.Marshal(*req.NodePools)
		require.NoError(t, err)
		return strings.ReplaceAll(string(b), req.Name, "CLUSTER"), *req.NodePools
	}

	t.Run("0", func(t *testing.T) {
		t.Log("Doing: building the pools of a three worker tenant with no monitoring node")
		pools, err := tenantWorkerPools(3, 0)
		require.NoError(t, err)

		t.Log("Testing: one worker pool with no labels or taints, as before")
		require.Equal(t, []nvke.NodePoolSpec{{DesiredNodeCount: 3}}, pools)

		t.Log("Testing: the Forge create request sends the node pools it always sent")
		got, _ := forgePools(t, pools)
		require.JSONEq(t, forgePoolsWithoutMonitoring, got)

		t.Log("Testing: the capacity check asks for the site node, the control plane and the three workers")
		var checker capacityRecorder
		require.NoError(t, requireRunCapacity(t.Context(), &checker, siteStageFull, 3, 0))
		require.Equal(t, 2, checker.cpu)
		require.Equal(t, 3, checker.workers)

		t.Log("Testing: the site allowlists no label key")
		require.Nil(t, tenantRoleLabelKeys(0))
	})

	t.Run("1", func(t *testing.T) {
		t.Log("Doing: building the pools of a three worker tenant with one monitoring node")
		pools, err := tenantWorkerPools(3, 1)
		require.NoError(t, err)

		t.Log("Testing: the workers are labeled workers, and a second pool is labeled and tainted for monitoring")
		require.Equal(t, []nvke.NodePoolSpec{
			{DesiredNodeCount: 3, NodeLabels: map[string]string{"node-role.kubernetes.io/worker": ""}},
			{
				Name:             "monitoring",
				NodeNamePrefix:   "m",
				DesiredNodeCount: 1,
				NodeLabels:       map[string]string{"node-role.kubernetes.io/monitoring": ""},
				NodeTaints:       []api.NodeTaint{{Key: "monitoring", Value: "true", Effect: api.NodeTaintEffectNoSchedule}},
			},
		}, pools)

		t.Log("Testing: the Forge create request carries both pools with their labels and the taint")
		_, sent := forgePools(t, pools)
		require.Len(t, sent, 2)
		require.Equal(t, "worker", sent[0].Name)
		require.Equal(t, "w", sent[0].NodeNamePrefix)
		require.Equal(t, 3, sent[0].DesiredNodeCount)
		require.Equal(t, map[string]string{"node-role.kubernetes.io/worker": ""}, sent[0].NodeLabels)
		require.Empty(t, sent[0].NodeTaints)
		require.Equal(t, "monitoring", sent[1].Name)
		require.Equal(t, "m", sent[1].NodeNamePrefix)
		require.Equal(t, 1, sent[1].DesiredNodeCount)
		require.Equal(t, map[string]string{"node-role.kubernetes.io/monitoring": ""}, sent[1].NodeLabels)
		require.Equal(t, []api.NodeTaint{{Key: "monitoring", Value: "true", Effect: api.NodeTaintEffectNoSchedule}}, sent[1].NodeTaints)

		t.Log("Testing: the DSX pool specs carry the same labels and taint, and only the worker pool takes the default name")
		dsxPools := dsxNodePoolSpecs(pools, "tray-worker", "t", dsx.Site{SSHKeyGroupID: "k", VpcPrefixID: "p"}, dsx.InstanceType{Name: "tray", ID: "tray-type"})
		require.Len(t, dsxPools, 2)
		require.Equal(t, "tray-worker", dsxPools[0].Name)
		require.Equal(t, "monitoring", dsxPools[1].Name)
		require.Equal(t, map[string]string{"node-role.kubernetes.io/worker": ""}, dsxPools[0].NodeLabels)
		require.Empty(t, dsxPools[0].NodeTaints)
		require.Equal(t, map[string]string{"node-role.kubernetes.io/monitoring": ""}, dsxPools[1].NodeLabels)
		require.Equal(t, []api.NodeTaint{{Key: "monitoring", Value: "true", Effect: api.NodeTaintEffectNoSchedule}}, dsxPools[1].NodeTaints)

		t.Log("Testing: the capacity check counts the monitoring node beside the three workers")
		var checker capacityRecorder
		require.NoError(t, requireRunCapacity(t.Context(), &checker, siteStageFull, 3, 1))
		require.Equal(t, 2, checker.cpu)
		require.Equal(t, 4, checker.workers)

		t.Log("Testing: the site allowlists the two node-role label keys the pools carry")
		require.Equal(t, []string{"node-role.kubernetes.io/worker", "node-role.kubernetes.io/monitoring"}, tenantRoleLabelKeys(1))
	})

	t.Run("-1", func(t *testing.T) {
		t.Log("Testing: a negative monitoring count is refused before the provider is asked")
		_, err := tenantWorkerPools(3, -1)
		require.ErrorContains(t, err, "-e2e.monitoring-workers")
		var checker capacityRecorder
		require.ErrorContains(t, requireRunCapacity(t.Context(), &checker, siteStageFull, 3, -1), "-e2e.monitoring-workers")
		require.Zero(t, checker.cpu, "the provider should not have been asked for capacity")
	})
}

func TestAllowTenantNodeLabels(t *testing.T) {
	saved := monitoringWorkers
	t.Cleanup(func() { monitoringWorkers = saved })

	t.Log("Doing: building the stack of a run with no monitoring node")
	monitoringWorkers = 0
	stack := allowTenantNodeLabels(site.Stack{Arch: "x86_64"})

	t.Log("Testing: the stack keeps the chart's allowlist and its other fields")
	require.Nil(t, stack.NodeMetadataAllowlistedLabelKeys)
	require.Equal(t, "x86_64", stack.Arch)

	t.Log("Doing: building the stack of a run with one monitoring node, as -e2e.monitoring-workers=1 sets")
	monitoringWorkers = 1
	stack = allowTenantNodeLabels(site.Stack{Arch: "x86_64"})

	t.Log("Testing: the stack allowlists exactly the two node-role keys")
	require.Equal(t, []string{"node-role.kubernetes.io/worker", "node-role.kubernetes.io/monitoring"}, stack.NodeMetadataAllowlistedLabelKeys)
	require.Equal(t, "x86_64", stack.Arch)
}
