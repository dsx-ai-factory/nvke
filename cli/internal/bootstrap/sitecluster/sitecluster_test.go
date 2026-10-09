// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sitecluster

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dsx-ai-factory/nke/cli/internal/bootstrap/sitespec"
	api "github.com/dsx-ai-factory/nke/controller/api"
)

func capiInfraCluster(t *testing.T, req api.CreateClusterJSONRequestBody) map[string]interface{} {
	t.Helper()
	if req.Infra == nil {
		t.Fatal("the request carries no infra payload")
	}
	return req.Infra.Cluster
}

func testSiteSpec() *sitespec.SiteSpec {
	return &sitespec.SiteSpec{
		ClusterName: "nke-site",
		Cluster: sitespec.Cluster{
			Nodes: []sitespec.Node{
				{Name: "node1", IP: "172.26.0.10", TPMEKThumbprint: strings.Repeat("1", 64)},
				{Name: "node2", IP: "172.26.0.11", TPMEKThumbprint: strings.Repeat("2", 64)},
				{Name: "node3", IP: "172.26.0.12", TPMEKThumbprint: strings.Repeat("3", 64)},
			},
			K8sVersion:    "1.32.0",
			CniName:       "calico",
			CniVersion:    "3.31.4",
			PodCidr:       "192.168.0.0/16",
			ServiceCidr:   "10.96.0.0/12",
			KubeletConfig: sitespec.KubeletConfig{SystemReservedMemory: "512Mi"},
		},
	}
}

// TestClusterRequest_IsACAPICluster pins the two things that make the controller treat
// this as a CAPI cluster rather than a Forge one. The controller rejects the request
// outright if either is wrong, but it rejects it at the far end of a
// bootstrap that has already built a Kind cluster and deployed five releases.
func TestClusterRequest_IsACAPICluster(t *testing.T) {
	req, err := ClusterRequest(testSiteSpec())
	if err != nil {
		t.Fatalf("ClusterRequest returned error: %v", err)
	}

	if req.Nico != nil {
		t.Errorf("nico = %#v, want nil: the controller rejects it for CAPI clusters", req.Nico)
	}
	if req.Infra == nil {
		t.Fatal("infra is nil; supplying it is how the controller selects CAPI mode")
	}
	if len(req.Infra.Cluster) == 0 {
		t.Fatal("infra.cluster is empty; the controller requires it for a CAPI cluster")
	}
}

// TestClusterRequest_CarriesTheSiteSpecAsProvisionedHosts is the heart of the
// mapping: the hosts an operator listed become the provider's site spec in the
// same order.
func TestClusterRequest_CarriesTheSiteSpecAsProvisionedHosts(t *testing.T) {
	req, err := ClusterRequest(testSiteSpec())
	if err != nil {
		t.Fatalf("ClusterRequest returned error: %v", err)
	}

	infra := capiInfraCluster(t, req)
	if got := infra["kind"]; got != provisionedClusterKind {
		t.Errorf("infra.cluster kind = %v, want %q", got, provisionedClusterKind)
	}
	if got := infra["apiVersion"]; got != provisionedAPIVersion {
		t.Errorf("infra.cluster apiVersion = %v, want %q", got, provisionedAPIVersion)
	}

	spec, ok := infra["spec"].(map[string]interface{})
	if !ok {
		t.Fatalf("infra.cluster has no spec: %#v", infra)
	}
	hosts, ok := spec["hosts"].([]map[string]interface{})
	if !ok {
		t.Fatalf("infra.cluster spec has no hosts: %#v", spec)
	}
	want := []struct{ hostname, ip, tpmEKPubHash string }{
		{"node1", "172.26.0.10", strings.Repeat("1", 64)},
		{"node2", "172.26.0.11", strings.Repeat("2", 64)},
		{"node3", "172.26.0.12", strings.Repeat("3", 64)},
	}
	if len(hosts) != len(want) {
		t.Fatalf("got %d hosts, want %d: %#v", len(hosts), len(want), hosts)
	}
	for i, w := range want {
		if hosts[i]["hostname"] != w.hostname || hosts[i]["ipAddress"] != w.ip ||
			hosts[i]["tpmEkPubHash"] != w.tpmEKPubHash {
			t.Errorf("host[%d] = %#v, want hostname=%q ipAddress=%q tpmEkPubHash=%q",
				i, hosts[i], w.hostname, w.ip, w.tpmEKPubHash)
		}
	}
}

