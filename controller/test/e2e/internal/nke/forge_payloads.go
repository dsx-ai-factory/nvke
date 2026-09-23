// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nke

import (
	_ "embed"
	"fmt"
	"strings"
	"text/template"
)

var (
	//go:embed nico_forge_ipxe.tmpl
	nicoForgeIPXETmpl string
	//go:embed nico_forge_prekubeadm.tmpl
	nicoForgePreKubeadmTmpl string
)

// ForgeNodeInputs is everything a Forge node needs that a qualification run
// decides rather than the site. Each field names something under test, which
// is why none of them has a default: a run that did not say which OS image to
// boot has not said what it is qualifying.
type ForgeNodeInputs struct {
	// BootLoaderURL and ImageURL are the qcow imager and the OS image it
	// writes. They are separate because the imager is versioned on its own and
	// neither URL can be derived from the other.
	BootLoaderURL string
	ImageURL      string

	// KubernetesVersion selects the nke-kubeadm-distro package, e.g. "1.34.2".
	KubernetesVersion string
	// CNIVersion selects the nke-cni-ovn-images package, e.g. "1.4.0".
	CNIVersion string
	// SpireVersion selects the spire-agent package, e.g. "1.14.1+tpm1.9.0".
	SpireVersion string

	// AgentArgs overrides the node agent's arguments. Empty means the port
	// CWE dials and nothing else. Whether the agent runs --disable-mtls has to
	// agree with how the dialing side was deployed, and a mismatch fails as
	// an opaque TLS handshake error, so a caller that decides the one decides
	// the other here.
	AgentArgs string
}

const (
	defaultAgentArgs = "--secure-server-port 15005"
)

func (i ForgeNodeInputs) validate() error {
	missing := make([]string, 0, 5)
	for _, field := range []struct {
		name  string
		value string
	}{
		{"BootLoaderURL", i.BootLoaderURL},
		{"ImageURL", i.ImageURL},
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
func BuildForgeNodePayloads(inputs ForgeNodeInputs) (NodePayloads, error) {
	if err := inputs.validate(); err != nil {
		return NodePayloads{}, err
	}
	if strings.TrimSpace(inputs.AgentArgs) == "" {
		inputs.AgentArgs = defaultAgentArgs
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
