// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testSubnet = "10.231.7.0/24"
	nodeImage  = "kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5"
)

// kindArgs is the `run` command line of kind's nerdctl provider for a control-plane node.
func kindArgs(name string) []string {
	return []string{
		"run", "--name", name, "--hostname", name, "--label", "io.x-k8s.kind.role=control-plane", "--privileged",
		"--security-opt", "seccomp=unconfined", "--security-opt", "apparmor=unconfined", "--tmpfs", "/tmp", "--tmpfs", "/run",
		"--volume", "/var", "--volume", "/lib/modules:/lib/modules:ro", "-e", "KIND_EXPERIMENTAL_CONTAINERD_SNAPSHOTTER",
		"--detach", "--tty", "--label", "io.x-k8s.kind.cluster=c", "--net", "kind", "--restart=on-failure:1", "--init=false",
		"--sysctl=net.ipv6.conf.all.disable_ipv6=0", "--sysctl=net.ipv6.conf.all.forwarding=1",
		"--publish=127.0.0.1:34567:6443/tcp", "-e", "KUBECONFIG=/etc/kubernetes/admin.conf", nodeImage,
	}
}

func (h *harness) kindNetwork() {
	h.t.Helper()
	require.Equal(h.t, 0, h.run("network", "create", "--subnet", testSubnet, "kind"), h.stderr.String())
}

// callsMatching are the calls of the fakes that start with the prefix.
func (h *harness) callsMatching(prefix string) []string {
	h.t.Helper()
	var matching []string
	for _, call := range h.calls() {
		if strings.HasPrefix(call, prefix) {
			matching = append(matching, call)
		}
	}
	return matching
}

