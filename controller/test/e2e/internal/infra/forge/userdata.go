// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"
)

const (
	// defaultNodeUser is the account the OSS os-image releases bake, from
	// os-image/packer/cidata/autoinstall-lvm.pkrtpl. The internal image
	// pipeline bakes dgxc-admin instead, which is why this is an input
	// rather than a constant: a run against the wrong one gets no
	// authorized_keys and fails as a publickey rejection twenty minutes in.
	//
	// Cloud-init must not declare the account either way: a users: block
	// would shadow the baked one and lock everyone out of the node
	// bootstrap has to run on.
	defaultNodeUser = "ubuntu"
)

// HostTools are the programs `nkectl bootstrap site` needs on the host it
// runs on, which the NKE image deliberately does not ship. Each version is an
// input because the repository pins it: KindVersion and ClusterctlVersion in
// make/tool-versions.mk, NerdctlVersion in nkectl-bootstrap-e2e.yml.
type HostTools struct {
	// NerdctlVersion is nerdctl-full without the leading v. nkectl drives kind
	// through nerdctl; Docker is not supported.
	NerdctlVersion string
	// KindVersion carries the leading v, as kind's release URLs do.
	KindVersion string
	// ClusterctlVersion must match the CAPI version nkectl was built with.
	// bootstrap site shells out to it, and nothing documents that it must be
	// present: without it the run dies with "clusterctl: executable file not
	// found".
	ClusterctlVersion string
}

