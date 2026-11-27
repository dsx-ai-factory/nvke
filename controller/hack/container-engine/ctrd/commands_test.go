// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// started is a harness with a running container named c on the kind network.
func started(t *testing.T) (*harness, string) {
	t.Helper()
	h := newHarness(t)
	h.kindNetwork()
	h.setFile("images", "docker.io/library/img:latest application/vnd.oci.image.manifest.v1+json sha256:aaa 1.0_MiB linux/arm64 -\n")
	require.Equal(t, 0, h.run("run", "-d", "--name", "c", "--network", "kind", "--label", "k=v", "--entrypoint", "/bin/sh", "img", "-c", "sleep"), h.stderr.String())
	return h, strings.TrimSpace(h.stdout.String())
}

func TestExec(t *testing.T) {
	h, id := started(t)
	execCall := func() string {
		calls := h.callsMatching("ctr --address /run/test.sock --namespace ns tasks exec")
		require.NotEmpty(t, calls)
		return calls[len(calls)-1]
	}

	require.Equal(t, 0, h.run("exec", "--privileged", "c", "/bin/sh", "-c", "echo hi"), h.stderr.String())
	assert.Equal(t, "exec output\n", h.stdout.String())
	assert.Regexp(t, `tasks exec --exec-id [0-9a-f]{12} `+id+` /bin/sh -c echo hi$`, execCall())

	require.Equal(t, 0, h.run("exec", "-i", "-e", "A=b", "-e", "C=d", "c", "cat"))
	assert.Regexp(t, `tasks exec --exec-id [0-9a-f]{12} `+id+` env A=b C=d cat$`, execCall(), "ctr has no flag for the environment of the process")

	require.Equal(t, 0, h.run("exec", "--detach", "c", "sh", "script.sh", "status"))
	assert.Regexp(t, `tasks exec --exec-id [0-9a-f]{12} --detach --log-uri file:///dev/null `+id+` sh script.sh status$`, execCall())

	t.Run("the exit status of the process is the status of exec", func(t *testing.T) {
		t.Setenv("EXEC_STATUS", "7")
		assert.Equal(t, 7, h.run("exec", "c", "false"))
	})
	t.Run("only a running container", func(t *testing.T) {
		require.Equal(t, 0, h.run("stop", "c"))
		assert.Equal(t, 1, h.run("exec", "c", "true"))
		assert.Contains(t, h.stderr.String(), "container c is not running")
	})
}

func TestLogs(t *testing.T) {
	h, id := started(t)
	logPath := filepath.Join(h.root, "containers", id, "log")
	require.NoError(t, os.WriteFile(logPath, []byte("INFO: detected cgroup v2\nReached target Multi-User System.\n"), 0o600))

	require.Equal(t, 0, h.run("logs", "c"))
	assert.Equal(t, "INFO: detected cgroup v2\nReached target Multi-User System.\n", h.stdout.String())

	require.Equal(t, 0, h.run("stop", "c"))
	require.Equal(t, 0, h.run("logs", "-f", "c"), "following a container that has stopped ends at the end of its log")
	assert.Contains(t, h.stdout.String(), "Multi-User System")

	require.Equal(t, 1, h.run("logs", "nope"))
}