func TestRunKindNode(t *testing.T) {
	t.Setenv("KIND_EXPERIMENTAL_CONTAINERD_SNAPSHOTTER", "")
	require.NoError(t, os.Unsetenv("KIND_EXPERIMENTAL_CONTAINERD_SNAPSHOTTER"))
	h := newHarness(t)
	h.kindNetwork()
	h.setFile("images", "docker.io/kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5 application/vnd.oci.image.index.v1+json sha256:aaa 1.0_MiB linux/arm64 -\n")

	require.Equal(t, 0, h.run(kindArgs("c-control-plane")...), h.stderr.String())
	id := strings.TrimSpace(h.stdout.String())
	require.Len(t, id, 64)

	t.Run("the container ctr is asked for", func(t *testing.T) {
		creates := h.callsMatching("ctr --address /run/test.sock --namespace ns containers create")
		require.Len(t, creates, 1)
		got := creates[0]
		for _, want := range []string{
			"--env KUBECONFIG=/etc/kubernetes/admin.conf",
			"--label io.x-k8s.kind.role=control-plane",
			"--label io.x-k8s.kind.cluster=c",
			"--mount type=tmpfs,src=tmpfs,dst=/tmp,options=noexec:nosuid:nodev",
			"--mount type=tmpfs,src=tmpfs,dst=/run,options=noexec:nosuid:nodev",
			"--mount type=bind,src=" + filepath.Join(h.root, "containers", id, "volumes", "0") + ",dst=/var,options=rbind:rw",
			"--mount type=bind,src=/lib/modules,dst=/lib/modules,options=rbind:ro",
			"--mount type=cgroup,src=cgroup,dst=/sys/fs/cgroup,options=rw:nosuid:noexec:nodev:relatime",
			"--mount type=bind,src=" + filepath.Join(h.root, "containers", id, "hosts") + ",dst=/etc/hosts,options=rbind:rw",
			"--with-ns cgroup:",
			"--with-ns network:/run/netns/ctrd-" + id[:12],
			"--hostname c-control-plane",
			"--privileged",
			"--tty",
		} {
			assert.Contains(t, got, want)
		}
		assert.NotContains(t, got, "--net-host")
		assert.NotContains(t, got, "dst=/run,options=rbind", "a node asks for a tmpfs /run, and has no volume there")
		assert.True(t, strings.HasSuffix(got, " docker.io/kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5 "+id), got)
		assert.NotContains(t, got, "KIND_EXPERIMENTAL_CONTAINERD_SNAPSHOTTER", "a variable that is not set is not passed")
	})

	t.Run("the anonymous volume starts with what the image has there", func(t *testing.T) {
		assert.FileExists(t, filepath.Join(h.root, "containers", id, "volumes", "0", "lib", "dpkg", "status"))
		assert.NotEmpty(t, h.callsMatching("umount "))
	})

	t.Run("the network namespace, the sysctls and the plugins, in order", func(t *testing.T) {
		ns := "ctrd-" + id[:12]
		calls := h.calls()
		var after []string
		for _, call := range calls {
			if strings.HasPrefix(call, "ip ") || strings.Contains(call, " ADD ") || strings.HasPrefix(call, "supervisor") {
				after = append(after, call)
			}
		}
		netns := "netns=/run/netns/" + ns
		assert.Equal(t, []string{
			"ip netns del " + ns,
			"ip netns add " + ns,
			"ip netns exec " + ns + " sh -c echo \"$1\" > \"$2\" sh 0 /proc/sys/net/ipv6/conf/all/disable_ipv6",
			"ip netns exec " + ns + " sh -c echo \"$1\" > \"$2\" sh 1 /proc/sys/net/ipv6/conf/all/forwarding",
			"loopback ADD id=" + id + " ifname=lo " + netns,
			"bridge ADD id=" + id + " ifname=eth0 " + netns,
			"portmap ADD id=" + id + " ifname=eth0 " + netns,
			"firewall ADD id=" + id + " ifname=eth0 " + netns,
			"supervisor _supervise " + id,
		}, after)
	})

	t.Run("the plugins are handed the chain", func(t *testing.T) {
		var bridge, portmap map[string]any
		require.NoError(t, json.Unmarshal([]byte(h.readFile("cni-bridge-ADD.json")), &bridge))
		require.NoError(t, json.Unmarshal([]byte(h.readFile("cni-portmap-ADD.json")), &portmap))
		assert.Equal(t, "kind", bridge["name"])
		assert.Equal(t, "1.0.0", bridge["cniVersion"])
		assert.Nil(t, bridge["prevResult"])
		assert.Nil(t, bridge["runtimeConfig"], "a first start asks for no address")
		assert.NotNil(t, portmap["prevResult"], "the result of the bridge goes on to portmap")
		assert.Equal(t, map[string]any{"portMappings": []any{map[string]any{"hostPort": 34567.0, "containerPort": 6443.0, "protocol": "tcp", "hostIP": "127.0.0.1"}}}, portmap["runtimeConfig"])
	})

	t.Run("the container as callers read it", func(t *testing.T) {
		for template, want := range map[string]string{
			`{{ index .Config.Labels "io.x-k8s.kind.role"}}`:                                                                 "control-plane",
			`{{ index .Config.Labels "desktop.docker.io/ports/6443/tcp" }}`:                                                  "",
			`{{range .NetworkSettings.Networks}}{{.IPAddress}},{{.GlobalIPv6Address}}{{end}}`:                                "10.4.0.5,fc00:4::5",
			`{{ with (index (index .NetworkSettings.Ports "6443/tcp") 0) }}{{ printf "%s\t%s" .HostIp .HostPort }}{{ end }}`: "127.0.0.1\t34567",
			`{{.State.Running}}`: "true",
			`{{(index .NetworkSettings.Networks "kind").IPAddress}}`: "10.4.0.5",
			`{{.Name}}`: "/c-control-plane",
		} {
			require.Equal(t, 0, h.run("inspect", "-f", template, "c-control-plane"), template+": "+h.stderr.String())
			assert.Equal(t, want+"\n", h.stdout.String(), template)
		}
		require.Equal(t, 0, h.run("inspect", id[:12]))
		var inspected []struct {
			Created string `json:"Created"`
			Config  struct {
				Labels map[string]string `json:"Labels"`
			} `json:"Config"`
			State struct {
				Running bool `json:"Running"`
			} `json:"State"`
		}
		require.NoError(t, json.Unmarshal(h.stdout.Bytes(), &inspected), "inspect prints an array, and an ID prefix finds the container")
		require.Len(t, inspected, 1)
		assert.True(t, inspected[0].State.Running)
		assert.Equal(t, "c", inspected[0].Config.Labels["io.x-k8s.kind.cluster"])
	})

	t.Run("the files of the container", func(t *testing.T) {
		dir := filepath.Join(h.root, "containers", id)
		hosts, err := os.ReadFile(filepath.Join(dir, "hosts"))
		require.NoError(t, err)
		assert.Equal(t, "127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n10.4.0.5\tc-control-plane\n", string(hosts))
		resolv, err := os.ReadFile(filepath.Join(dir, "resolv.conf"))
		require.NoError(t, err)
		assert.Equal(t, "nameserver 192.0.2.53\n", string(resolv))
		hostname, err := os.ReadFile(filepath.Join(dir, "hostname"))
		require.NoError(t, err)
		assert.Equal(t, "c-control-plane\n", string(hostname))
	})

	t.Run("stopping it signals what the image says, detaches it in reverse and records the exit", func(t *testing.T) {
		require.Equal(t, 0, h.run("stop", "--time", "10", "c-control-plane"), h.stderr.String())
		assert.Equal(t, "c-control-plane\n", h.stdout.String())
		assert.Contains(t, h.calls(), "ctr --address /run/test.sock --namespace ns tasks kill --signal 37 "+id, "SIGRTMIN+3 is signal 37")
		var del []string
		for _, call := range h.calls() {
			if strings.Contains(call, " DEL ") {
				del = append(del, strings.Fields(call)[0])
			}
		}
		assert.Equal(t, []string{"bridge", "portmap", "firewall", "firewall", "portmap", "bridge"}[3:], del[len(del)-3:], "the last plugin first")
		assert.Contains(t, h.calls(), "ip netns del ctrd-"+id[:12])
		var firewall map[string]any
		require.NoError(t, json.Unmarshal([]byte(h.readFile("cni-firewall-DEL.json")), &firewall))
		assert.NotNil(t, firewall["prevResult"], "the result of the attachment goes back to the plugins")

		require.Equal(t, 0, h.run("inspect", "-f", "{{.State.Running}} {{.State.ExitCode}} {{(index .NetworkSettings.Networks \"kind\").IPAddress}}.", "c-control-plane"))
		assert.Equal(t, "false 137 .\n", h.stdout.String())
		require.Equal(t, 0, h.run("ps", "-a", "--format", "{{.Names}} {{.Status}}"))
		assert.Equal(t, "c-control-plane Exited (137)\n", h.stdout.String())
		require.Equal(t, 0, h.run("ps", "--format", "{{.Names}}"))
		assert.Empty(t, h.stdout.String(), "ps without -a lists running containers")
	})

	t.Run("starting it again asks for the addresses it had", func(t *testing.T) {
		require.Equal(t, 0, h.run("start", "c-control-plane"), h.stderr.String())
		var bridge struct {
			RuntimeConfig struct {
				IPs []string `json:"ips"`
			} `json:"runtimeConfig"`
		}
		require.NoError(t, json.Unmarshal([]byte(h.readFile("cni-bridge-ADD.json")), &bridge))
		assert.Equal(t, []string{"10.4.0.5", "fc00:4::5"}, bridge.RuntimeConfig.IPs)
	})

	t.Run("removing it takes the container, its files and its volumes", func(t *testing.T) {
		require.Equal(t, 1, h.run("rm", "c-control-plane"))
		assert.Contains(t, h.stderr.String(), "it is running")
		require.Equal(t, 0, h.run("rm", "-f", "-v", "c-control-plane"), h.stderr.String())
		assert.Contains(t, h.calls(), "ctr --address /run/test.sock --namespace ns containers delete "+id)
		assert.NoDirExists(t, filepath.Join(h.root, "containers", id))
		require.Equal(t, 0, h.run("rm", "-f", "c-control-plane"), "-f ignores a container that is gone")
		require.Equal(t, 1, h.run("rm", "c-control-plane"))
	})
}

