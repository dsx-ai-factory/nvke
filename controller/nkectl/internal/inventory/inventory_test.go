// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package inventory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validYAML is a minimal well-formed inventory used as the happy-path baseline.
const validYAML = `
clusterName: nke-site
cluster:
  initNode:
    name: node1
    ip: 10.0.0.1
  joinNodes:
    - name: node2
      ip: 10.0.0.2
    - name: node3
      ip: 10.0.0.3
  k8sVersion: "1.32.0"
  cniName: calico
  cniVersion: "3.31.4"
  podCidr: "10.100.0.0/16"
  serviceCidr: "10.200.0.0/16"
  kubeletConfig:
    systemReservedMemory: "16Gi"
registry:
  bootstrapRepo: "nvcr.io/org/cloud-workflow-engine"
  bootstrapChart: "cloud-workflow-engine"
`

func TestLoadValidInventory(t *testing.T) {
	path := writeTemp(t, validYAML)

	inv, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error for valid inventory: %v", err)
	}

	if inv.ClusterName != "nke-site" {
		t.Errorf("ClusterName = %q, want %q", inv.ClusterName, "nke-site")
	}
	if inv.Cluster.InitNode.IP != "10.0.0.1" {
		t.Errorf("InitNode.IP = %q, want %q", inv.Cluster.InitNode.IP, "10.0.0.1")
	}
	if len(inv.Cluster.JoinNodes) != 2 {
		t.Fatalf("JoinNodes count = %d, want 2", len(inv.Cluster.JoinNodes))
	}
	if inv.Registry.BootstrapRepo != "nvcr.io/org/cloud-workflow-engine" {
		t.Errorf("Registry.BootstrapRepo = %q, want %q", inv.Registry.BootstrapRepo, "nvcr.io/org/cloud-workflow-engine")
	}
	if inv.Registry.BootstrapChart != "cloud-workflow-engine" {
		t.Errorf("Registry.BootstrapChart = %q, want %q", inv.Registry.BootstrapChart, "cloud-workflow-engine")
	}
	// NodeAgentPort is optional and should be defaulted.
	if inv.Cluster.NodeAgentPort != DefaultNodeAgentPort {
		t.Errorf("NodeAgentPort = %q, want default %q", inv.Cluster.NodeAgentPort, DefaultNodeAgentPort)
	}
}

func TestLoadNoJoinNodes(t *testing.T) {
	const singleNode = `
clusterName: solo
cluster:
  initNode:
    name: node1
    ip: 10.0.0.1
  k8sVersion: "1.32.0"
  cniName: ovn-kubernetes
  podCidr: "10.100.0.0/16"
  serviceCidr: "10.200.0.0/16"
registry:
  bootstrapRepo: "nvcr.io/org/cloud-workflow-engine"
  bootstrapChart: "cloud-workflow-engine"
`
	inv, err := Load(writeTemp(t, singleNode))
	if err != nil {
		t.Fatalf("Load returned error for single-node inventory: %v", err)
	}
	if len(inv.Cluster.JoinNodes) != 0 {
		t.Errorf("JoinNodes count = %d, want 0", len(inv.Cluster.JoinNodes))
	}
}

