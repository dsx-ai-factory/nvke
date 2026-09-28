// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nke

import (
	"strings"
	"testing"
)

// Synthetic identifiers. Nothing here reaches Forge -- these are carried
// through a request builder and read back out of it -- so a fixture only has
// to be distinguishable from its neighbors in a failure message. UUID-shaped
// because BuildCreateRequest parses them, which is a check worth keeping:
// nico-forge rejects a malformed id here rather than at the API.
const (
	testOrgID          = "0000000000000000"
	testSiteID         = "00000000-0000-4000-8000-000000000001"
	testVpcID          = "00000000-0000-4000-8000-000000000002"
	testInstanceTypeID = "00000000-0000-4000-8000-000000000003"
	testSubnetID       = "00000000-0000-4000-8000-000000000004"
	testSSHKeyGroupID  = "00000000-0000-4000-8000-000000000005"
)

func completeForgeInputs() NodeInputs {
	return NodeInputs{
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

	// The charts package and openvswitch-switch are asserted because dropping
	// either is silent here and fatal on hardware: the agent refuses InstallCNI
	// without the charts deb and without host ovs-vsctl, and that failure
	// terminates provisioning rather than retrying.
	joined := strings.Join(payloads.PreKubeadmCommands, "\n")
	for _, want := range []string{
		"nke-kubeadm-distro-" + inputs.KubernetesVersion,
		"nke-cni-ovn-images-" + inputs.CNIVersion,
		"nke-cni-ovn-charts-" + inputs.CNIVersion,
		"openvswitch-switch",
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

func TestBuildForgeNodePayloadsAgentArgs(t *testing.T) {
	for _, tc := range []struct {
		name, args, want string
	}{
		{"defaults to the dialed port", "", `AGENT_ARGS="--secure-server-port 15005"`},
		{"carries an override verbatim", "--secure-server-port 15005 --disable-mtls", `AGENT_ARGS="--secure-server-port 15005 --disable-mtls"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inputs := completeForgeInputs()
			inputs.AgentArgs = tc.args
			payloads, err := BuildForgeNodePayloads(inputs)
			if err != nil {
				t.Fatalf("building payloads: %v", err)
			}
			if joined := strings.Join(payloads.PreKubeadmCommands, "\n"); !strings.Contains(joined, tc.want) {
				t.Errorf("pre-kubeadm commands should write %s", tc.want)
			}
		})
	}
}

func TestBuildForgeNodePayloadsNamesEveryMissingInput(t *testing.T) {
	_, err := BuildForgeNodePayloads(NodeInputs{})
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
		OrgID:             testOrgID,
		SiteID:            testSiteID,
		VpcID:             testVpcID,
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
		OrgID:             testOrgID,
		SiteID:            testSiteID,
		VpcID:             testVpcID,
		NodePayloads:      &payloads,
		ControlPlane: singlePoolControlPlane(NodePoolSpec{
			Name: "control-plane", NodeNamePrefix: "controlplane", ControlPlane: true, DesiredNodeCount: 1,
			InstanceTypeID: testInstanceTypeID,
			SubnetIDs:      []string{testSubnetID},
			SSHKeyGroupIDs: []string{testSSHKeyGroupID},
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