func TestContainersOfANetworkResolveEachOther(t *testing.T) {
	h := newHarness(t)
	h.kindNetwork()
	h.setFile("images", "docker.io/library/registry:2 application/vnd.oci.image.index.v1+json sha256:aaa 1.0_MiB linux/arm64 -\n")
	require.Equal(t, 0, h.run("run", "-d", "--name", "registry-mirror", "--restart=always", "--network", "kind", "-e", "REGISTRY_PROXY_REMOTEURL=https://registry-1.docker.io", "registry:2"), h.stderr.String())
	require.Equal(t, 0, h.run("run", "-d", "--name", "node-a", "--hostname", "worker-1", "--network", "kind", "--entrypoint", "/bin/sh", "registry:2", "-c", "sleep"), h.stderr.String())

	dir := func(name string) string {
		meta, err := h.eng.findContainer(name)
		require.NoError(t, err)
		return filepath.Join(h.root, "containers", meta.ID)
	}
	registry, err := os.ReadFile(filepath.Join(dir("registry-mirror"), "hosts"))
	require.NoError(t, err)
	node, err := os.ReadFile(filepath.Join(dir("node-a"), "hosts"))
	require.NoError(t, err)
	assert.Contains(t, string(registry), "\tnode-a worker-1\n", "the cache learns of the node that started after it")
	assert.Contains(t, string(node), "\tregistry-mirror ", "the node knows the cache by name")
	assert.Contains(t, string(node), "\tworker-1 node-a\n", "a container is known by its hostname and its name")

	require.Equal(t, 0, h.run("stop", "node-a"), h.stderr.String())
	registry, err = os.ReadFile(filepath.Join(dir("registry-mirror"), "hosts"))
	require.NoError(t, err)
	assert.NotContains(t, string(registry), "node-a", "a container that stopped is no longer listed")
}

