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

func TestBuildNicoVMSNodePoolKubeReservedMemory(t *testing.T) {
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

	pool, err := buildNicoVMSNodePool(spec)
	if err != nil {
		t.Fatalf("building nico-vms node pool: %v", err)
	}
	if pool.KubeReservedMemory != "250Mi" {
		t.Errorf("kube reserved memory = %q, want 250Mi", pool.KubeReservedMemory)
	}
}

func TestCAPDNodePoolRequest(t *testing.T) {
	clusterID := uuid.New()
	req, err := CAPDNodePoolRequest(clusterID, NodePoolSpec{
		Name:                 "worker-extra",
		NodeNamePrefix:       "wx",
		DesiredNodeCount:     1,
		SystemReservedMemory: "2Gi",
		KubeReservedMemory:   "512Mi",
	}, "capd-node:test")
	if err != nil {
		t.Fatalf("building CAPD node pool request: %v", err)
	}
	if req.ClusterID != clusterID {
		t.Errorf("cluster ID = %s, want %s", req.ClusterID, clusterID)
	}
	if req.Name != "worker-extra" || req.NodeNamePrefix != "wx" || req.DesiredNodeCount != 1 {
		t.Errorf("node pool identity/count = (%q, %q, %d), want (worker-extra, wx, 1)", req.Name, req.NodeNamePrefix, req.DesiredNodeCount)
	}
	if req.SystemReservedMemory != "2Gi" || req.KubeReservedMemory != "512Mi" {
		t.Errorf("reserved memory = (%q, %q), want (2Gi, 512Mi)", req.SystemReservedMemory, req.KubeReservedMemory)
	}
	if req.Nico != nil {
		t.Errorf("CAPD request contains nico fields: nico=%#v", req.Nico)
	}
	if req.Infra == nil {
		t.Fatalf("CAPD request has no infra fields")
	}
	if req.Infra.SpireAttestationMethod != api.SpireAttestationMethodCreateJoinToken {
		t.Errorf("SPIRE attestation method = %q, want %q", req.Infra.SpireAttestationMethod, api.SpireAttestationMethodCreateJoinToken)
	}
	assertBootstrapSuccessCommands(t, req.Infra.PreKubeadmCommands)

	spec, ok := req.Infra.MachineTemplate["spec"].(map[string]any)
	if !ok {
		t.Fatalf("CAPI machine template spec = %#v, want an object", req.Infra.MachineTemplate["spec"])
	}
	template, ok := spec["template"].(map[string]any)
	if !ok {
		t.Fatalf("CAPI machine template payload = %#v, want an object", spec["template"])
	}
	machineSpec, ok := template["spec"].(map[string]any)
	if !ok {
		t.Fatalf("CAPI machine template machine spec = %#v, want an object", template["spec"])
	}
	if got := machineSpec["customImage"]; got != "capd-node:test" {
		t.Errorf("custom image = %#v, want capd-node:test", got)
	}
}

func TestUnit_BuildNicoVMSRequestControlPlaneVIP(t *testing.T) {
	type parameters struct {
		spec    ClusterSpec
		wantVIP string
	}

	baseSpec := ClusterSpec{
		Name:              "nico-test",
		Backend:           BackendNicoVMS,
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
				OSProfile:             "nke-nico-vms-24.04-amd64-fde",
				ConfigProfile:         "nico-vms-cp",
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
			req, err := buildNicoVMSRequest(params.spec)
			if err != nil {
				t.Fatalf("buildNicoVMSRequest failed: %v", err)
			}
			if req.ControlPlaneVIP != params.wantVIP {
				t.Fatalf("ControlPlaneVIP = %q, want %q", req.ControlPlaneVIP, params.wantVIP)
			}
		})
	}
}

func TestUnit_BuildNicoVMSRequestControlPlaneConfig(t *testing.T) {
	spec := ClusterSpec{
		Name:              "nico-cp-controller",
		Backend:           BackendNicoVMS,
		KubernetesVersion: "1.34.2",
		CNIPlugin:         "ovn-kubernetes",
		OrgID:             "org-1",
		SiteID:            "a1b2c3d4-0001-4000-8000-000000000001",
		VpcID:             "a1b2c3d4-0002-4000-8000-000000000001",
		ControlPlane: ControlPlane{
			Config: &ControlPlaneConfigSpec{
				DesiredCount:         1,
				InstanceTypeID:       "a1b2c3d4-0005-4000-8000-000000000003",
				OSProfile:            "nke-nico-vms-24.04-amd64-fde",
				ConfigProfile:        "nico-vms-cp",
				VpcPrefixIDs:         []string{"a1b2c3d4-0004-4000-8000-000000000001"},
				SSHKeyGroupIDs:       []string{"a1b2c3d4-0006-4000-8000-000000000001"},
				SystemReservedMemory: "1Gi",
			},
		},
		Workers: []NodePoolSpec{{
			Name:                  "worker",
			NodeNamePrefix:        "w",
			DesiredNodeCount:      1,
			InstanceTypeID:        "a1b2c3d4-0005-4000-8000-000000000003",
			VpcPrefixIDs:          []string{"a1b2c3d4-0004-4000-8000-000000000001"},
			SSHKeyGroupIDs:        []string{"a1b2c3d4-0006-4000-8000-000000000001"},
			OSProfile:             "nke-nico-vms-24.04-amd64-fde",
			ConfigProfile:         "nico-vms-worker",
			DiskSelectionStrategy: "smallest",
		}},
	}

	req, err := buildNicoVMSRequest(spec)
	if err != nil {
		t.Fatalf("buildNicoVMSRequest failed: %v", err)
	}
	if req.ControlPlaneConfig == nil {
		t.Fatal("ControlPlaneConfig is nil")
	}
	if req.ControlPlaneConfig.DesiredCount != 1 {
		t.Fatalf("DesiredCount = %d, want 1", req.ControlPlaneConfig.DesiredCount)
	}
	if req.ControlPlaneConfig.Nico == nil {
		t.Fatal("ControlPlaneConfig.Nico is nil")
	}
	if req.ControlPlaneConfig.Nico.OSProfile != "nke-nico-vms-24.04-amd64-fde" {
		t.Fatalf("Nico.OSProfile = %q", req.ControlPlaneConfig.Nico.OSProfile)
	}
	if len(req.ControlPlaneConfig.Nico.InstanceTypePreference) != 1 {
		t.Fatalf("Nico.InstanceTypePreference len = %d, want 1", len(req.ControlPlaneConfig.Nico.InstanceTypePreference))
	}
	if req.NodePools == nil || len(*req.NodePools) != 1 || (*req.NodePools)[0].ControlPlane {
		t.Fatalf("expected a single worker node pool, got %#v", req.NodePools)
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
	if pools[0].Infra == nil {
		t.Fatal("first Nico node pool infra is nil")
	}
	assertBootstrapSuccessCommands(t, pools[0].Infra.PreKubeadmCommands)
	if pools[1].Name != "worker" || pools[1].ControlPlane {
		t.Errorf("second Nico node pool = %q controlPlane=%t, want worker controlPlane=false", pools[1].Name, pools[1].ControlPlane)
	}
	if pools[1].Infra == nil {
		t.Fatal("second Nico node pool infra is nil")
	}
	assertBootstrapSuccessCommands(t, pools[1].Infra.PreKubeadmCommands)
}

func assertBootstrapSuccessCommands(t *testing.T, got []string) {
	t.Helper()
	want := containerCAPIReadyCommands()
	if len(got) != len(want) {
		t.Fatalf("bootstrap success command count = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("bootstrap success command %d = %q, want %q", i, got[i], want[i])
		}
	}
}
