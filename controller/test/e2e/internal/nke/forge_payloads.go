// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nke

import (
	_ "embed"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"text/template"
)

var (
	//go:embed nico_forge_ipxe.tmpl
	nicoForgeIPXETmpl string
	//go:embed nico_forge_prekubeadm.tmpl
	nicoForgePreKubeadmTmpl string
)

// NodeInputs is everything building an NKE node from an image needs that a
// qualification run decides rather than the site. None of these fields is
// specific to a provider -- they describe any such node -- which is why the
// type is not named for one. What is provider-specific is the rendering, and
// that lives in BuildForgeNodePayloads below.
//
// Each field names something under test, which is why none of them has a
// default: a run that did not say which OS image to boot has not said what it
// is qualifying.
type NodeInputs struct {
	// BootLoaderURL and ImageURL are the qcow imager and the OS image it
	// writes. They are separate because the imager is versioned on its own and
	// neither URL can be derived from the other.
	BootLoaderURL string
	ImageURL      string

	// Arch is the machine's architecture, "amd64" or "arm64". It selects the
	// console and kernel parameters the iPXE script sets, which differ by
	// architecture rather than by run. It comes from the instance type the
	// node is created as, so it cannot disagree with the hardware.
	Arch string

	// KubernetesVersion selects the nke-kubeadm-distro package, e.g. "1.34.2".
	KubernetesVersion string
	// CNIVersion selects the nke-cni-ovn-images package, e.g. "1.4.0".
	CNIVersion string
	// SpireVersion selects the spire-agent package, e.g. "1.14.1+tpm1.9.0".
	SpireVersion string

	// AptSuite is the NKE apt suite the node installs its packages from, one
	// per Ubuntu release. Empty derives it from ImageURL, which is where the
	// answer already is: the suite has to match the release the image was
	// built from, and a separately supplied one is just an opportunity for
	// the two to disagree. Set it only when the URL does not name a release.
	AptSuite string

	// AgentArgs overrides the node agent's arguments. Empty means the port
	// CWE dials and nothing else. Whether the agent runs --disable-mtls has to
	// agree with how the dialing side was deployed, and a mismatch fails as
	// an opaque TLS handshake error, so a caller that decides the one decides
	// the other here.
	AgentArgs string
}

const (
	defaultAgentArgs = "--secure-server-port 15005"

	// AgentArgsNoMTLS is the no-SPIRE path, and has to be paired with
	// `nkectl bootstrap site --disable-mtls`. On the default args the agent
	// expects a SPIFFE identity, which it can only get if the site spec
	// carries the node's TPM EK thumbprint -- without one nkectl deploys no
	// TPM attestor, the agent never attests, and the cluster sits in
	// Provisioning until the timeout rather than reporting anything.
	//
	// os-image/SPEC.md does not list this as a supported production mode. It
	// is how a site gets built today: the SBOM procedure bootstraps this way
	// and turns SPIRE and TPM on afterwards, in the management-plane install.
	AgentArgsNoMTLS = defaultAgentArgs + " --disable-mtls"
)