func TestPs(t *testing.T) {
	h, id := started(t)
	require.Equal(t, 0, h.run("create", "--name", "idle", "--network", "kind", "--label", "k=other", "--label", "only=idle", "img"))

	for name, tt := range map[string]struct {
		args []string
		want string
	}{
		"running ones only":         {[]string{"--format", "{{.Names}}"}, "c\n"},
		"all":                       {[]string{"-a", "--format", "{{.Names}}"}, "c\nidle\n"},
		"a label with its value":    {[]string{"-a", "--filter", "label=k=other", "--format", "{{.Names}}"}, "idle\n"},
		"a label without a value":   {[]string{"-a", "--filter", "label=only", "--format", "{{.Names}}"}, "idle\n"},
		"labels are anded":          {[]string{"-a", "--filter", "label=k=v", "--filter", "label=only", "--format", "{{.Names}}"}, ""},
		"quiet prints short IDs":    {[]string{"-aq", "--filter", "label=k=v"}, id[:12] + "\n"},
		"a label by template":       {[]string{"-a", "--format", `{{.Label "k"}}`}, "v\nother\n"},
		"the state":                 {[]string{"-a", "--format", "{{.Names}}={{.State}}"}, "c=running\nidle=created\n"},
		"the image as it is stored": {[]string{"--format", "{{.Image}}"}, "docker.io/library/img:latest\n"},
		"a missing label is empty":  {[]string{"--format", `[{{.Label "nope"}}]`}, "[]\n"},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, 0, h.run(append([]string{"ps"}, tt.args...)...), h.stderr.String())
			assert.Equal(t, tt.want, h.stdout.String())
		})
	}
	t.Run("the table", func(t *testing.T) {
		require.Equal(t, 0, h.run("ps"))
		lines := strings.Split(strings.TrimSpace(h.stdout.String()), "\n")
		require.Len(t, lines, 2)
		assert.Contains(t, lines[0], "CONTAINER ID")
		assert.Contains(t, lines[1], id[:12])
	})
	t.Run("another filter is not supported", func(t *testing.T) {
		assert.Equal(t, 1, h.run("ps", "--filter", "name=c"))
		assert.Contains(t, h.stderr.String(), "not supported by this engine")
	})
}

func TestImages(t *testing.T) {
	h := newHarness(t)
	const pull = "ctr --address /run/test.sock --namespace ns images pull --platform linux/" + runtime.GOARCH + " "

	require.Equal(t, 0, h.run("pull", "registry:2"))
	assert.Equal(t, []string{pull + "docker.io/library/registry:2"}, h.callsMatching("ctr --address /run/test.sock --namespace ns images pull"))
	assert.Empty(t, h.stdout.String(), "the progress lines of ctr are not printed")

	require.Equal(t, 0, h.run("pull", "--platform", "linux/amd64", "quay.io/frrouting/frr:9.1.0"))
	assert.Contains(t, h.calls(), "ctr --address /run/test.sock --namespace ns images pull --platform linux/amd64 quay.io/frrouting/frr:9.1.0")

	t.Run("what is stored is inspected, in both spellings", func(t *testing.T) {
		require.Equal(t, 0, h.run("image", "inspect", "registry:2"))
		require.Equal(t, 0, h.run("inspect", "--type=image", "docker.io/library/registry:2"))
		var images []struct {
			ID       string   `json:"Id"`
			RepoTags []string `json:"RepoTags"`
		}
		require.NoError(t, json.Unmarshal(h.stdout.Bytes(), &images))
		require.Len(t, images, 1)
		assert.Equal(t, "sha256:aaa", images[0].ID)
		assert.Equal(t, []string{"docker.io/library/registry:2"}, images[0].RepoTags)
		assert.Equal(t, 1, h.run("image", "inspect", "nope:1"))
		assert.Equal(t, 1, h.run("inspect", "--type=image", "nope:1"))
		assert.Contains(t, h.stderr.String(), "no such image: docker.io/library/nope:1")
	})

	t.Run("create pulls what is missing and only that", func(t *testing.T) {
		h.kindNetwork()
		before := len(h.callsMatching("ctr --address /run/test.sock --namespace ns images pull"))
		require.Equal(t, 0, h.run("create", "--network", "kind", "registry:2"))
		require.Equal(t, 0, h.run("create", "--network", "kind", "never-pulled:1"))
		assert.Len(t, h.callsMatching("ctr --address /run/test.sock --namespace ns images pull"), before+1)
	})

	t.Run("save writes an archive for the node's import and flattens an index", func(t *testing.T) {
		const arm = "sha256:2222"
		export := archive(t, map[string]string{
			"index.json":        `{"schemaVersion":2,"manifests":[{"mediaType":"application/vnd.oci.image.index.v1+json","digest":"sha256:1111","size":9,"annotations":{"io.containerd.image.name":"docker.io/library/registry:2"}}]}`,
			"blobs/sha256/1111": `{"manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + arm + `","size":5,"platform":{"os":"linux","architecture":"` + runtime.GOARCH + `"}}]}`,
		}, []string{"index.json", "blobs/sha256/1111"})
		data, err := os.ReadFile(export)
		require.NoError(t, err)
		h.setFile("export.tar", string(data))
		out := filepath.Join(t.TempDir(), "image.tar")

		require.Equal(t, 0, h.run("save", "-o", out, "registry:2"), h.stderr.String())
		exports := h.callsMatching("ctr --address /run/test.sock --namespace ns images export")
		require.Len(t, exports, 1)
		assert.Equal(t, "ctr --address /run/test.sock --namespace ns images export --platform linux/"+runtime.GOARCH+" "+out+".export docker.io/library/registry:2", exports[0])
		assert.NoFileExists(t, out+".export")
		assert.Contains(t, readArchive(t, out)["index.json"], `"digest":"`+arm+`"`)

		assert.Equal(t, 1, h.run("save", "-o", out, "nope:1"))
		assert.Equal(t, 1, h.run("save", "registry:2"), "a archive on standard output is not supported")
	})

	t.Run("load imports an archive", func(t *testing.T) {
		require.Equal(t, 0, h.run("load", "-i", "/tmp/images.tar"))
		assert.Contains(t, h.calls(), "ctr --address /run/test.sock --namespace ns images import --platform linux/"+runtime.GOARCH+" /tmp/images.tar")
		assert.Equal(t, 1, h.run("load"))
	})
}

