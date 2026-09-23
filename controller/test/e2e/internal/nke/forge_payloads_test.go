// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nke

import (
	"strings"
	"testing"
)

func completeForgeInputs() ForgeNodeInputs {
	return ForgeNodeInputs{
		BootLoaderURL:     "https://example.invalid/qcow-imager.efi",
		ImageURL:          "https://example.invalid/nke-24.04.qcow2",
		KubernetesVersion: "1.34.2",
		CNIVersion:        "1.4.0",
		SpireVersion:      "1.14.1+tpm1.9.0",
	}
}

func TestBuildForgeNodePayloadsRendersEveryInput(t *testing.T) {
	inputs := completeForgeInputs()
	payloads, err := BuildForgeNodePayloads(inputs)
	if err != nil {
		t.Fatalf("building payloads: %v", err)
	}

	if !strings.HasPrefix(payloads.IPXEScript, "#!ipxe\n") {
		t.Errorf("iPXE script should start with the shebang, got %q", firstLine(payloads.IPXEScript))
	}
	for _, want := range []string{inputs.BootLoaderURL, inputs.ImageURL} {
		if !strings.Contains(payloads.IPXEScript, want) {
			t.Errorf("iPXE script should name %q", want)
		}
	}

	// The template comment must not survive into what the machine boots.
	if strings.Contains(payloads.IPXEScript, "SPDX") {
		t.Error("iPXE script should not carry the template's own header")
	}

	joined := strings.Join(payloads.PreKubeadmCommands, "\n")
	for _, want := range []string{
		"nke-kubeadm-distro-" + inputs.KubernetesVersion,
		"nke-cni-ovn-images-" + inputs.CNIVersion,
		"spire-agent=" + inputs.SpireVersion,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("pre-kubeadm commands should install %q", want)
		}
	}
	for _, command := range payloads.PreKubeadmCommands {
		if strings.TrimSpace(command) == "" {
			t.Error("pre-kubeadm commands should carry no blank entries")
		}
	}
}

func TestBuildForgeNodePayloadsNamesEveryMissingInput(t *testing.T) {
	_, err := BuildForgeNodePayloads(ForgeNodeInputs{})
	if err == nil {
		t.Fatal("empty inputs should not render")
	}
	for _, field := range []string{"BootLoaderURL", "ImageURL", "KubernetesVersion", "CNIVersion", "SpireVersion"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("error should name %q, got %v", field, err)
		}
	}
}

// nico-forge rejects a spec with no payloads rather than sending the container
// stub to hardware, where it would boot nothing and install nothing.
func TestBuildNicoForgeRequestRequiresPayloads(t *testing.T) {
	spec := ClusterSpec{
		Name:              "forge",
		Backend:           BackendNicoForge,
		KubernetesVersion: "1.34.2",
		OrgID:             "novxwn552ssc",
		SiteID:            "e85ae455-2962-4742-b885-4b9ae22769c8",
		VpcID:             "b64210af-8f8e-4e37-ad0a-8cf7c6babf28",
	}

	if _, err := BuildCreateRequest(spec); err == nil {
		t.Fatal("a nico-forge request with no payloads should fail")
	}

	spec.NodePayloads = &NodePayloads{IPXEScript: "#!ipxe\n"}
	if _, err := BuildCreateRequest(spec); err == nil {
		t.Fatal("a nico-forge request with no pre-kubeadm commands should fail")
	}
}

// The manifests nico-forge builds are the ones CAPNico already reconciles for
// nico-containers, so the two differ in payloads and identifiers alone.
func TestBuildNicoForgeRequestCarriesThePayloads(t *testing.T) {
	payloads, err := BuildForgeNodePayloads(completeForgeInputs())
	if err != nil {
		t.Fatalf("building payloads: %v", err)
	}

	req, err := BuildCreateRequest(ClusterSpec{
		Name:              "forge",
		Backend:           BackendNicoForge,
		KubernetesVersion: "1.34.2",
		CNIPlugin:         "ovn-kubernetes",
		OrgID:             "novxwn552ssc",
		SiteID:            "e85ae455-2962-4742-b885-4b9ae22769c8",
		VpcID:             "b64210af-8f8e-4e37-ad0a-8cf7c6babf28",
		NodePayloads:      &payloads,
		ControlPlane: singlePoolControlPlane(NodePoolSpec{
			Name: "control-plane", NodeNamePrefix: "controlplane", ControlPlane: true, DesiredNodeCount: 1,
			InstanceTypeID: "d1a6401c-6b19-4261-92c3-79555cb56462",
			SubnetIDs:      []string{"53c42b79-3dbf-40e8-9363-75228d7b6e1b"},
			SSHKeyGroupIDs: []string{"5057cbb3-51ba-4568-b9ad-531cdd626d97"},
		}),
	})
	if err != nil {
		t.Fatalf("building request: %v", err)
	}

	// CAPI manifests, the same shape nico-containers builds, rather than the
	// Forge-native provider block nico-vms uses.
	if req.Infra == nil {
		t.Fatal("nico-forge should build CAPI infrastructure manifests")
	}
	if req.Nico != nil {
		t.Error("nico-forge should not also set the Forge-native provider: CWE accepts exactly one")
	}

	if req.NodePools == nil || len(*req.NodePools) != 1 {
		t.Fatalf("expected one node pool, got %v", req.NodePools)
	}
	pool := (*req.NodePools)[0]
	if got := pool.Infra.PreKubeadmCommands; len(got) != len(payloads.PreKubeadmCommands) {
		t.Errorf("pool carries %d pre-kubeadm commands, want %d", len(got), len(payloads.PreKubeadmCommands))
	}
	for _, command := range pool.Infra.PreKubeadmCommands {
		if strings.Contains(command, "bootstrap-success.complete") {
			t.Error("pool carries the container stub, not the Forge commands")
		}
	}
}

// nke_ControlPlane builds a single-pool control plane, keeping the table above
// readable.
func singlePoolControlPlane(pools ...NodePoolSpec) ControlPlane {
	return ControlPlane{Pools: pools, ControlPlaneVIP: "10.0.0.1"}
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