func TestSetDefaultsNeverInjectsAPIKey(t *testing.T) {
	// Regression guard for the tier-0 known issue: SetDefaults must not fill
	// in credentials. A missing registry.apiKey stays empty.
	inv := &Inventory{}
	inv.SetDefaults()
	if inv.Registry.APIKey != "" {
		t.Errorf("SetDefaults set Registry.APIKey = %q, want empty", inv.Registry.APIKey)
	}
	if inv.Cluster.NodeAgentPort != DefaultNodeAgentPort {
		t.Errorf("SetDefaults NodeAgentPort = %q, want %q", inv.Cluster.NodeAgentPort, DefaultNodeAgentPort)
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	// A typo in a field name must fail loudly, not be silently ignored.
	const typo = `
clusterName: nke-site
cluster:
  initNode:
    name: node1
    ip: 10.0.0.1
  k8sVersion: "1.32.0"
  cniName: calico
  podCidr: "10.100.0.0/16"
  serviceCidr: "10.200.0.0/16"
  cniVersionn: "3.31.4"
registry:
  bootstrapRepo: "nvcr.io/org/cloud-workflow-engine"
  bootstrapChart: "cloud-workflow-engine"
`
	if _, err := Load(writeTemp(t, typo)); err == nil {
		t.Fatal("Load accepted an inventory with an unknown field; want error")
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml")); err == nil {
		t.Fatal("Load accepted a missing file; want error")
	}
}

func TestLoadEmptyFile(t *testing.T) {
	if _, err := Load(writeTemp(t, "")); err == nil {
		t.Fatal("Load accepted an empty file; want error")
	}
}

// TestValidateInvalidInputs drives the validator over a range of bad fields.
// Each case asserts that the specific offending field is named in the error so
// the operator gets an actionable message.
func TestValidateInvalidInputs(t *testing.T) {
	base := func() *Inventory {
		return &Inventory{
			ClusterName: "nke-site",
			Cluster: Cluster{
				InitNode:      Node{Name: "node1", IP: "10.0.0.1"},
				K8sVersion:    "1.32.0",
				CniName:       "calico",
				PodCidr:       "10.100.0.0/16",
				ServiceCidr:   "10.200.0.0/16",
				NodeAgentPort: DefaultNodeAgentPort,
			},
			Registry: Registry{
				BootstrapRepo:  "nvcr.io/org/cloud-workflow-engine",
				BootstrapChart: "cloud-workflow-engine",
			},
		}
	}

	tests := []struct {
		name    string
		mutate  func(*Inventory)
		wantSub string
	}{
		{"missing clusterName", func(i *Inventory) { i.ClusterName = "" }, pathClusterName},
		{"bad clusterName", func(i *Inventory) { i.ClusterName = "Nke_Site" }, pathClusterName},
		{"missing init ip", func(i *Inventory) { i.Cluster.InitNode.IP = "" }, pathInitNodeIP},
		{"bad init ip", func(i *Inventory) { i.Cluster.InitNode.IP = "999.0.0.1" }, pathInitNodeIP},
		{"missing init name", func(i *Inventory) { i.Cluster.InitNode.Name = "" }, pathInitNodeName},
		{"bad join ip", func(i *Inventory) {
			i.Cluster.JoinNodes = []Node{{Name: "node2", IP: "not-an-ip"}}
		}, "cluster.joinNodes[0].ip"},
		{"missing join name", func(i *Inventory) {
			i.Cluster.JoinNodes = []Node{{Name: "", IP: "10.0.0.2"}}
		}, "cluster.joinNodes[0].name"},
		{"duplicate node name", func(i *Inventory) {
			i.Cluster.JoinNodes = []Node{{Name: "node1", IP: "10.0.0.2"}}
		}, "name \"node1\" is used"},
		{"duplicate node ip", func(i *Inventory) {
			i.Cluster.JoinNodes = []Node{{Name: "node2", IP: "10.0.0.1"}}
		}, "ip \"10.0.0.1\" is used"},
		{"missing k8sVersion", func(i *Inventory) { i.Cluster.K8sVersion = "" }, pathK8sVersion},
		{"bad k8sVersion", func(i *Inventory) { i.Cluster.K8sVersion = "v1.32" }, pathK8sVersion},
		{"missing cni", func(i *Inventory) { i.Cluster.CniName = "" }, pathCniName},
		{"unsupported cni", func(i *Inventory) { i.Cluster.CniName = "flannel" }, pathCniName},
		{"missing podCidr", func(i *Inventory) { i.Cluster.PodCidr = "" }, pathPodCidr},
		{"bad podCidr", func(i *Inventory) { i.Cluster.PodCidr = "10.100.0.0" }, pathPodCidr},
		{"bad podCidr in list", func(i *Inventory) { i.Cluster.PodCidr = "10.100.0.0/16,nope" }, pathPodCidr},
		{"missing serviceCidr", func(i *Inventory) { i.Cluster.ServiceCidr = "" }, pathServiceCidr},
		{"bad nodeAgentPort", func(i *Inventory) { i.Cluster.NodeAgentPort = "70000" }, pathNodeAgentPort},
		{"missing bootstrapRepo", func(i *Inventory) { i.Registry.BootstrapRepo = "" }, pathBootstrapRepo},
		{"missing bootstrapChart", func(i *Inventory) { i.Registry.BootstrapChart = "" }, pathBootstrapChart},
		{"whitespace in bootstrapRepo", func(i *Inventory) { i.Registry.BootstrapRepo = "bad repo" }, pathBootstrapRepo},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inv := base()
			tc.mutate(inv)
			err := inv.Validate()
			if err == nil {
				t.Fatalf("Validate accepted invalid input; want error mentioning %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestValidateAggregatesErrors confirms that multiple problems are reported
// together rather than one at a time.
func TestValidateAggregatesErrors(t *testing.T) {
	inv := &Inventory{} // everything missing
	err := inv.Validate()
	if err == nil {
		t.Fatal("Validate accepted an empty inventory; want error")
	}
	for _, want := range []string{pathClusterName, pathInitNodeIP, pathK8sVersion, pathBootstrapRepo} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("aggregated error missing %q; got:\n%s", want, err.Error())
		}
	}
}

func TestValidValidatesClean(t *testing.T) {
	inv := &Inventory{
		ClusterName: "nke-site",
		Cluster: Cluster{
			InitNode:      Node{Name: "node1", IP: "10.0.0.1"},
			JoinNodes:     []Node{{Name: "node2", IP: "10.0.0.2"}},
			K8sVersion:    "1.32.0",
			CniName:       "ovn-kubernetes",
			PodCidr:       "10.100.0.0/16,10.101.0.0/16",
			ServiceCidr:   "10.200.0.0/16",
			NodeAgentPort: "15005",
		},
		Registry: Registry{
			BootstrapRepo:  "nvcr.io/org/cloud-workflow-engine",
			BootstrapChart: "cloud-workflow-engine",
		},
	}
	if err := inv.Validate(); err != nil {
		t.Errorf("Validate rejected a valid inventory: %v", err)
	}
}

// noJoinYAML is a valid inventory with no join nodes, used to exercise the
// JOIN_NODES environment overlay.
const noJoinYAML = `
clusterName: nke-site
cluster:
  initNode:
    name: node1
    ip: 10.0.0.1
  k8sVersion: "1.32.0"
  cniName: calico
  podCidr: "10.100.0.0/16"
  serviceCidr: "10.200.0.0/16"
registry:
  bootstrapRepo: "nvcr.io/org/cloud-workflow-engine"
  bootstrapChart: "cloud-workflow-engine"
`

func TestLoadEnvOverridesFile(t *testing.T) {
	// validYAML sets clusterName=nke-site, k8sVersion=1.32.0, initNode.ip=10.0.0.1.
	// Each env var must win over the file value, including the deeply-nested one.
	t.Setenv("CLUSTER_NAME", "from-env")
	t.Setenv("CLUSTER_K8S_VERSION", "1.33.1")
	t.Setenv("INIT_NODE_IP", "10.9.9.9")

	inv, err := Load(writeTemp(t, validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if inv.ClusterName != "from-env" {
		t.Errorf("ClusterName = %q, want env override %q", inv.ClusterName, "from-env")
	}
	if inv.Cluster.K8sVersion != "1.33.1" {
		t.Errorf("K8sVersion = %q, want env override %q", inv.Cluster.K8sVersion, "1.33.1")
	}
	if inv.Cluster.InitNode.IP != "10.9.9.9" {
		t.Errorf("InitNode.IP = %q, want nested env override %q", inv.Cluster.InitNode.IP, "10.9.9.9")
	}
}

func TestLoadEnvFillsField(t *testing.T) {
	// validYAML sets no nodeAgentPort; the env var supplies it.
	t.Setenv("CLUSTER_NODE_AGENT_PORT", "15005")

	inv, err := Load(writeTemp(t, validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if inv.Cluster.NodeAgentPort != "15005" {
		t.Errorf("NodeAgentPort = %q, want env value %q", inv.Cluster.NodeAgentPort, "15005")
	}
}

func TestLoadJoinNodesFromEnv(t *testing.T) {
	t.Setenv("JOIN_NODES", "node2:10.0.0.2,node3:10.0.0.3")

	inv, err := Load(writeTemp(t, noJoinYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(inv.Cluster.JoinNodes) != 2 {
		t.Fatalf("JoinNodes count = %d, want 2", len(inv.Cluster.JoinNodes))
	}
	if inv.Cluster.JoinNodes[0].Name != "node2" || inv.Cluster.JoinNodes[0].IP != "10.0.0.2" {
		t.Errorf("JoinNodes[0] = %+v, want {node2 10.0.0.2}", inv.Cluster.JoinNodes[0])
	}
}

func TestLoadJoinNodesEnvMalformedFails(t *testing.T) {
	// A malformed JOIN_NODES entry must fail loudly, not be silently discarded.
	t.Setenv("JOIN_NODES", "node2:10.0.0.2,bogus")

	if _, err := Load(writeTemp(t, noJoinYAML)); err == nil {
		t.Fatal("Load accepted a malformed JOIN_NODES entry; want error")
	}
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "inventory.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp inventory: %v", err)
	}
	return path
}
