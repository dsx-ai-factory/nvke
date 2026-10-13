//go:build kubeadm_bringup

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package cloudinit renders the per-VM cloud-init template at
// ../cloud-init/user-data.yml.tmpl by substituting __KEY__ placeholders.
//
// We deliberately use the same sed-style placeholder convention as
// tests/smoke/vm/cloud-init/user-data.yml.tmpl (kubeadm Phase 2) and the
// preflight-1vm.sh script — so the template renders identically whether
// it's driven from bash or Go, and the same .tmpl file is reused by both.
package cloudinit

import (
	"fmt"
	"os"
	"strings"
)

// Vars holds the substitution values for one cloud-init render.
//
// The zero value is invalid — Validate() must pass before Render().
// Required fields match the placeholder list in
// cloud-init/user-data.yml.tmpl's header comment.
type Vars struct {
	Hostname     string // __HOSTNAME__
	PasswordHash string // __PASSWORD_HASH__   (output of `openssl passwd -6 ...`)

	// K8sProvision is the runcmd block that puts kubeadm, the kubelet and a
	// container runtime on the node, already indented as YAML list items.
	// AptProvision() builds the NVKE apt repository form; CommunityProvision()
	// builds the upstream form for hosts that cannot reach that repository.
	K8sProvision string // __K8S_PROVISION__
}

// Validate returns an error listing any required fields that are empty.
func (v Vars) Validate() error {
	var missing []string
	check := func(name, val string) {
		if val == "" {
			missing = append(missing, name)
		}
	}
	check("Hostname", v.Hostname)
	check("PasswordHash", v.PasswordHash)
	check("K8sProvision", v.K8sProvision)
	if len(missing) > 0 {
		return fmt.Errorf("cloudinit: required Vars fields empty: %s", strings.Join(missing, ", "))
	}
	return nil
}

// Render reads templatePath and returns the cloud-init body with all
// __KEY__ placeholders substituted. The replacer applies values in a single
// pass — substituted values are not themselves re-scanned for placeholders.
func Render(templatePath string, vars Vars) (string, error) {
	if err := vars.Validate(); err != nil {
		return "", err
	}
	raw, err := os.ReadFile(templatePath)
	if err != nil {
		return "", fmt.Errorf("cloudinit: read template %s: %w", templatePath, err)
	}
	replacer := strings.NewReplacer(
		"__HOSTNAME__", vars.Hostname,
		"__PASSWORD_HASH__", vars.PasswordHash,
		"__K8S_PROVISION__", vars.K8sProvision,
	)
	out := replacer.Replace(string(raw))
	if strings.Contains(out, "__") {
		// Surface any unmapped placeholder rather than silently shipping a
		// VM that won't apt-install. Cheap guardrail; matches the sed-style
		// expectation that all placeholders must resolve.
		for _, line := range strings.Split(out, "\n") {
			if i := strings.Index(line, "__"); i >= 0 {
				j := strings.Index(line[i+2:], "__")
				if j >= 0 {
					return "", fmt.Errorf("cloudinit: unresolved placeholder in rendered template: %q", line[i:i+2+j+2])
				}
			}
		}
	}
	return out, nil
}

// AptProvision returns the runcmd block that installs the Kubernetes stack from
// the NVKE apt repository. This is what the lane used before, kept because it is
// the right path on a host that can reach that repository.
//
// The plain Ubuntu cloud image has no NVKE apt repo, so add one. The NVKE node
// image already ships it signed-by /etc/apt/keyrings/nke.asc, and a second entry
// with a different keyring makes apt fail with "Conflicting values set for
// option Signed-By", so only add ours when it is absent.
func AptProvision(repoURL, gpgURL, suite, component, deb, debVersion string) string {
	return "  - bash -c 'if grep -Rqs \"" + suite + "\" /etc/apt/sources.list.d/ /etc/apt/sources.list; then" +
		" echo \"NVKE apt repo already present in image; using it\"; else" +
		" install -d -m 0755 /etc/apt/keyrings && curl -fsSL \"" + gpgURL + "\" |" +
		" gpg --dearmor -o /etc/apt/keyrings/nke.gpg &&" +
		" echo \"deb [signed-by=/etc/apt/keyrings/nke.gpg] " + repoURL + " " + suite + " " + component + "\"" +
		" > /etc/apt/sources.list.d/nke.list; fi'\n" +
		"  - apt-get update -o DPkg::Lock::Timeout=600\n" +
		"  - DEBIAN_FRONTEND=noninteractive apt-get install -y -o DPkg::Lock::Timeout=600 " + deb + "=" + debVersion + " nvke-agent"
}

// CommunityProvision returns the runcmd block that installs the same stack from
// upstream releases, and nvke-agent from a .deb the host serves.
//
// The NVKE apt repository is behind a source-IP allow list that GitHub runners
// are outside of. Everything that repository's kubeadm package ships is an
// upstream release, and the agent resolves its tools with exec.LookPath, so
// fetching them directly gives the agent the same thing. Both URLs are served by
// the test host, which needs no NVIDIA network.
func CommunityProvision(provisionScriptURL, agentDebURL string) string {
	return "  - curl -fsSL --retry 3 -o /tmp/install-k8s.sh " + provisionScriptURL + "\n" +
		"  - bash /tmp/install-k8s.sh\n" +
		"  - curl -fsSL --retry 3 -o /tmp/nvke-agent.deb " + agentDebURL + "\n" +
		"  - DEBIAN_FRONTEND=noninteractive apt-get install -y -o DPkg::Lock::Timeout=600 /tmp/nvke-agent.deb"
}