func (i NodeInputs) validate() error {
	missing := make([]string, 0, 5)
	for _, field := range []struct {
		name  string
		value string
	}{
		{"BootLoaderURL", i.BootLoaderURL},
		{"ImageURL", i.ImageURL},
		{"Arch", i.Arch},
		{"KubernetesVersion", i.KubernetesVersion},
		{"CNIVersion", i.CNIVersion},
		{"SpireVersion", i.SpireVersion},
	} {
		if strings.TrimSpace(field.value) == "" {
			missing = append(missing, field.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("forge node inputs are incomplete: %s", strings.Join(missing, ", "))
	}
	return nil
}

// BuildForgeNodePayloads renders the iPXE script and the pre-kubeadm commands
// a Forge node boots and installs with.
func BuildForgeNodePayloads(inputs NodeInputs) (NodePayloads, error) {
	if err := inputs.validate(); err != nil {
		return NodePayloads{}, err
	}
	if strings.TrimSpace(inputs.AgentArgs) == "" {
		inputs.AgentArgs = defaultAgentArgs
	}
	if err := inputs.checkArch(); err != nil {
		return NodePayloads{}, err
	}
	if strings.TrimSpace(inputs.AptSuite) == "" {
		suite, err := aptSuiteFor(inputs.ImageURL)
		if err != nil {
			return NodePayloads{}, err
		}
		inputs.AptSuite = suite
	}

	ipxe, err := renderTemplate("nico_forge_ipxe", nicoForgeIPXETmpl, inputs)
	if err != nil {
		return NodePayloads{}, err
	}
	preKubeadm, err := renderTemplate("nico_forge_prekubeadm", nicoForgePreKubeadmTmpl, inputs)
	if err != nil {
		return NodePayloads{}, err
	}

	// The commands template is a line per command, which keeps it readable as
	// the shell it becomes. Blank lines are layout, not commands.
	commands := make([]string, 0, 16)
	for _, line := range strings.Split(preKubeadm, "\n") {
		if strings.TrimSpace(line) != "" {
			commands = append(commands, line)
		}
	}

	return NodePayloads{IPXEScript: ipxe, PreKubeadmCommands: commands}, nil
}

// archInURL and bootLoaderArchInURL match the architecture an artifact names.
// The two artifacts spell it differently: the OS image uses the Go/Debian
// names and the EFI imager uses the uname ones.
var (
	archInURL           = regexp.MustCompile(`\b(amd64|arm64)\b`)
	bootLoaderArchInURL = regexp.MustCompile(`\b(x86_64|aarch64)\b`)
)

// unameArch is the imager's spelling of each architecture.
var unameArch = map[string]string{"amd64": "x86_64", "arm64": "aarch64"}

// checkArch refuses an image or imager built for a different machine than the
// one being created.
//
// The URLs come from -e2e.versions and the architecture from the instance type,
// so nothing upstream forces them to agree: pointing a run at an arm64 site
// while leaving the amd64 image URL in place is a one-variable mistake. Caught
// here, it is a message naming both; left alone, it is a machine that boots an
// image its firmware cannot execute and reports nothing.
//
// A URL naming no architecture is accepted. Only a stated disagreement is an
// error, because an internal build may follow neither convention.
func (i NodeInputs) checkArch() error {
	for _, artifact := range []struct {
		what, url, want string
		pattern         *regexp.Regexp
	}{
		{"OS image", i.ImageURL, i.Arch, archInURL},
		{"boot loader", i.BootLoaderURL, unameArch[i.Arch], bootLoaderArchInURL},
	} {
		if artifact.want == "" {
			continue
		}
		for _, found := range artifact.pattern.FindAllString(artifact.url, -1) {
			if found != artifact.want {
				return fmt.Errorf("the node is %s but its %s names %s: %s", i.Arch, artifact.what, found, artifact.url)
			}
		}
	}
	return nil
}

// ubuntuReleaseInURL matches the Ubuntu release in an OS image filename, as in
// nke-node-non-fde-ubuntu-26.04.1-arm64.qcow2. The point release is optional
// and ignored: NKE publishes a suite per Ubuntu release, not per point.
var ubuntuReleaseInURL = regexp.MustCompile(`ubuntu-(\d+\.\d+)`)

// aptSuites maps an Ubuntu release to the NKE apt suite built for it. Adding a
// release means publishing its suite first; see
// agent/scripts/upload-s3-apt-intake.sh, whose default list is these two.
var aptSuites = map[string]string{
	"24.04": "noble",
	"26.04": "resolute",
}

// aptSuiteFor reads the suite out of the image URL rather than guessing one.
// An unknown release is an error and not a fallback: installing from the wrong
// suite fails as a package that cannot be found, minutes into a boot, with
// nothing naming the release as the cause.
func aptSuiteFor(imageURL string) (string, error) {
	match := ubuntuReleaseInURL.FindStringSubmatch(imageURL)
	if match == nil {
		return "", fmt.Errorf("cannot tell which Ubuntu release %q is, so cannot pick an apt suite: set NodeInputs.AptSuite", imageURL)
	}
	suite, ok := aptSuites[match[1]]
	if !ok {
		known := make([]string, 0, len(aptSuites))
		for release := range aptSuites {
			known = append(known, release)
		}
		sort.Strings(known)
		return "", fmt.Errorf("no NKE apt suite is known for Ubuntu %s (only %s); publish one and add it to aptSuites", match[1], strings.Join(known, ", "))
	}
	return suite, nil
}

func renderTemplate(name, text string, data any) (string, error) {
	// Missing keys are an error rather than "<no value>" in a boot script,
	// where the failure would otherwise surface as a node that never comes up.
	tmpl, err := template.New(name).Option("missingkey=error").Parse(text)
	if err != nil {
		return "", fmt.Errorf("parsing %s template: %w", name, err)
	}
	var out strings.Builder
	if err := tmpl.Execute(&out, data); err != nil {
		return "", fmt.Errorf("rendering %s template: %w", name, err)
	}
	return out.String(), nil
}