func TestBuild(t *testing.T) {
	ctx := t.TempDir()
	dockerfile := filepath.Join(ctx, "Dockerfile")

	t.Run("the arguments of buildctl", func(t *testing.T) {
		assert.Equal(t, []string{
			"--addr", "unix:///run/bk.sock", "build", "--frontend", "dockerfile.v0",
			"--local", "context=" + ctx, "--local", "dockerfile=" + ctx, "--opt", "filename=Dockerfile",
			"--opt", "platform=linux/arm64", "--opt", "build-arg:A=b", "--opt", "build-arg:C=d",
			"--output", `type=image,"name=docker.local/cwe:tilt,docker.io/library/x:1",unpack=true`,
		}, buildctlArgs("unix:///run/bk.sock", ctx, dockerfile, "linux/arm64", []string{"docker.local/cwe:tilt", "docker.io/library/x:1"}, []string{"A=b", "C=d"}, ""))
	})
	t.Run("an archive output gets the name from the tag", func(t *testing.T) {
		got := buildctlArgs("h", ctx, dockerfile, "", []string{"docker.io/library/i:1"}, nil, "type=docker,dest=/w/images.tar,rewrite-timestamp=true")
		assert.Equal(t, []string{"--output", `type=docker,dest=/w/images.tar,rewrite-timestamp=true,"name=docker.io/library/i:1"`}, got[len(got)-2:])
		got = buildctlArgs("h", ctx, dockerfile, "", []string{"i:1"}, nil, "type=oci,name=mine,dest=/x")
		assert.Equal(t, []string{"--output", "type=oci,name=mine,dest=/x"}, got[len(got)-2:], "an exporter that names the image keeps its name")
	})

	t.Run("through the command", func(t *testing.T) {
		h := newHarness(t)
		t.Setenv("FROM_ENV", "e")
		// A build argument without a value reads the environment, so UNSET must be absent.
		t.Setenv("UNSET", "")
		require.NoError(t, os.Unsetenv("UNSET"))
		require.Equal(t, 0, h.run("build", "--platform", "linux/arm64", "--tag", "docker.local/cwe:tilt", "-f", dockerfile,
			"--build-arg", "A=b", "--build-arg", "FROM_ENV", "--build-arg", "UNSET", ctx), h.stderr.String())
		require.Len(t, h.callsMatching("buildctl"), 1)
		assert.Equal(t, "buildctl --addr unix:///run/bk.sock build --frontend dockerfile.v0 --local context="+ctx+" --local dockerfile="+ctx+
			" --opt filename=Dockerfile --opt platform=linux/arm64 --opt build-arg:A=b --opt build-arg:FROM_ENV=e"+
			` --output type=image,"name=docker.local/cwe:tilt",unpack=true`, h.callsMatching("buildctl")[0])
		assert.Equal(t, 1, h.run("build", ctx, ctx))
	})
}

