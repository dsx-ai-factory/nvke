// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nke

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/api"
)

func TestUnit_CAPDRequestPublishesFailureDomains(t *testing.T) {
	tests := []struct {
		name    string
		domains []CAPDFailureDomain
		want    []map[string]any
	}{
		{
			name:    "control-plane and worker domains use the v1beta2 list",
			domains: []CAPDFailureDomain{{Name: "fd-a", ControlPlane: true}, {Name: "fd-b", ControlPlane: true}, {Name: "fd-worker", ControlPlane: false}},
			want:    []map[string]any{{"name": "fd-a", "controlPlane": true}, {"name": "fd-b", "controlPlane": true}, {"name": "fd-worker", "controlPlane": false}},
		},
		{
			name:    "worker-only domains preserve controlPlane false",
			domains: []CAPDFailureDomain{{Name: "fd-worker", ControlPlane: false}},
			want:    []map[string]any{{"name": "fd-worker", "controlPlane": false}},
		},
		{name: "nil domains omit the key"},
		{name: "empty domains omit the key", domains: []CAPDFailureDomain{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := buildCAPDRequest(ClusterSpec{
				KubernetesVersion:  "1.34.2",
				CAPDNodeImage:      "node-image:local",
				ControlPlane:       ControlPlane{Config: &ControlPlaneConfigSpec{DesiredCount: 1}},
				CAPDFailureDomains: tt.domains,
			})
			require.NoError(t, err)
			spec, ok := req.Infra.Cluster["spec"].(map[string]any)
			require.True(t, ok)
			if tt.want == nil {
				require.NotContains(t, spec, "failureDomains")
			} else {
				require.Equal(t, tt.want, spec["failureDomains"])
			}
		})
	}
}

