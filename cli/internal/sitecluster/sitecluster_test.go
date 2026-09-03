// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sitecluster

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/NVIDIA/nke/cli/internal/sitespec"
	api "github.com/NVIDIA/nke/controller/api"
)

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

// TestClusterRequest_IsACAPICluster pins the three things that make CWE treat
// this as a CAPI cluster rather than a Forge one. CWE rejects the request
// outright if any of them is wrong, but it rejects it at the far end of a
// bootstrap that has already built a Kind cluster and deployed five releases.
func TestClusterRequest_IsACAPICluster(t *testing.T) {
	req, err := ClusterRequest(testSiteSpec())
	if err != nil {
		t.Fatalf("ClusterRequest returned error: %v", err)
	}

	if req.SiteID != uuid.Nil {
		t.Errorf("siteID = %v, want the nil UUID: that is how CWE selects CAPI mode", req.SiteID)
	}
	if req.OrgID != "" {
		t.Errorf("orgID = %q, want empty: CWE rejects it for CAPI clusters", req.OrgID)
	}
	if req.VpcID != uuid.Nil {
		t.Errorf("vpcID = %v, want the nil UUID: CWE rejects it for CAPI clusters", req.VpcID)
	}
	if len(req.CapiInfraCluster) == 0 {
		t.Fatal("capiInfraCluster is empty; CWE requires it when siteID is the nil UUID")
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

	if got := req.CapiInfraCluster["kind"]; got != provisionedClusterKind {
		t.Errorf("capiInfraCluster kind = %v, want %q", got, provisionedClusterKind)
	}
	if got := req.CapiInfraCluster["apiVersion"]; got != provisionedAPIVersion {
		t.Errorf("capiInfraCluster apiVersion = %v, want %q", got, provisionedAPIVersion)
	}

	spec, ok := req.CapiInfraCluster["spec"].(map[string]interface{})
	if !ok {
		t.Fatalf("capiInfraCluster has no spec: %#v", req.CapiInfraCluster)
	}
	hosts, ok := spec["hosts"].([]map[string]interface{})
	if !ok {
		t.Fatalf("capiInfraCluster spec has no hosts: %#v", spec)
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

// TestClusterRequest_OneControlPlanePoolSizedToTheSiteSpec: every site spec
// node joins as a control-plane node, so a pool that does not count them all
// leaves hosts unused and the cluster smaller than the file asked for.
func TestClusterRequest_OneControlPlanePoolSizedToTheSiteSpec(t *testing.T) {
	req, err := ClusterRequest(testSiteSpec())
	if err != nil {
		t.Fatalf("ClusterRequest returned error: %v", err)
	}

	if req.NodePools == nil || len(*req.NodePools) != 1 {
		t.Fatalf("want exactly one node pool, got %#v", req.NodePools)
	}
	pool := (*req.NodePools)[0]
	if !pool.ControlPlane {
		t.Error("the pool is not a control-plane pool")
	}
	if pool.DesiredNodeCount != 3 {
		t.Errorf("desiredNodeCount = %d, want 3 site spec nodes", pool.DesiredNodeCount)
	}
	if pool.SystemReservedMemory != "512Mi" {
		t.Errorf("systemReservedMemory = %q, want the site spec's kubelet reservation", pool.SystemReservedMemory)
	}
	if got := pool.CapiInfraMachineTemplate["kind"]; got != provisionedMachineTemplateKind {
		t.Errorf("capiInfraMachineTemplate kind = %v, want %q", got, provisionedMachineTemplateKind)
	}
	if pool.CapiSpireAttestationMethod != api.SpireAttestationMethodCreateTpm {
		t.Errorf("capiSpireAttestationMethod = %q, want %q",
			pool.CapiSpireAttestationMethod, api.SpireAttestationMethodCreateTpm)
	}
}

// TestClusterRequest_PassesTheNetworkThrough covers both states: CIDRs named in
// the site spec reach CWE, and a site spec that names none leaves CWE's own
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
		t.Errorf("network = %#v, want nil so CWE applies its own defaults", req.Network)
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
// nothing type-checks them. Round-tripping through JSON is what the CWE client
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