func TestCreateRefusals(t *testing.T) {
	h := newHarness(t)
	h.kindNetwork()
	h.setFile("images", "docker.io/library/img:latest application/vnd.oci.image.index.v1+json sha256:aaa 1.0_MiB linux/arm64 -\n")
	require.Equal(t, 0, h.run("create", "--name", "taken", "--network", "kind", "img"), h.stderr.String())

	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no network", []string{"create", "img"}, "no default network"},
		{"a network that is not there", []string{"create", "--network", "nope", "img"}, "network nope not found"},
		{"a name in use", []string{"create", "--name", "taken", "--network", "kind", "img"}, "already in use"},
		{"a command without an entrypoint", []string{"create", "--network", "kind", "img", "sleep", "1"}, "a command without --entrypoint"},
		{"another security option", []string{"create", "--network", "kind", "--security-opt", "label=disable", "img"}, "--security-opt label=disable: not supported by this engine"},
		{"a named volume", []string{"create", "--network", "kind", "-v", "data:/data", "img"}, "--volume data:/data"},
		{"a volume option that is not known", []string{"create", "--network", "kind", "-v", "/a:/b:nocopy", "img"}, "--volume option nocopy"},
		{"a sysctl outside net", []string{"create", "--network", "kind", "--sysctl", "kernel.shmmax=1", "img"}, "only net.* settings"},
		{"GPU capabilities", []string{"create", "--network", "kind", "--gpus", "all,capabilities=utility", "img"}, "--gpus all,capabilities=utility"},
		{"a port with no host port", []string{"create", "--network", "kind", "-p", "6443", "img"}, "--publish 6443"},
		{"a flag it does not have", []string{"create", "--network", "kind", "--volumes-from", "x", "img"}, "create --volumes-from: not supported by this engine"},
		{"run in the foreground", []string{"run", "--network", "kind", "img"}, "run without --detach"},
		{"no image", []string{"create", "--network", "kind"}, "an image is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, 1, h.run(tt.args...))
			assert.Contains(t, h.stderr.String(), tt.want)
		})
	}
	all, err := h.eng.listContainers()
	require.NoError(t, err)
	assert.Len(t, all, 1, "a refused create leaves nothing behind")
}

