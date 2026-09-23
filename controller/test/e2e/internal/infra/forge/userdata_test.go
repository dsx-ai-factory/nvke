// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func completeTools() HostTools {
	return HostTools{NerdctlVersion: "2.3.5", KindVersion: "v0.32.0", ClusterctlVersion: "v1.12.3"}
}

func completeUserData() userDataInputs {
	return userDataInputs{
		Hostname:      "e2e-site-1",
		AuthorizedKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample",
		Arch:          "amd64",
		NodeCommands:  []string{`printf 'AGENT_ARGS="--secure-server-port 15005"\n' > /etc/default/nke-agent`, "touch /run/cloud-init.done"},
		Tools:         completeTools(),
	}
}

func TestRenderUserData(t *testing.T) {
	in := completeUserData()
	out, err := renderUserData(in)
	if err != nil {
		t.Fatalf("rendering: %v", err)
	}
	if !strings.HasPrefix(out, "#cloud-config\n") {
		t.Fatalf("cloud-init only reads a #cloud-config document, got %q", strings.SplitN(out, "\n", 2)[0])
	}

	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("user data is not YAML: %v", err)
	}
	// Either block would break the node: users shadows the baked account,
	// phone_home is what Forge rewrites into a degraded cloud-init.
	for _, forbidden := range []string{"users", "phone_home", "ssh_pwauth"} {
		if _, ok := doc[forbidden]; ok {
			t.Errorf("user data must not carry %s", forbidden)
		}
	}

	var cfg cloudConfig
	if err := yaml.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if cfg.Hostname != in.Hostname {
		t.Errorf("hostname = %q, want %q", cfg.Hostname, in.Hostname)
	}
	var key *writeFile
	for i := range cfg.WriteFiles {
		if strings.HasSuffix(cfg.WriteFiles[i].Path, "/.ssh/authorized_keys") {
			key = &cfg.WriteFiles[i]
		}
	}
	if key == nil {
		t.Fatal("user data writes no authorized key")
	}
	if !key.Defer {
		t.Error("the authorized key must be deferred until the account exists")
	}
	if strings.TrimSpace(key.Content) != in.AuthorizedKey {
		t.Errorf("authorized key = %q, want %q", key.Content, in.AuthorizedKey)
	}

	// A node command's quoting must survive marshaling byte for byte.
	index := func(cmd string) int {
		for i, c := range cfg.RunCmd {
			if c == cmd {
				return i
			}
		}
		return -1
	}
	agent, network := index(in.NodeCommands[0]), index("nerdctl network create kind --subnet 10.4.1.0/24 --gateway 10.4.1.1 || true")
	if agent < 0 {
		t.Fatalf("node command lost or altered in marshaling: %q", in.NodeCommands[0])
	}
	if network < agent {
		t.Error("the kind network has to be created after the node stack is installed")
	}
	if index("systemctl enable --now containerd") < 0 {
		t.Error("containerd is never started")
	}

	joined := strings.Join(cfg.RunCmd, "\n")
	for _, want := range []string{
		"nerdctl-full-2.3.5-linux-amd64.tar.gz",
		"kind.sigs.k8s.io/dl/v0.32.0/kind-linux-amd64",
		"cluster-api/releases/download/v1.12.3/clusterctl-linux-amd64",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("run commands should fetch %s", want)
		}
	}
	// nke-kubeadm-distro conflicts with Ubuntu's containerd package.
	if strings.Contains(joined, "apt-get install -y containerd") || strings.Contains(joined, "apt install containerd") {
		t.Error("run commands must not install Ubuntu's containerd")
	}
}

func TestRenderUserDataRejectsIncompleteInputs(t *testing.T) {
	for name, mutate := range map[string]func(*userDataInputs){
		"no key":           func(in *userDataInputs) { in.AuthorizedKey = " " },
		"no node commands": func(in *userDataInputs) { in.NodeCommands = nil },
		"no kind version":  func(in *userDataInputs) { in.Tools.KindVersion = "" },
	} {
		t.Run(name, func(t *testing.T) {
			in := completeUserData()
			mutate(&in)
			if _, err := renderUserData(in); err == nil {
				t.Error("expected an error")
			}
		})
	}
}
