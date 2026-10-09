// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nke

import (
	"strings"
	"testing"

	"github.com/dsx-ai-factory/nke/controller/api"
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
		ImageURL:          "https://example.invalid/nke-node-non-fde-ubuntu-24.04.1-amd64.qcow2",
		Arch:              "amd64",
		KubernetesVersion: "1.34.2",
		CNIVersion:        "1.4.0",
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
	for _, field := range []string{"BootLoaderURL", "ImageURL", "KubernetesVersion", "CNIVersion"} {
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

	spec.NodePayloads = &RolePayloads{
		ControlPlane: NodePayloads{IPXEScript: "#!ipxe\n"},
		Worker:       NodePayloads{IPXEScript: "#!ipxe\n"},
	}
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
		NodePayloads:      &RolePayloads{ControlPlane: payloads, Worker: payloads},
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
		t.Error("nico-forge should not also set the Forge-native provider: the controller accepts exactly one")
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

// TestBuildNicoForgeRequestRoutesEachRoleItsOwnPayload pins which role's
// payload reaches which pool.
//
// Nothing else does. The two roles are routed by a bool, both branches return
// the same type, and every other fixture here passes one payload for both
// roles, so inverting the branch leaves every assertion in this package
// passing. What it would do on hardware is boot a mixed-architecture tenant's
// workers on the control plane's image, which never reports and looks exactly
// like the machines failing to provision.
//
// Distinguishable payloads rather than real ones: what is under test is the
// routing, and a marker is the only thing an assertion can follow end to end.
func TestBuildNicoForgeRequestRoutesEachRoleItsOwnPayload(t *testing.T) {
	marker := func(role string) NodePayloads {
		return NodePayloads{
			IPXEScript:         "#!ipxe\necho " + role + "\n",
			PreKubeadmCommands: []string{"echo " + role},
		}
	}

	req, err := BuildCreateRequest(ClusterSpec{
		Name:              "forge",
		Backend:           BackendNicoForge,
		KubernetesVersion: "1.34.2",
		CNIPlugin:         "ovn-kubernetes",
		OrgID:             testOrgID,
		SiteID:            testSiteID,
		VpcID:             testVpcID,
		NodePayloads:      &RolePayloads{ControlPlane: marker("control-plane"), Worker: marker("worker")},
		ControlPlane: singlePoolControlPlane(NodePoolSpec{
			Name: "cp", NodeNamePrefix: "cp", ControlPlane: true, DesiredNodeCount: 1,
			InstanceTypeID: testInstanceTypeID,
			SubnetIDs:      []string{testSubnetID},
			SSHKeyGroupIDs: []string{testSSHKeyGroupID},
		}),
		Workers: []NodePoolSpec{{
			Name: "workers", NodeNamePrefix: "worker", DesiredNodeCount: 1,
			InstanceTypeID: testInstanceTypeID,
			SubnetIDs:      []string{testSubnetID},
			SSHKeyGroupIDs: []string{testSSHKeyGroupID},
		}},
	})
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	if req.NodePools == nil || len(*req.NodePools) != 2 {
		t.Fatalf("expected a control-plane pool and a worker pool, got %v", req.NodePools)
	}

	for _, pool := range *req.NodePools {
		// ControlPlane is a plain bool on the pool, not a pointer.
		want := "worker"
		if pool.ControlPlane {
			want = "control-plane"
		}
		other := "worker"
		if want == "worker" {
			other = "control-plane"
		}

		commands := strings.Join(pool.Infra.PreKubeadmCommands, "\n")
		if !strings.Contains(commands, want) {
			t.Errorf("pool %q carries pre-kubeadm commands %q, want the %s payload", pool.Name, commands, want)
		}
		if strings.Contains(commands, other) {
			t.Errorf("pool %q carries the %s payload's pre-kubeadm commands: %q", pool.Name, other, commands)
		}

		script := ipxeScriptOf(t, pool)
		if !strings.Contains(script, want) {
			t.Errorf("pool %q carries iPXE script %q, want the %s payload", pool.Name, script, want)
		}
		if strings.Contains(script, other) {
			t.Errorf("pool %q carries the %s payload's iPXE script: %q", pool.Name, other, script)
		}
	}
}

// ipxeScriptOf digs the script out of the pool's NicoMachineTemplate, which is
// an untyped map because that is how the controller takes a CAPI manifest.
func ipxeScriptOf(t *testing.T, pool api.NodePoolCreateConfig) string {
	t.Helper()
	spec, ok := pool.Infra.MachineTemplate[capiSpecKey].(map[string]any)
	if !ok {
		t.Fatalf("pool %q machine template has no %s", pool.Name, capiSpecKey)
	}
	template, ok := spec["template"].(map[string]any)
	if !ok {
		t.Fatalf("pool %q machine template has no template", pool.Name)
	}
	inner, ok := template[capiSpecKey].(map[string]any)
	if !ok {
		t.Fatalf("pool %q template has no %s", pool.Name, capiSpecKey)
	}
	script, _ := inner["ipxeScript"].(string)
	return script
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

// TestBuildForgeNodePayloadsBootParametersFollowTheArch covers the split that
// blocked the first GB200 attempt: the amd64 line names an Intel IOMMU and two
// x86-only PCI parameters, which an ARM kernel cannot act on.
func TestBuildForgeNodePayloadsBootParametersFollowTheArch(t *testing.T) {
	for _, tc := range []struct {
		arch             string
		want, wantAbsent []string
	}{
		{
			arch:       "amd64",
			want:       []string{"console=ttyS1,115200", "intel_iommu=on", "pci=realloc=off", "reboot=pci"},
			wantAbsent: []string{"ttyAMA0", "ip=dhcp", "fb=false"},
		},
		{
			// Asserted against the ytl GB200 instances rather than against
			// what the amd64 line suggests, because the two differ in ways no
			// rule predicts: pci=realloc=off survives, iommu=pt is spelled the
			// same, and ip=dhcp and fb=false appear only here.
			arch:       "arm64",
			want:       []string{"console=ttyAMA0,115200", "ip=dhcp", "fb=false", "pci=realloc=off", "iommu=pt"},
			wantAbsent: []string{"intel_iommu", "reboot=pci", "ttyS1", "console=tty0", "iommu.passthrough"},
		},
	} {
		t.Run(tc.arch, func(t *testing.T) {
			inputs := completeForgeInputs()
			inputs.Arch = tc.arch
			// The image has to match, or checkArch rejects the pair before
			// the boot parameters are reached.
			inputs.ImageURL = "https://example.invalid/nke-node-non-fde-ubuntu-24.04.1-" + tc.arch + ".qcow2"
			payloads, err := BuildForgeNodePayloads(inputs)
			if err != nil {
				t.Fatalf("building payloads: %v", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(payloads.IPXEScript, want) {
					t.Errorf("%s iPXE script should set %q, got:\n%s", tc.arch, want, payloads.IPXEScript)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(payloads.IPXEScript, absent) {
					t.Errorf("%s iPXE script should not carry %q, got:\n%s", tc.arch, absent, payloads.IPXEScript)
				}
			}
		})
	}
}

// TestBuildForgeNodePayloadsAptSuiteFollowsTheImage covers the other blocker:
// NKE publishes a suite per Ubuntu release, so a 26.04 image installing from
// the 24.04 suite finds no packages.
func TestBuildForgeNodePayloadsAptSuiteFollowsTheImage(t *testing.T) {
	for _, tc := range []struct{ arch, url, want, wantAbsent string }{
		{"amd64", "https://cdn.invalid/nke-node-non-fde-ubuntu-24.04.1-amd64.qcow2", "nke-noble", "resolute"},
		{"arm64", "https://cdn.invalid/nke-node-non-fde-ubuntu-26.04.1-arm64.qcow2", "nke-resolute", "noble"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			inputs := completeForgeInputs()
			inputs.Arch = tc.arch
			inputs.BootLoaderURL = "https://cdn.invalid/qcow-imager.efi"
			inputs.ImageURL = tc.url
			payloads, err := BuildForgeNodePayloads(inputs)
			if err != nil {
				t.Fatalf("building payloads: %v", err)
			}
			joined := strings.Join(payloads.PreKubeadmCommands, "\n")
			if !strings.Contains(joined, tc.want) {
				t.Errorf("should install from %q, got:\n%s", tc.want, joined)
			}
			if strings.Contains(joined, tc.wantAbsent) {
				t.Errorf("should not name %q, got:\n%s", tc.wantAbsent, joined)
			}
		})
	}
}

// An unrecognized release has to fail here rather than on the node, where it
// surfaces as a package that cannot be found minutes into a boot.
func TestBuildForgeNodePayloadsRejectsAnUnknownRelease(t *testing.T) {
	for _, tc := range []struct{ name, url, want string }{
		{"no release in the url", "https://cdn.invalid/nke-node.qcow2", "cannot tell which Ubuntu release"},
		{"a release with no suite", "https://cdn.invalid/nke-node-ubuntu-22.04-amd64.qcow2", "no NKE apt suite is known for Ubuntu 22.04"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inputs := completeForgeInputs()
			inputs.ImageURL = tc.url
			_, err := BuildForgeNodePayloads(inputs)
			if err == nil {
				t.Fatal("should refuse to render an apt source for an unknown release")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error should say %q, got %v", tc.want, err)
			}
		})
	}
}

// An explicit suite is for an image URL that names no release, so it has to
// win over the derivation rather than be checked against it.
func TestBuildForgeNodePayloadsExplicitAptSuiteWins(t *testing.T) {
	inputs := completeForgeInputs()
	inputs.ImageURL = "https://cdn.invalid/some-internal-build.qcow2"
	inputs.AptSuite = "resolute"
	payloads, err := BuildForgeNodePayloads(inputs)
	if err != nil {
		t.Fatalf("building payloads: %v", err)
	}
	if joined := strings.Join(payloads.PreKubeadmCommands, "\n"); !strings.Contains(joined, "nke-resolute") {
		t.Errorf("should install from the stated suite, got:\n%s", joined)
	}
}

// The repository can be pointed at an arm64 site while the OS image variables
// still name amd64, which is a one-variable mistake that otherwise surfaces as
// a machine that boots an image its firmware cannot execute.
func TestBuildForgeNodePayloadsRejectsAnArchMismatch(t *testing.T) {
	for _, tc := range []struct{ name, arch, image, loader, want string }{
		{
			name:  "arm64 node, amd64 image",
			arch:  "arm64",
			image: "https://cdn.invalid/releases/v1/amd64/nke-node-non-fde-ubuntu-26.04.1-amd64.qcow2",
			want:  "the node is arm64 but its OS image names amd64",
		},
		{
			name:   "arm64 node, x86_64 imager",
			arch:   "arm64",
			image:  "https://cdn.invalid/releases/v1/arm64/nke-node-non-fde-ubuntu-26.04.1-arm64.qcow2",
			loader: "https://cdn.invalid/efi/x86_64/qcow-imager.efi",
			want:   "the node is arm64 but its boot loader names x86_64",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inputs := completeForgeInputs()
			inputs.Arch = tc.arch
			inputs.ImageURL = tc.image
			if tc.loader != "" {
				inputs.BootLoaderURL = tc.loader
			}
			_, err := BuildForgeNodePayloads(inputs)
			if err == nil {
				t.Fatal("should refuse an artifact built for another machine")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error should say %q, got %v", tc.want, err)
			}
		})
	}

	// A matching pair, and a URL naming no architecture, both pass.
	for _, tc := range []struct{ name, image, loader string }{
		{"matching arm64", "https://cdn.invalid/arm64/nke-node-ubuntu-26.04.1-arm64.qcow2", "https://cdn.invalid/efi/aarch64/qcow-imager.efi"},
		{"silent about arch", "https://cdn.invalid/nke-node-ubuntu-26.04.1.qcow2", "https://cdn.invalid/qcow-imager.efi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inputs := completeForgeInputs()
			inputs.Arch = "arm64"
			inputs.ImageURL, inputs.BootLoaderURL = tc.image, tc.loader
			if _, err := BuildForgeNodePayloads(inputs); err != nil {
				t.Errorf("should accept: %v", err)
			}
		})
	}
}

// A mixed cluster gives each role the payloads for its own machine. Sharing
// one set would boot half the nodes on an image their firmware cannot execute.
func TestForgeRequestGivesEachRoleItsOwnPayloads(t *testing.T) {
	build := func(arch string) NodePayloads {
		t.Helper()
		inputs := completeForgeInputs()
		inputs.Arch = arch
		inputs.ImageURL = "https://cdn.invalid/nke-node-non-fde-ubuntu-26.04.1-" + arch + ".qcow2"
		inputs.BootLoaderURL = "https://cdn.invalid/qcow-imager.efi"
		payloads, err := BuildForgeNodePayloads(inputs)
		if err != nil {
			t.Fatalf("building %s payloads: %v", arch, err)
		}
		return payloads
	}
	controlPlane, worker := build("amd64"), build("arm64")

	if !strings.Contains(controlPlane.IPXEScript, "intel_iommu=on") {
		t.Error("the amd64 control plane should keep the x86 kernel parameters")
	}
	if !strings.Contains(worker.IPXEScript, "console=ttyAMA0") || strings.Contains(worker.IPXEScript, "intel_iommu") {
		t.Error("the arm64 worker should get the Grace boot parameters")
	}
	if controlPlane.IPXEScript == worker.IPXEScript {
		t.Fatal("the two roles should not share one iPXE script")
	}
}

// The ytl GB200 instances set no luks-key, so neither does the arm64 script.
// The amd64 one keeps it, which is what has been booting az61.
func TestBuildForgeNodePayloadsLuksKeyFollowsTheArch(t *testing.T) {
	for _, tc := range []struct {
		arch string
		want bool
	}{{"amd64", true}, {"arm64", false}} {
		t.Run(tc.arch, func(t *testing.T) {
			inputs := completeForgeInputs()
			inputs.Arch = tc.arch
			inputs.ImageURL = "https://cdn.invalid/nke-node-non-fde-ubuntu-26.04.1-" + tc.arch + ".qcow2"
			inputs.BootLoaderURL = "https://cdn.invalid/qcow-imager.efi"
			payloads, err := BuildForgeNodePayloads(inputs)
			if err != nil {
				t.Fatalf("building payloads: %v", err)
			}
			if got := strings.Contains(payloads.IPXEScript, "luks.key="); got != tc.want {
				t.Errorf("luks key present = %v, want %v, in:\n%s", got, tc.want, payloads.IPXEScript)
			}
		})
	}
}

// cloud-init reports the exit status of the last command in runcmd, so without
// a guard a failed apt step still ends with the success sentinel and the node
// reports clean while missing what it needs to join. The site node guards for
// the same reason; this asserts the tenant path does too, and that the guard
// is first, since anything above it runs unguarded.
func TestBuildForgeNodePayloadsGuardsAgainstAFailedStep(t *testing.T) {
	payloads, err := BuildForgeNodePayloads(completeForgeInputs())
	if err != nil {
		t.Fatalf("building payloads: %v", err)
	}
	if len(payloads.PreKubeadmCommands) == 0 {
		t.Fatal("no pre-kubeadm commands")
	}
	if got := payloads.PreKubeadmCommands[0]; got != "set -eu" {
		t.Errorf("first pre-kubeadm command is %q, want \"set -eu\": anything before it runs unguarded", got)
	}
}

// The image builds nke-agent from this checkout's agent/ source at bake time,
// so the installed agent is the build under qualification. Installing the apt
// repository's unpinned nke-agent over it would swap that for whichever build
// the suite currently holds, and the run would still report the baked version.
//
// Asserted rather than left to dpkg. A bake version sorts above a release tag
// today, so apt declines the swap, which makes the bug invisible until the
// repository publishes something that sorts higher.
func TestBuildForgeNodePayloadsDoesNotInstallNkeAgent(t *testing.T) {
	payloads, err := BuildForgeNodePayloads(completeForgeInputs())
	if err != nil {
		t.Fatalf("building payloads: %v", err)
	}
	for _, command := range payloads.PreKubeadmCommands {
		if !strings.Contains(command, "apt-get install") {
			continue
		}
		for _, field := range strings.Fields(command) {
			if field == "nke-agent" {
				t.Errorf("the node's own nke-agent should be left alone, got:\n%s", command)
			}
		}
	}
}

// The NKE OS image pre-installs spire-agent and owns its units, its first-boot
// renderer and its TPM plugin checksum. Installing the apt repository's build
// over that swaps an image-owned package for a generic one, so the pre-kubeadm
// commands must not mention it at all.
func TestBuildForgeNodePayloadsDoesNotInstallSpireAgent(t *testing.T) {
	payloads, err := BuildForgeNodePayloads(completeForgeInputs())
	if err != nil {
		t.Fatalf("building payloads: %v", err)
	}
	joined := strings.Join(payloads.PreKubeadmCommands, "\n")
	if strings.Contains(joined, "spire-agent") {
		t.Errorf("the node's own spire-agent should be left alone, got:\n%s", joined)
	}
	if strings.Contains(joined, "--allow-downgrades") {
		t.Error("nothing here should need a downgrade")
	}
}