func TestParsePublish(t *testing.T) {
	got, err := parsePublish([]string{"127.0.0.1:34567:6443/TCP", "8080:80", "0.0.0.0:53:53/udp", "[::1]:9000:9000/tcp"})
	require.NoError(t, err)
	assert.Equal(t, []portMapping{
		{HostIP: "127.0.0.1", HostPort: 34567, ContainerPort: 6443, Protocol: "tcp"},
		{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
		{HostIP: "0.0.0.0", HostPort: 53, ContainerPort: 53, Protocol: "udp"},
		{HostIP: "::1", HostPort: 9000, ContainerPort: 9000, Protocol: "tcp"},
	}, got)
}

func TestCDIDevices(t *testing.T) {
	tests := []struct {
		gpus    string
		want    []string
		wantErr bool
	}{
		{gpus: ""},
		{gpus: " all ", want: []string{"nvidia.com/gpu=all"}},
		{gpus: "-1", want: []string{"nvidia.com/gpu=all"}},
		{gpus: "2", want: []string{"nvidia.com/gpu=0", "nvidia.com/gpu=1"}},
		{gpus: `"device=0,1"`, want: []string{"nvidia.com/gpu=0", "nvidia.com/gpu=1"}},
		{gpus: "device=GPU-b20a5f22", want: []string{"nvidia.com/gpu=GPU-b20a5f22"}},
		{gpus: "0", wantErr: true},
		{gpus: "all,capabilities=compute", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.gpus, func(t *testing.T) {
			got, err := cdiDevices(tt.gpus)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestNicoSimNodeCreateStartRestart(t *testing.T) {
	h := newHarness(t)
	h.kindNetwork()
	h.setFile("images", "docker.io/library/cwe-capd-kindest-node:local application/vnd.oci.image.manifest.v1+json sha256:aaa 1.0_MiB linux/arm64 -\n")
	args := []string{
		"create", "--name", "node-0123abcd", "--hostname", "node", "--privileged", "--security-opt", "seccomp=unconfined",
		"--security-opt", "apparmor=unconfined", "--tmpfs", "/tmp", "--tmpfs", "/run", "--tmpfs", "/var/lib/etcd:size=4g",
		"--restart", "always", "--network", "kind", "--env", "KUBECONFIG=/etc/kubernetes/admin.conf", "--volume", "/var",
		"--volume", "/lib/modules:/lib/modules:ro", "--entrypoint", "/bin/sh", "--tty", "--gpus", "all",
		"--label", "nvidia.com/nico-containers.managed=true", "cwe-capd-kindest-node:local", "-c", "set -e; exec /sbin/init",
	}
	require.Equal(t, 0, h.run(args...), h.stderr.String())
	id := strings.TrimSpace(h.stdout.String())

	create := h.callsMatching("ctr --address /run/test.sock --namespace ns containers create")
	require.Len(t, create, 1)
	assert.Contains(t, create[0], "--mount type=tmpfs,src=tmpfs,dst=/var/lib/etcd,options=size=4g:noexec:nosuid:nodev")
	assert.Contains(t, create[0], "--device nvidia.com/gpu=all")
	assert.Contains(t, create[0], "--label nvidia.com/nico-containers.managed=true")
	assert.True(t, strings.HasSuffix(create[0], " docker.io/library/cwe-capd-kindest-node:local "+id+" /bin/sh -c set -e; exec /sbin/init"), "the entrypoint replaces the image's: %q", create[0])

	require.Equal(t, 0, h.run("inspect", "-f", "{{.State.Status}}", id))
	assert.Equal(t, "created\n", h.stdout.String())
	require.Equal(t, 0, h.run("start", id), h.stderr.String())
	require.Equal(t, 0, h.run("start", id), "starting a running container is not an error")
	assert.Len(t, h.callsMatching("supervisor "), 1)

	require.Equal(t, 0, h.run("restart", "--time", "10", id), h.stderr.String())
	assert.Len(t, h.callsMatching("supervisor "), 2, "a restart is a stop and a start of the same container")
	assert.Len(t, h.callsMatching("ctr --address /run/test.sock --namespace ns containers create"), 1, "the container, and so its disk, is the same")
}

// resolvOf is the resolv.conf a container's next start mounts.
func (h *harness) resolvOf(id string) string {
	h.t.Helper()
	data, err := os.ReadFile(filepath.Join(h.root, "containers", id, "resolv.conf"))
	require.NoError(h.t, err)
	return string(data)
}

func TestResolvConfIsFixedAtCreate(t *testing.T) {
	create := func(h *harness, name string) string {
		require.Equal(t, 0, h.run("create", "--name", name, "--network", "kind", "--entrypoint", "/bin/sh", "img", "-c", "true"), h.stderr.String())
		return strings.TrimSpace(h.stdout.String())
	}
	newNode := func(t *testing.T) *harness {
		t.Helper()
		h := newHarness(t)
		h.kindNetwork()
		h.setFile("images", "docker.io/library/img:latest application/vnd.oci.image.manifest.v1+json sha256:aaa 1.0_MiB linux/arm64 -\n")
		return h
	}

	t.Run("a start with another CTRD_DNS keeps the one it was created with", func(t *testing.T) {
		h := newNode(t)
		h.eng.cfg.DNS = []string{"1.1.1.1", "8.8.8.8"}
		id := create(h, "a")
		assert.Equal(t, "nameserver 1.1.1.1\nnameserver 8.8.8.8\n", h.resolvOf(id))

		h.eng.cfg.DNS = nil
		require.Equal(t, 0, h.run("start", "a"), h.stderr.String())
		assert.Equal(t, "nameserver 1.1.1.1\nnameserver 8.8.8.8\n", h.resolvOf(id), "a shell without CTRD_DNS does not change the container")

		h.eng.cfg.DNS = []string{"9.9.9.9"}
		require.Equal(t, 0, h.run("restart", "--time", "0", "a"), h.stderr.String())
		assert.Equal(t, "nameserver 1.1.1.1\nnameserver 8.8.8.8\n", h.resolvOf(id), "nor does a shell with another one")

		require.Equal(t, 0, h.run("stop", "--time", "0", "a"))
		require.Equal(t, 0, h.run("start", "a"))
		assert.Equal(t, "nameserver 1.1.1.1\nnameserver 8.8.8.8\n", h.resolvOf(id))
	})

	t.Run("a container created without CTRD_DNS keeps the host's resolvers", func(t *testing.T) {
		h := newNode(t)
		id := create(h, "b")
		assert.Equal(t, "nameserver 192.0.2.53\n", h.resolvOf(id))

		h.eng.cfg.DNS = []string{"1.1.1.1"}
		require.Equal(t, 0, h.run("restart", "--time", "0", "b"), h.stderr.String())
		assert.Equal(t, "nameserver 192.0.2.53\n", h.resolvOf(id), "setting CTRD_DNS later does not reach a container that exists")

		h.eng.cfg.DNS = nil
		require.NoError(t, os.WriteFile(h.eng.cfg.ResolvConfs[0], []byte("nameserver 198.51.100.1\n"), 0o644))
		require.Equal(t, 0, h.run("restart", "--time", "0", "b"), h.stderr.String())
		assert.Equal(t, "nameserver 192.0.2.53\n", h.resolvOf(id), "nor does a change of the host's file")

		other := create(h, "c")
		assert.Equal(t, "nameserver 198.51.100.1\n", h.resolvOf(other), "a container created now has the new one")
	})

	t.Run("a container created before the record follows the environment of each start", func(t *testing.T) {
		h := newNode(t)
		id := create(h, "d")
		meta, err := h.eng.loadContainer(id)
		require.NoError(t, err)
		meta.ResolvConf = ""
		require.NoError(t, h.eng.saveContainer(meta))

		h.eng.cfg.DNS = []string{"1.1.1.1"}
		require.Equal(t, 0, h.run("restart", "--time", "0", "d"), h.stderr.String())
		assert.Equal(t, "nameserver 1.1.1.1\n", h.resolvOf(id))
		h.eng.cfg.DNS = nil
		require.Equal(t, 0, h.run("restart", "--time", "0", "d"), h.stderr.String())
		assert.Equal(t, "nameserver 192.0.2.53\n", h.resolvOf(id), "the old behavior, which only a recreate ends")
	})

	t.Run("a refused create records nothing", func(t *testing.T) {
		h := newNode(t)
		h.eng.cfg.ResolvConfs = []string{filepath.Join(h.dir, "missing")}
		require.Equal(t, 1, h.run("create", "--name", "e", "--network", "kind", "--entrypoint", "/bin/sh", "img", "-c", "true"))
		assert.Contains(t, h.stderr.String(), "set CTRD_DNS")
		assert.NoDirExists(t, filepath.Join(h.root, "containers"))
	})
}

func TestHostNetworkContainerAndCp(t *testing.T) {
	h := newHarness(t)
	h.setFile("images", "quay.io/frrouting/frr:9.1.0 application/vnd.oci.image.index.v1+json sha256:aaa 1.0_MiB linux/arm64 -\n")
	require.Equal(t, 0, h.run("create", "--name", "bgp-containers", "--network", "host", "--cap-add", "NET_ADMIN", "--cap-add", "SYS_ADMIN", "--cap-add", "NET_BIND_SERVICE", "quay.io/frrouting/frr:9.1.0"), h.stderr.String())

	create := h.callsMatching("ctr --address /run/test.sock --namespace ns containers create")
	require.Len(t, create, 1)
	for _, want := range []string{"--net-host", "--cap-add CAP_NET_ADMIN", "--cap-add CAP_SYS_ADMIN", "--cap-add CAP_NET_BIND_SERVICE", "--with-ns cgroup:"} {
		assert.Contains(t, create[0], want)
	}
	assert.NotContains(t, create[0], "--hostname")
	assert.NotContains(t, create[0], "network:/run/netns")
	assert.NotContains(t, create[0], "--privileged")
	assert.Contains(t, create[0], "--mount type=bind,src="+filepath.Join(h.root, "containers", strings.TrimSpace(h.stdout.String()), "volumes", "run")+",dst=/run,options=rbind:rw", "the image's /run is a volume, as it is under docker")

	source := filepath.Join(t.TempDir(), "frr.conf")
	require.NoError(t, os.WriteFile(source, []byte("router bgp 65000\n"), 0o640))
	require.Equal(t, 0, h.run("cp", source, "bgp-containers:/etc/frr/frr.conf"), h.stderr.String())
	copied, err := os.ReadFile(filepath.Join(h.dir, "fake", "mounted", "etc", "frr", "frr.conf"))
	require.NoError(t, err)
	assert.Equal(t, "router bgp 65000\n", string(copied))
	info, err := os.Stat(filepath.Join(h.dir, "fake", "mounted", "etc", "frr", "frr.conf"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), info.Mode().Perm())

	require.Equal(t, 0, h.run("cp", source, "bgp-containers:/etc/frr"), "a directory as the destination takes the file under its name")
	assert.FileExists(t, filepath.Join(h.dir, "fake", "mounted", "etc", "frr", "frr.conf"))

	require.Equal(t, 0, h.run("start", "bgp-containers"), h.stderr.String())
	assert.Empty(t, h.callsMatching("bridge "), "a host-network container has no namespace to attach")
	require.Equal(t, 1, h.run("cp", source, "bgp-containers:/etc/frr/x"))
	assert.Contains(t, h.stderr.String(), "cp into a running container: not supported by this engine")
	require.Equal(t, 1, h.run("cp", "bgp-containers:/etc/frr/frr.conf", "/tmp/x"))
	assert.Contains(t, h.stderr.String(), "cp from a container")
}