func TestInfoAndVersion(t *testing.T) {
	h := newHarness(t)

	require.Equal(t, 0, h.run("info", "--format", "{{.ServerVersion}}"))
	assert.Equal(t, "v2.4.2\n", h.stdout.String())

	require.Equal(t, 0, h.run("info", "--format", "{{json .}}"))
	var info struct {
		CgroupDriver    string   `json:"CgroupDriver"`
		CgroupVersion   string   `json:"CgroupVersion"`
		MemoryLimit     bool     `json:"MemoryLimit"`
		PidsLimit       bool     `json:"PidsLimit"`
		CPUShares       bool     `json:"CPUShares"`
		SecurityOptions []string `json:"SecurityOptions"`
	}
	require.NoError(t, json.Unmarshal(h.stdout.Bytes(), &info), "kind decodes this")
	assert.Contains(t, []string{"1", "2"}, info.CgroupVersion)
	assert.NotEmpty(t, info.CgroupDriver)
	assert.NotNil(t, info.SecurityOptions)

	require.Equal(t, 0, h.run("version", "--format", "{{.Server.Arch}}"))
	assert.Equal(t, runtime.GOARCH+"\n", h.stdout.String())
	require.Equal(t, 0, h.run("version"))
	assert.Contains(t, h.stdout.String(), "v2.4.2")
}

func TestSupervise(t *testing.T) {
	if _, err := os.Stat("/dev/ptmx"); err != nil {
		t.Skip("no pseudo-terminals here")
	}
	h, id := started(t)
	require.Equal(t, 0, h.run("_supervise", id), h.stderr.String())

	log, err := os.ReadFile(filepath.Join(h.root, "containers", id, "log"))
	require.NoError(t, err)
	assert.Contains(t, strings.ReplaceAll(string(log), "\r", ""), "console line\nsecond line\n", "the console is the log")
	code, err := os.ReadFile(filepath.Join(h.root, "containers", id, "exitcode"))
	require.NoError(t, err)
	assert.Equal(t, "3\n", string(code), "the status of ctr is the status of the task")
	assert.Contains(t, h.calls(), "ctr --address /run/test.sock --namespace ns tasks start "+id)
	moved, err := os.ReadFile(filepath.Join(h.dir, "cgroups", id[:12], "cgroup.procs"))
	require.NoError(t, err)
	assert.Equal(t, "0\n", string(moved), "the supervisor moved itself into a cgroup of its own")
}

func TestUnsupported(t *testing.T) {
	h := newHarness(t)
	for _, args := range [][]string{
		{"compose"},
		{"images"},
		{"network", "ls"},
		{"network", "connect", "kind", "c"},
		{"image", "ls"},
		{"volume", "ls"},
		{"inspect", "--type=volume", "x"},
		{"build", "--secret", "id=x", "."},
		{"push", "x"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			assert.Equal(t, 1, h.run(args...))
			assert.Contains(t, h.stderr.String(), "not supported by this engine")
		})
	}
	assert.Equal(t, 1, h.run())
}