func (t HostTools) validate() error {
	missing := make([]string, 0, 3)
	for _, field := range []struct{ name, value string }{
		{"NerdctlVersion", t.NerdctlVersion},
		{"KindVersion", t.KindVersion},
		{"ClusterctlVersion", t.ClusterctlVersion},
	} {
		if strings.TrimSpace(field.value) == "" {
			missing = append(missing, field.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("host tools are incomplete: %s", strings.Join(missing, ", "))
	}
	return nil
}

// userDataInputs is everything the bootstrap node's cloud-init is rendered
// from.
type userDataInputs struct {
	Hostname      string
	AuthorizedKey string
	Arch          string
	// NodeUser is the account the image bakes. Empty is defaultNodeUser.
	NodeUser string
	// NodeCommands install the agent stack, shared with tenant nodes so the
	// package list exists once.
	NodeCommands []string
	Tools        HostTools
}

type writeFile struct {
	Path        string `json:"path"`
	Owner       string `json:"owner,omitempty"`
	Permissions string `json:"permissions,omitempty"`
	Defer       bool   `json:"defer,omitempty"`
	Content     string `json:"content"`
}

type cloudConfig struct {
	Hostname         string      `json:"hostname"`
	PreserveHostname bool        `json:"preserve_hostname"`
	ManageEtcHosts   bool        `json:"manage_etc_hosts"`
	WriteFiles       []writeFile `json:"write_files"`
	RunCmd           []string    `json:"runcmd"`
}

// renderUserData builds the cloud-init for a node `nkectl bootstrap site`
// runs on. It is marshaled rather than templated: its commands are shell,
// full of quotes and redirections, and YAML written by hand around them is
// how a boot payload ends up subtly different from what was reviewed.
//
// It carries no phone_home block. The instance is created with phone-home
// disabled, and Forge rewrites that block in a way that leaves cloud-init
// reporting warnings, which node preflight reads as failure.
func renderUserData(in userDataInputs) (string, error) {
	if err := in.Tools.validate(); err != nil {
		return "", err
	}
	if strings.TrimSpace(in.AuthorizedKey) == "" {
		return "", fmt.Errorf("no authorized key: the session has no other way into the node")
	}
	if len(in.NodeCommands) == 0 {
		return "", fmt.Errorf("no node commands: the agent stack would never be installed")
	}
	nodeUser := in.NodeUser
	if strings.TrimSpace(nodeUser) == "" {
		nodeUser = defaultNodeUser
	}

	run := make([]string, 0, len(in.NodeCommands)+13)

	run = append(run,
		// cloud-init runs these as one /bin/sh script and reports the exit
		// status of its last command, and the last one here ends in
		// `|| true`. Without this the script always succeeds: a timed-out
		// route wait, a failed apt step or a missing clusterctl all let
		// `cloud-init status --wait` pass a node that is not usable, and the
		// run dies much later inside bootstrap instead. `set -u` is safe
		// because every command below is fully rendered before it is sent --
		// nothing here reads a shell variable.
		"set -eu",

		// BlueField hosts race cloud-init for the network, and apt fails
		// without one. Bounded, so a node with no route fails here rather
		// than silently.
		`timeout 300s sh -c 'until ip -4 route list default | grep -q .; do echo "waiting for an IPv4 default route"; sleep 5; done'`,

		// The key is written deferred, after the account exists, into a
		// directory write_files creates as root. sshd refuses keys in a
		// directory the user does not own.
		fmt.Sprintf("install -d -m 0700 -o %[1]s -g %[1]s /home/%[1]s/.ssh", nodeUser),
		fmt.Sprintf("chown %[1]s:%[1]s /home/%[1]s/.ssh/authorized_keys", nodeUser),
	)

	run = append(run, in.NodeCommands...)

	run = append(run,
		// The site node runs a kind cluster and then the whole NKE stack on
		// one machine, and Ubuntu's default of 128 inotify instances is not
		// enough for that many controllers: the 2026-09-29 ytl bootstrap had
		// to be raised to 1024 by hand before it would complete. Written as a
		// drop-in rather than set live, so it survives the reboots the
		// bootstrap performs.
		//
		// Only the site node. A tenant node runs a kubelet and its workload,
		// which is the ordinary load this default was chosen for, and no
		// tenant has been observed to exhaust it.
		"printf 'fs.inotify.max_user_instances = 1024\n' > /etc/sysctl.d/99-nke-e2e-inotify.conf",
		"sysctl --system",

		// nke-kubeadm-distro ships a containerd and declares Conflicts: runc,
		// so installing Ubuntu's containerd aborts the whole apt transaction.
		// Start the one already there.
		"systemctl enable --now containerd",

		// nerdctl-full carries the CNI plugins and buildkit kind needs. Rootful:
		// bootstrap adds an iptables rule and a bridge address.
		fmt.Sprintf("curl -fsSL -o /tmp/nerdctl.tgz https://github.com/containerd/nerdctl/releases/download/v%[1]s/nerdctl-full-%[1]s-linux-%[2]s.tar.gz", in.Tools.NerdctlVersion, in.Arch),
		"tar -C /usr/local -xzf /tmp/nerdctl.tgz",
		"systemctl enable --now buildkit || true",

		fmt.Sprintf("curl -fsSL -o /usr/local/bin/kind https://kind.sigs.k8s.io/dl/%s/kind-linux-%s", in.Tools.KindVersion, in.Arch),
		"chmod 0755 /usr/local/bin/kind",

		fmt.Sprintf("curl -fsSL -o /usr/local/bin/clusterctl https://github.com/kubernetes-sigs/cluster-api/releases/download/%s/clusterctl-linux-%s", in.Tools.ClusterctlVersion, in.Arch),
		"chmod 0755 /usr/local/bin/clusterctl",

		// kind's default network is dual-stack and the NKE image boots
		// ipv6.disable=1, so the bridge CNI fails and no container starts.
		// kind reuses a network that already exists.
		"nerdctl network create kind --subnet 10.4.1.0/24 --gateway 10.4.1.1 || true",
	)

	cfg := cloudConfig{
		Hostname:         in.Hostname,
		PreserveHostname: false,
		ManageEtcHosts:   true,
		WriteFiles: []writeFile{
			{
				// bootstrap site runs as root, over a session logged in as
				// the baked account.
				Path:        "/etc/sudoers.d/" + nodeUser,
				Owner:       "root:root",
				Permissions: "0440",
				Content:     nodeUser + " ALL=(ALL) NOPASSWD:ALL\n",
			},
			{
				// A per-run key rather than the image's shared password. The
				// instance's SSH key group does not do this: it governs
				// Serial-over-LAN console access, not authorized_keys.
				Path:        "/home/" + nodeUser + "/.ssh/authorized_keys",
				Permissions: "0600",
				Defer:       true,
				Content:     strings.TrimSpace(in.AuthorizedKey) + "\n",
			},
		},
		RunCmd: run,
	}

	body, err := yaml.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("marshaling cloud-init: %w", err)
	}
	return "#cloud-config\n" + string(body), nil
}
