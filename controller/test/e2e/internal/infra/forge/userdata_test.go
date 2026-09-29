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

	// First, not merely present: cloud-init reports the exit status of the
	// script's last command, and the last one is a `|| true`. Anything above
	// this line runs unguarded, so a failure there would still be reported as
	// a node that came up clean.
	if len(cfg.RunCmd) == 0 || cfg.RunCmd[0] != "set -eu" {
		t.Error("run commands must begin with set -eu so a failed step fails cloud-init")
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

// TestRenderUserDataNodeUser covers the account the image bakes reaching every
// place cloud-init writes it. Run 36540138816 failed as a publickey rejection
// twenty minutes into provisioning because the harness assumed dgxc-admin and
// the os-image release baked ubuntu, so the key went to a home directory for
// an account that did not exist.
func TestRenderUserDataNodeUser(t *testing.T) {
	for _, tc := range []struct{ name, set, want string }{
		{"empty is the os-image default", "", defaultNodeUser},
		{"an internal-pipeline image", "dgxc-admin", "dgxc-admin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := completeUserData()
			in.NodeUser = tc.set
			out, err := renderUserData(in)
			if err != nil {
				t.Fatalf("rendering: %v", err)
			}
			var cfg cloudConfig
			if err := yaml.Unmarshal([]byte(out), &cfg); err != nil {
				t.Fatalf("decoding: %v", err)
			}

			var sawKey, sawSudoers bool
			for _, f := range cfg.WriteFiles {
				switch f.Path {
				case "/home/" + tc.want + "/.ssh/authorized_keys":
					sawKey = true
				case "/etc/sudoers.d/" + tc.want:
					sawSudoers = true
					if !strings.HasPrefix(f.Content, tc.want+" ") {
						t.Errorf("sudoers grants %q, want %q", f.Content, tc.want)
					}
				}
			}
			if !sawKey {
				t.Errorf("no authorized_keys under /home/%s", tc.want)
			}
			if !sawSudoers {
				t.Errorf("no sudoers drop-in for %s", tc.want)
			}

			// The runcmd that creates .ssh has to name the same account, or
			// the deferred key write lands in a directory sshd will refuse.
			var chowned bool
			for _, cmd := range cfg.RunCmd {
				if strings.Contains(cmd, "/home/"+tc.want+"/.ssh") {
					chowned = true
				}
				if tc.want != "dgxc-admin" && strings.Contains(cmd, "dgxc-admin") {
					t.Errorf("runcmd still names dgxc-admin: %s", cmd)
				}
			}
			if !chowned {
				t.Errorf("no runcmd prepares /home/%s/.ssh", tc.want)
			}
		})
	}
}

// The site node runs kind plus the whole stack on one machine, and the
// 2026-09-29 ytl bootstrap needed this raised by hand before it completed.
func TestRenderUserDataRaisesInotifyInstances(t *testing.T) {
	out, err := renderUserData(completeUserData())
	if err != nil {
		t.Fatalf("rendering: %v", err)
	}
	for _, want := range []string{"fs.inotify.max_user_instances = 1024", "/etc/sysctl.d/", "sysctl --system"} {
		if !strings.Contains(out, want) {
			t.Errorf("user data should carry %q", want)
		}
	}
}
