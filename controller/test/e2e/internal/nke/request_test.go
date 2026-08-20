// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nke

import (
	"testing"

	"github.com/google/uuid"

	"github.com/NVIDIA/nke/controller/api"
)

func TestApplyNetworkConfig(t *testing.T) {
	tests := []struct {
		name string
		spec *ClusterNetworkSpec
		want bool
	}{
		{name: "nil", spec: nil, want: false},
		{name: "empty", spec: &ClusterNetworkSpec{}, want: false},
		{name: "blank", spec: &ClusterNetworkSpec{PodCIDR: " ", ServiceCIDR: "\t"}, want: false},
		{name: "configured", spec: &ClusterNetworkSpec{PodCIDR: "10.250.0.0/16"}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req api.CreateClusterRequest
			applyNetworkConfig(&req, tt.spec)
			if (req.Network != nil) != tt.want {
				t.Fatalf("Network configured = %t, want %t", req.Network != nil, tt.want)
			}
		})
	}
}

func TestBuildForgeNodePoolKubeReservedMemory(t *testing.T) {
	spec := NodePoolSpec{
		Name:               "worker",
		NodeNamePrefix:     "w",
		DesiredNodeCount:   1,
		InstanceTypeID:     uuid.NewString(),
		VpcPrefixIDs:       []string{uuid.NewString()},
		SSHKeyGroupIDs:     []string{uuid.NewString()},
		OSProfile:          "nke-test",
		ConfigProfile:      "test-config",
		KubeReservedMemory: "250Mi",
	}

	pool, err := buildForgeNodePool(spec)
	if err != nil {
		t.Fatalf("building forge node pool: %v", err)
	}
	if pool.KubeReservedMemory != "250Mi" {
		t.Errorf("kube reserved memory = %q, want 250Mi", pool.KubeReservedMemory)
	}
}

func TestUnit_BuildForgeRequestControlPlaneVIP(t *testing.T) {
	type parameters struct {
		spec    ClusterSpec
		wantVIP string
	}

	baseSpec := ClusterSpec{
		Name:              "forge-test",
		Backend:           BackendForgeVMS,
		KubernetesVersion: "1.34.8",
		CNIPlugin:         "ovn-kubernetes",
		OrgID:             "org-1",
		SiteID:            "a1b2c3d4-0001-4000-8000-000000000001",
		VpcID:             "a1b2c3d4-0002-4000-8000-000000000001",
		ControlPlane: ControlPlane{
			Pools: []NodePoolSpec{{
				Name:                  "control-plane",
				NodeNamePrefix:        "cp",
				DesiredNodeCount:      1,
				InstanceTypeID:        "a1b2c3d4-0005-4000-8000-000000000001",
				VpcPrefixIDs:          []string{"a1b2c3d4-0004-4000-8000-000000000001"},
				SSHKeyGroupIDs:        []string{"a1b2c3d4-0006-4000-8000-000000000001"},
				OSProfile:             "nke-forge-vms-24.04-amd64-fde",
				ConfigProfile:         "forge-vms-cp",
				DiskSelectionStrategy: "smallest",
			}},
		},
	}

	tests := map[string]parameters{
		"preserves explicit VIP": {
			spec: func() ClusterSpec {
				spec := baseSpec
				spec.ControlPlane.ControlPlaneVIP = "172.31.0.10"
				return spec
			}(),
			wantVIP: "172.31.0.10",
		},
		"preserves empty VIP": {
			spec:    baseSpec,
			wantVIP: "",
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			req, err := buildForgeRequest(params.spec)
			if err != nil {
				t.Fatalf("buildForgeRequest failed: %v", err)
			}
			if req.ControlPlaneVIP != params.wantVIP {
				t.Fatalf("ControlPlaneVIP = %q, want %q", req.ControlPlaneVIP, params.wantVIP)
			}
		})
	}
}

func TestBuildNicoRequestNodePools(t *testing.T) {
	instanceTypeID := uuid.NewString()
	subnetID := uuid.NewString()
	spec := ClusterSpec{
		Name:   "nico-test",
		SiteID: uuid.NewString(),
		VpcID:  uuid.NewString(),
		ControlPlane: ControlPlane{Pools: []NodePoolSpec{{
			Name:             "control-plane",
			InstanceTypeID:   instanceTypeID,
			SubnetIDs:        []string{subnetID},
			DesiredNodeCount: 1,
		}}},
		Workers: []NodePoolSpec{{
			Name:             "worker",
			InstanceTypeID:   instanceTypeID,
			SubnetIDs:        []string{subnetID},
			DesiredNodeCount: 1,
		}},
	}

	req, err := buildNicoRequest(spec)
	if err != nil {
		t.Fatalf("building Nico request: %v", err)
	}
	if req.NodePools == nil {
		t.Fatal("Nico request node pools are nil")
	}
	pools := *req.NodePools
	if len(pools) != 2 {
		t.Fatalf("Nico request node pool count = %d, want 2", len(pools))
	}
	if pools[0].Name != "control-plane" || !pools[0].ControlPlane {
		t.Errorf("first Nico node pool = %q controlPlane=%t, want control-plane controlPlane=true", pools[0].Name, pools[0].ControlPlane)
	}
	assertBootstrapSuccessCommands(t, pools[0].CapiPreKubeadmCommands)
	if pools[1].Name != "worker" || pools[1].ControlPlane {
		t.Errorf("second Nico node pool = %q controlPlane=%t, want worker controlPlane=false", pools[1].Name, pools[1].ControlPlane)
	}
	assertBootstrapSuccessCommands(t, pools[1].CapiPreKubeadmCommands)
}

func assertBootstrapSuccessCommands(t *testing.T, got []string) {
	t.Helper()
	if len(got) != len(containerCAPIReadyCommands) {
		t.Fatalf("bootstrap success command count = %d, want %d", len(got), len(containerCAPIReadyCommands))
	}
	for i := range containerCAPIReadyCommands {
		if got[i] != containerCAPIReadyCommands[i] {
			t.Fatalf("bootstrap success command %d = %q, want %q", i, got[i], containerCAPIReadyCommands[i])
		}
	}
}