// TestClusterRequest_ControlPlaneSizedToTheSiteSpec: every site spec node joins
// as a control-plane node, so a control plane that does not count them all
// leaves hosts unused and the cluster smaller than the file asked for.
func TestClusterRequest_ControlPlaneSizedToTheSiteSpec(t *testing.T) {
	req, err := ClusterRequest(testSiteSpec())
	if err != nil {
		t.Fatalf("ClusterRequest returned error: %v", err)
	}

	cp := req.ControlPlaneConfig
	if cp == nil {
		t.Fatal("ClusterRequest sent no controlPlaneConfig")
	}
	if cp.DesiredCount != 3 {
		t.Errorf("desiredCount = %d, want 3 site spec nodes", cp.DesiredCount)
	}
	if cp.SystemReservedMemory != "512Mi" {
		t.Errorf("systemReservedMemory = %q, want the site spec's kubelet reservation", cp.SystemReservedMemory)
	}
	if cp.Infra == nil {
		t.Fatal("controlPlaneConfig carries no infra payload")
	}
	if got := cp.Infra.MachineTemplate["kind"]; got != provisionedMachineTemplateKind {
		t.Errorf("infra.machineTemplate kind = %v, want %q", got, provisionedMachineTemplateKind)
	}
}

// TestClusterRequest_SendsNoControlPlaneNodePool: the controller rejects a control-plane
// node pool once control-plane controller provisioning is enabled, and a site
// cluster has no workers, so the request should carry no pools at all.
func TestClusterRequest_SendsNoControlPlaneNodePool(t *testing.T) {
	req, err := ClusterRequest(testSiteSpec())
	if err != nil {
		t.Fatalf("ClusterRequest returned error: %v", err)
	}

	if req.NodePools != nil && len(*req.NodePools) != 0 {
		t.Errorf("the request carries node pools, want none: %#v", *req.NodePools)
	}
}

// TestClusterRequest_PassesTheNetworkThrough covers both states: CIDRs named in
// the site spec reach the controller, and a site spec that names none leaves the controller's own
// defaults in place rather than sending empty strings.
func TestClusterRequest_PassesTheNetworkThrough(t *testing.T) {
	req, err := ClusterRequest(testSiteSpec())
	if err != nil {
		t.Fatalf("ClusterRequest returned error: %v", err)
	}
	if req.Network == nil || req.Network.PodCIDR != "192.168.0.0/16" || req.Network.ServiceCIDR != "10.96.0.0/12" {
		t.Errorf("network = %#v, want the site spec's CIDRs", req.Network)
	}

	inv := testSiteSpec()
	inv.Cluster.PodCidr, inv.Cluster.ServiceCidr = "", ""
	req, err = ClusterRequest(inv)
	if err != nil {
		t.Fatalf("ClusterRequest returned error: %v", err)
	}
	if req.Network != nil {
		t.Errorf("network = %#v, want nil so the controller applies its own defaults", req.Network)
	}
}

// TestClusterRequest_RejectsDuplicateHosts: the provider keys an allocation on
// a host's IP address, so two entries sharing one would be two machines bound
// to a single running node. It cannot see that; this can.
func TestClusterRequest_RejectsDuplicateHosts(t *testing.T) {
	inv := testSiteSpec()
	inv.Cluster.Nodes[2].IP = inv.Cluster.Nodes[0].IP
	if _, err := ClusterRequest(inv); err == nil {
		t.Error("expected an error for a repeated IP address")
	}

	inv = testSiteSpec()
	inv.Cluster.Nodes[1].Name = inv.Cluster.Nodes[0].Name
	if _, err := ClusterRequest(inv); err == nil {
		t.Error("expected an error for a repeated hostname")
	}
}