func TestUnit_ContainerCAPIReadyCommandsPreserveBundleSource(t *testing.T) {
	t.Setenv("NKE_E2E_BUNDLE_SERVER_URL", "http://bundle-mirror/")
	commands := containerCAPIReadyCommands()
	if len(commands) != 3 {
		t.Fatalf("pre-kubeadm command count = %d, want 3", len(commands))
	}
	agentEnv := filepath.Join(t.TempDir(), "nke-agent")
	if err := os.WriteFile(agentEnv, []byte("AGENT_ARGS=\"--secure-server-port 15005\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	command := strings.ReplaceAll(commands[0], "/etc/default/nke-agent", agentEnv)
	command = strings.ReplaceAll(command, "systemctl restart nke-agent.service", "true")
	for range 2 {
		if output, err := exec.CommandContext(t.Context(), "sh", "-c", command).CombinedOutput(); err != nil {
			t.Fatalf("agent override failed: %v: %s", err, output)
		}
	}
	got, err := os.ReadFile(agentEnv)
	if err != nil {
		t.Fatal(err)
	}
	want := "AGENT_ARGS=\"--secure-server-port 15005 --disable-mlx5-rescan --bundle-server-url http://bundle-mirror/ --index-file-url http://bundle-mirror/index.json\"\n"
	if string(got) != want {
		t.Fatalf("agent arguments = %q, want %q", got, want)
	}

	pool, err := buildCAPDNodePool(NodePoolSpec{Name: "worker"}, "node-image:local")
	if err != nil {
		t.Fatal(err)
	}
	if pool.Infra == nil || len(pool.Infra.PreKubeadmCommands) != 3 {
		t.Fatalf("CAPD node pool does not include the agent override: %+v", pool.Infra)
	}
	cp, err := buildCAPDControlPlaneConfig(ClusterSpec{
		CAPDNodeImage: "node-image:local",
		ControlPlane: ControlPlane{Config: &ControlPlaneConfigSpec{
			DesiredCount: 1,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cp.Infra == nil || len(cp.Infra.PreKubeadmCommands) != 3 {
		t.Fatalf("CAPD control plane does not include the agent override: %+v", cp.Infra)
	}

	t.Setenv("NKE_E2E_BUNDLE_SERVER_URL", "")
	if got := containerCAPIReadyCommands(); len(got) != 2 {
		t.Fatalf("post-init CNI command count = %d, want 2", len(got))
	}
}

func TestUnit_ContainerCAPIReadyCommandsReplaceBundleSource(t *testing.T) {
	type parameters struct {
		agentArgs string
		bundleURL string
		want      string
	}
	tests := map[string]parameters{
		"index flag without bundle flag": {
			agentArgs: `--secure-server-port 15005 --index-file-url http://old/index.json`,
			bundleURL: "http://bundle-mirror/",
			want:      `--secure-server-port 15005 --disable-mlx5-rescan --bundle-server-url http://bundle-mirror/ --index-file-url http://bundle-mirror/index.json`,
		},
		"replace stale flags without duplicates": {
			agentArgs: `--disable-mlx5-rescan --secure-server-port 15005 --bundle-server-url http://old/ --index-file-url http://old/index.json`,
			bundleURL: "http://bundle-mirror/",
			want:      `--secure-server-port 15005 --disable-mlx5-rescan --bundle-server-url http://bundle-mirror/ --index-file-url http://bundle-mirror/index.json`,
		},
		"preserve shell and sed characters": {
			agentArgs: `--secure-server-port 15005`,
			bundleURL: `http://bundle-mirror/a&b|c\d'$(id)/`,
			want:      `--secure-server-port 15005 --disable-mlx5-rescan --bundle-server-url http://bundle-mirror/a&b|c\d'$(id)/ --index-file-url http://bundle-mirror/a&b|c\d'$(id)/index.json`,
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("NKE_E2E_BUNDLE_SERVER_URL", params.bundleURL)
			agentEnv := filepath.Join(t.TempDir(), "nke-agent")
			if err := os.WriteFile(agentEnv, []byte("AGENT_ARGS=\""+params.agentArgs+"\"\nOTHER_SETTING=\"keep me\"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			command := containerCAPIReadyCommands()[0]
			command = strings.ReplaceAll(command, "/etc/default/nke-agent", agentEnv)
			command = strings.ReplaceAll(command, "systemctl restart nke-agent.service", "true")
			for range 2 {
				if output, err := exec.CommandContext(t.Context(), "sh", "-c", command).CombinedOutput(); err != nil {
					t.Fatalf("agent override failed: %v: %s", err, output)
				}
			}
			got, err := os.ReadFile(agentEnv)
			if err != nil {
				t.Fatal(err)
			}
			if want := "AGENT_ARGS=\"" + params.want + "\"\nOTHER_SETTING=\"keep me\"\n"; string(got) != want {
				t.Fatalf("agent arguments = %q, want %q", got, want)
			}
		})
	}
}

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
	t.Setenv("NKE_E2E_BUNDLE_SERVER_URL", "http://bundle-mirror/")
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

func TestBuildNicoNodePoolInterfaces(t *testing.T) {
	subnetA, subnetB := uuid.NewString(), uuid.NewString()
	prefix := uuid.NewString()

	for name, tc := range map[string]struct {
		subnets, vpcPrefixes []string
		want                 []map[string]any
		wantErr              string
	}{
		"subnet fabric": {
			subnets: []string{subnetA},
			want:    []map[string]any{{"subnetID": subnetA}},
		},
		"one interface per subnet": {
			subnets: []string{subnetA, subnetB},
			want:    []map[string]any{{"subnetID": subnetA}, {"subnetID": subnetB}},
		},
		// ytl: the pool names a prefix and no subnet, which is what failed as
		// "at least one subnet ID is required" before the builder knew prefixes.
		"VPC prefix fabric": {
			vpcPrefixes: []string{prefix},
			want:        []map[string]any{{"vpcPrefixID": prefix}},
		},
		"both kinds": {
			subnets:     []string{subnetA},
			vpcPrefixes: []string{prefix},
			wantErr:     "both subnet IDs and VPC prefix IDs are set",
		},
		"neither": {
			wantErr: "at least one subnet ID or VPC prefix ID is required",
		},
		"malformed prefix": {
			vpcPrefixes: []string{"not-a-uuid"},
			wantErr:     "parsing VPC prefix IDs",
		},
	} {
		t.Run(name, func(t *testing.T) {
			pool, err := buildNicoNodePool("c", uuid.New(), NodePoolSpec{
				Name:           "worker",
				InstanceTypeID: uuid.NewString(),
				SubnetIDs:      tc.subnets,
				VpcPrefixIDs:   tc.vpcPrefixes,
			}, NodePayloads{})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("building pool: %v", err)
			}
			if got := machineTemplateInterfaces(t, pool.Infra.MachineTemplate); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("interfaces = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBuildNicoControlPlaneConfigVpcPrefix(t *testing.T) {
	prefix := uuid.NewString()
	cfg, err := buildNicoControlPlaneConfig("c", uuid.New(), ClusterSpec{
		ControlPlane: ControlPlane{Config: &ControlPlaneConfigSpec{
			DesiredCount:   1,
			InstanceTypeID: uuid.NewString(),
			VpcPrefixIDs:   []string{prefix},
		}},
	}, NodePayloads{})
	if err != nil {
		t.Fatalf("building control plane config: %v", err)
	}
	want := []map[string]any{{"vpcPrefixID": prefix}}
	if got := machineTemplateInterfaces(t, cfg.Infra.MachineTemplate); !reflect.DeepEqual(got, want) {
		t.Errorf("control plane interfaces = %v, want %v: the prefix has to reach the machine template", got, want)
	}
}

// machineTemplateInterfaces digs the interfaces out of a NicoMachineTemplate.
func machineTemplateInterfaces(t *testing.T, template map[string]any) []map[string]any {
	t.Helper()
	spec, _ := template[capiSpecKey].(map[string]any)
	tmpl, _ := spec["template"].(map[string]any)
	inner, _ := tmpl[capiSpecKey].(map[string]any)
	interfaces, ok := inner["interfaces"].([]map[string]any)
	if !ok {
		t.Fatalf("machine template has no interfaces: %v", template)
	}
	return interfaces
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
