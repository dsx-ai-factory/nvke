// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nke

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/NVIDIA/nke/controller/api"
)

// The e2e specs themselves need docker and a management cluster, so these unit
// tests are what keeps the request builder honest in CI. A builder that quietly
// stopped emitting the inventory would otherwise only be caught by a 40-minute
// run nobody triggers.

func preprovisionedTestSpec() ClusterSpec {
	return ClusterSpec{
		Name:              "preprov",
		Backend:           BackendPreprovisioned,
		KubernetesVersion: "1.34.2",
		CNIPlugin:         "calico",
		PreprovisionedHosts: []PreprovisionedHost{
			{Name: "host-01", IP: "10.0.0.11"},
			{Name: "host-02", IP: "10.0.0.12", Role: "control-plane"},
		},
		ControlPlane: ControlPlane{
			ControlPlaneVIP: "10.0.0.10",
			Pools: []NodePoolSpec{{
				Name:             "cp",
				NodeNamePrefix:   "cp",
				DesiredNodeCount: 2,
			}},
		},
	}
}

func TestBuildPreprovisionedRequestCarriesInventoryInInfraCluster(t *testing.T) {
	req, err := buildCreateRequest(preprovisionedTestSpec())
	if err != nil {
		t.Fatalf("buildCreateRequest: %v", err)
	}

	// The nil site is what puts CWE in generic-CAPI mode; a real UUID would
	// route the request down the Forge path instead.
	if req.SiteID != uuid.Nil {
		t.Fatalf("SiteID = %s, want the nil UUID", req.SiteID)
	}
	if req.ControlPlaneVIP != "10.0.0.10" {
		t.Fatalf("ControlPlaneVIP = %q, want the substrate VIP", req.ControlPlaneVIP)
	}
	if !strings.HasPrefix(req.Name, "preprov-") {
		t.Fatalf("Name = %q, want a timestamped preprov name", req.Name)
	}

	if got := req.CapiInfraCluster["kind"]; got != "PreprovisionedCluster" {
		t.Fatalf("infra cluster kind = %v, want PreprovisionedCluster", got)
	}
	if got := req.CapiInfraCluster["apiVersion"]; got != "infrastructure.cluster.x-k8s.io/v1alpha1" {
		t.Fatalf("infra cluster apiVersion = %v", got)
	}

	spec, ok := req.CapiInfraCluster["spec"].(map[string]any)
	if !ok {
		t.Fatalf("infra cluster has no spec: %#v", req.CapiInfraCluster)
	}
	hosts, ok := spec["hosts"].([]map[string]any)
	if !ok {
		t.Fatalf("infra cluster spec has no hosts: %#v", spec)
	}
	if len(hosts) != 2 {
		t.Fatalf("hosts = %d, want 2", len(hosts))
	}

	if hosts[0]["name"] != "host-01" {
		t.Fatalf("first host name = %v", hosts[0]["name"])
	}
	addresses, ok := hosts[0]["addresses"].([]map[string]any)
	if !ok || len(addresses) != 1 {
		t.Fatalf("first host addresses = %#v", hosts[0]["addresses"])
	}
	if addresses[0]["address"] != "10.0.0.11" || addresses[0]["type"] != "InternalIP" {
		t.Fatalf("first host address = %#v", addresses[0])
	}

	// An empty role must be omitted rather than sent as "", which the CRD enum
	// rejects.
	if _, present := hosts[0]["role"]; present {
		t.Fatalf("host without a role should not carry the field: %#v", hosts[0])
	}
	if hosts[1]["role"] != "control-plane" {
		t.Fatalf("second host role = %v", hosts[1]["role"])
	}
}

func TestBuildPreprovisionedRequestNodePool(t *testing.T) {
	req, err := buildCreateRequest(preprovisionedTestSpec())
	if err != nil {
		t.Fatalf("buildCreateRequest: %v", err)
	}
	if req.NodePools == nil || len(*req.NodePools) != 1 {
		t.Fatalf("node pools = %#v, want exactly one", req.NodePools)
	}

	pool := (*req.NodePools)[0]
	if !pool.ControlPlane {
		t.Fatal("the pool should be a control-plane pool")
	}
	if pool.DesiredNodeCount != 2 {
		t.Fatalf("DesiredNodeCount = %d, want 2", pool.DesiredNodeCount)
	}

	// A preprovisioned host booted before CWE knew about it, so no cloud-init
	// could deliver SPIRE material. Anything but "none" makes CWE demand mTLS
	// against an agent that has no identity.
	if pool.CapiSpireAttestationMethod != api.SpireAttestationMethodCreateNone {
		t.Fatalf("CapiSpireAttestationMethod = %q, want none", pool.CapiSpireAttestationMethod)
	}

	if got := pool.CapiInfraMachineTemplate["kind"]; got != "PreprovisionedMachineTemplate" {
		t.Fatalf("machine template kind = %v", got)
	}
	template, ok := pool.CapiInfraMachineTemplate["spec"].(map[string]any)
	if !ok {
		t.Fatalf("machine template has no spec: %#v", pool.CapiInfraMachineTemplate)
	}
	inner, ok := template["template"].(map[string]any)
	if !ok {
		t.Fatalf("machine template has no template: %#v", template)
	}
	machineSpec, ok := inner["spec"].(map[string]any)
	if !ok {
		t.Fatalf("machine template has no machine spec: %#v", inner)
	}
	// Nothing is created, so nothing is configured. A non-empty spec here would
	// mean the harness had started describing infrastructure.
	if len(machineSpec) != 0 {
		t.Fatalf("machine spec = %#v, want empty", machineSpec)
	}

	// Forge-only fields are rejected by the API for a CAPI cluster, so the
	// builder must leave them unset.
	if pool.InstanceTypeID != uuid.Nil {
		t.Fatalf("InstanceTypeID = %s, want unset", pool.InstanceTypeID)
	}
	if pool.OSProfile != "" {
		t.Fatalf("OSProfile = %q, want unset", pool.OSProfile)
	}
}

func TestBuildPreprovisionedRequestRejectsIncompleteInventory(t *testing.T) {
	tests := map[string]ClusterSpec{
		"no hosts": func() ClusterSpec {
			spec := preprovisionedTestSpec()
			spec.PreprovisionedHosts = nil
			return spec
		}(),
		"host without an address": func() ClusterSpec {
			spec := preprovisionedTestSpec()
			spec.PreprovisionedHosts = []PreprovisionedHost{{Name: "host-01"}}
			return spec
		}(),
		"host without a name": func() ClusterSpec {
			spec := preprovisionedTestSpec()
			spec.PreprovisionedHosts = []PreprovisionedHost{{IP: "10.0.0.11"}}
			return spec
		}(),
	}

	for name, spec := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := buildCreateRequest(spec); err == nil {
				t.Fatal("expected an error, got none")
			}
		})
	}
}

func TestBuildPreprovisionedRequestWorkerPoolsFollowControlPlane(t *testing.T) {
	spec := preprovisionedTestSpec()
	spec.Workers = []NodePoolSpec{{
		Name:             "worker",
		NodeNamePrefix:   "w",
		DesiredNodeCount: 1,
	}}

	req, err := buildCreateRequest(spec)
	if err != nil {
		t.Fatalf("buildCreateRequest: %v", err)
	}
	pools := *req.NodePools
	if len(pools) != 2 {
		t.Fatalf("node pools = %d, want 2", len(pools))
	}
	if !pools[0].ControlPlane || pools[1].ControlPlane {
		t.Fatalf("pool roles = [%t %t], want control plane first", pools[0].ControlPlane, pools[1].ControlPlane)
	}
}