// TestClusterRequest_SerialisesToTheAPIShape: the manifests are built as
// map[string]interface{} because that is what the generated API takes, so
// nothing type-checks them. Round-tripping through JSON is what the controller client
// actually does, and it is the only check that the nesting is right.
func TestClusterRequest_SerialisesToTheAPIShape(t *testing.T) {
	req, err := ClusterRequest(testSiteSpec())
	if err != nil {
		t.Fatalf("ClusterRequest returned error: %v", err)
	}

	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshalling the request: %v", err)
	}
	for _, want := range []string{
		`"kind":"ProvisionedCluster"`,
		`"kind":"ProvisionedMachineTemplate"`,
		`"hostname":"node1"`,
		`"ipAddress":"172.26.0.10"`,
		`"template":{"spec":{}}`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("request JSON does not contain %s:\n%s", want, body)
		}
	}
}

// TestClusterRequest_CarriesTheControlPlaneVIP checks VIP forwarding and omission.
func TestClusterRequest_CarriesTheControlPlaneVIP(t *testing.T) {
	inv := testSiteSpec()
	inv.Cluster.ControlPlaneVIP = "10.0.0.100"

	req, err := ClusterRequest(inv)
	if err != nil {
		t.Fatalf("ClusterRequest returned error: %v", err)
	}
	if req.ControlPlaneVIP != "10.0.0.100" {
		t.Errorf("ControlPlaneVIP = %q, want %q", req.ControlPlaneVIP, "10.0.0.100")
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshalling the request: %v", err)
	}
	if !strings.Contains(string(body), `"controlPlaneVIP":"10.0.0.100"`) {
		t.Errorf("request JSON does not carry the VIP:\n%s", body)
	}

	empty := testSiteSpec() // no VIP
	req, err = ClusterRequest(empty)
	if err != nil {
		t.Fatalf("ClusterRequest returned error: %v", err)
	}
	if req.ControlPlaneVIP != "" {
		t.Errorf("ControlPlaneVIP = %q, want empty", req.ControlPlaneVIP)
	}
	body, err = json.Marshal(req)
	if err != nil {
		t.Fatalf("marshalling the request: %v", err)
	}
	if strings.Contains(string(body), "controlPlaneVIP") {
		t.Errorf("an empty VIP must be omitted from the JSON body, but it is present:\n%s", body)
	}
}

func TestClusterRequest_RejectsANilSiteSpec(t *testing.T) {
	if _, err := ClusterRequest(nil); err == nil {
		t.Error("expected an error for a nil site spec")
	}
}

func TestClusterRequest_RejectsNoNodes(t *testing.T) {
	inv := testSiteSpec()
	inv.Cluster.Nodes = nil
	if _, err := ClusterRequest(inv); err == nil {
		t.Error("expected an error for a site spec with no nodes")
	}
}

// TestClusterRequest_EtcdEncryption: the request carries the site spec's
// decision and nothing else decides it here. Unset resolves to on so a spec
// that skipped SetDefaults still cannot produce an unencrypted cluster.
func TestClusterRequest_EtcdEncryption(t *testing.T) {
	on, off := true, false
	for _, tc := range []struct {
		name  string
		field *bool
		want  bool
	}{
		{"unset", nil, true},
		{"true", &on, true},
		{"false", &off, false},
	} {
		inv := testSiteSpec()
		inv.Cluster.EtcdEncryption = tc.field
		req, err := ClusterRequest(inv)
		if err != nil {
			t.Fatalf("%s: ClusterRequest: %v", tc.name, err)
		}
		if req.EtcdEncrypted != tc.want {
			t.Errorf("%s: etcdEncrypted = %v, want %v", tc.name, req.EtcdEncrypted, tc.want)
		}
	}
}

func TestClusterRequest_ForwardsVIPMatchingNodeIP(t *testing.T) {
	for _, index := range []int{0, 1} {
		inv := testSiteSpec()
		vip := inv.Cluster.Nodes[index].IP
		t.Run(inv.Cluster.Nodes[index].Name, func(t *testing.T) {
			inv.Cluster.ControlPlaneVIP = vip
			req, err := ClusterRequest(inv)
			if err != nil {
				t.Fatalf("ClusterRequest: %v", err)
			}
			if req.ControlPlaneVIP != vip {
				t.Errorf("ControlPlaneVIP = %q, want %q", req.ControlPlaneVIP, vip)
			}
			body, err := json.Marshal(req)
			if err != nil {
				t.Fatalf("marshal request: %v", err)
			}
			if !strings.Contains(string(body), `"controlPlaneVIP":"`+vip+`"`) {
				t.Errorf("request JSON does not carry VIP %q", vip)
			}
		})
	}
}
