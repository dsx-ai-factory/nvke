// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bootstrapengine

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// fakeCall records one CommandRunner.Run invocation.
type fakeCall struct {
	env  map[string]string
	name string
	args []string
}

// fakeRunner is a scripted CommandRunner for unit tests: no real kind binary
// or container runtime required.
type fakeRunner struct {
	calls []fakeCall

	// outputs is consulted in call order for each Run invocation; if shorter
	// than the number of calls, the last entry is reused.
	outputs []fakeOutput
}

type fakeOutput struct {
	output string
	err    error
}

func (f *fakeRunner) Run(_ context.Context, env map[string]string, name string, args ...string) (string, error) {
	f.calls = append(f.calls, fakeCall{env: env, name: name, args: args})
	idx := len(f.calls) - 1
	if idx < len(f.outputs) {
		return f.outputs[idx].output, f.outputs[idx].err
	}
	if len(f.outputs) > 0 {
		last := f.outputs[len(f.outputs)-1]
		return last.output, last.err
	}
	return "", nil
}

func joinArgs(args []string) string { return strings.Join(args, " ") }

func TestCreate_UsesProviderEnv(t *testing.T) {
	tests := []struct {
		name        string
		provider    Provider
		wantEnvKey  string
		wantEnvVal  string
		wantEnvNone bool
	}{
		{name: "nerdctl sets KIND_EXPERIMENTAL_PROVIDER", provider: ProviderNerdctl, wantEnvKey: "KIND_EXPERIMENTAL_PROVIDER", wantEnvVal: "nerdctl"},
		{name: "docker sets no override", provider: ProviderDocker, wantEnvNone: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeRunner{
				outputs: []fakeOutput{
					{output: "No kind clusters found.\n", err: errors.New("exit status 1")}, // get clusters (Exists check)
					{output: ""},                 // create cluster
					{output: "apiVersion: v1\n"}, // get kubeconfig
				},
			}
			e := New(runner, nil)

			cluster, err := e.Create(context.Background(), Config{
				ClusterName: "test-cluster",
				Provider:    tt.provider,
			})
			if err != nil {
				t.Fatalf("Create returned error: %v", err)
			}
			if cluster.Name != "test-cluster" {
				t.Errorf("cluster.Name = %q, want %q", cluster.Name, "test-cluster")
			}

			// call[1] is `kind create cluster ...`.
			if len(runner.calls) < 2 {
				t.Fatalf("expected at least 2 calls, got %d", len(runner.calls))
			}
			createCall := runner.calls[1]
			if createCall.name != "kind" || createCall.args[0] != "create" {
				t.Fatalf("call[1] = %s %s, want a `kind create ...` call", createCall.name, joinArgs(createCall.args))
			}

			if tt.wantEnvNone {
				if v, ok := createCall.env[kindExperimentalProviderEnv]; ok {
					t.Errorf("docker provider set %s=%q, want unset", kindExperimentalProviderEnv, v)
				}
				return
			}
			if got := createCall.env[tt.wantEnvKey]; got != tt.wantEnvVal {
				t.Errorf("%s = %q, want %q", tt.wantEnvKey, got, tt.wantEnvVal)
			}
		})
	}
}

func TestCreate_RequiresClusterName(t *testing.T) {
	e := New(&fakeRunner{}, nil)
	if _, err := e.Create(context.Background(), Config{}); err == nil {
		t.Fatal("expected an error for a missing ClusterName")
	}
}

func TestCreate_FailsIfClusterAlreadyExists(t *testing.T) {
	runner := &fakeRunner{
		outputs: []fakeOutput{
			{output: "test-cluster\n"}, // get clusters: already exists
		},
	}
	e := New(runner, nil)
	_, err := e.Create(context.Background(), Config{ClusterName: "test-cluster", Provider: ProviderNerdctl})
	if err == nil {
		t.Fatal("expected an error when the cluster already exists")
	}
	if len(runner.calls) != 1 {
		t.Errorf("expected Create to stop after the existence check, got %d calls", len(runner.calls))
	}
}

func TestCreate_PropagatesKindError(t *testing.T) {
	runner := &fakeRunner{
		outputs: []fakeOutput{
			{output: "No kind clusters found.\n", err: errors.New("exit status 1")},
			{output: "ERROR: failed to create cluster\n", err: errors.New("exit status 1")},
		},
	}
	e := New(runner, nil)
	_, err := e.Create(context.Background(), Config{ClusterName: "test-cluster", Provider: ProviderNerdctl})
	if err == nil {
		t.Fatal("expected Create to propagate the kind create error")
	}
	if !strings.Contains(err.Error(), "failed to create cluster") {
		t.Errorf("error %q does not include kind's output", err)
	}
}

func TestDelete_IsSafeWhenNothingWasCreated(t *testing.T) {
	runner := &fakeRunner{outputs: []fakeOutput{{output: "Deleted clusters: [\"test-cluster\"]\n"}}}
	e := New(runner, nil)
	if err := e.Delete(context.Background(), "test-cluster", ProviderNerdctl); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}
	if len(runner.calls) != 1 || runner.calls[0].args[0] != "delete" {
		t.Fatalf("unexpected call: %+v", runner.calls)
	}
}

func TestDelete_RequiresClusterName(t *testing.T) {
	e := New(&fakeRunner{}, nil)
	if err := e.Delete(context.Background(), "", ProviderNerdctl); err == nil {
		t.Fatal("expected an error for a missing clusterName")
	}
}

func TestExists(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		err     error
		want    bool
		wantErr bool
	}{
		{name: "cluster present", output: "other-cluster\ntest-cluster\n", want: true},
		{name: "cluster absent", output: "other-cluster\n", want: false},
		{name: "no clusters at all", output: "No kind clusters found.\n", err: errors.New("exit status 1"), want: false},
		{name: "real failure propagates", output: "boom\n", err: errors.New("exit status 2"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeRunner{outputs: []fakeOutput{{output: tt.output, err: tt.err}}}
			e := New(runner, nil)
			got, err := e.Exists(context.Background(), "test-cluster", ProviderNerdctl)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Exists returned error: %v", err)
			}
			if got != tt.want {
				t.Errorf("Exists = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestExists_ScopesQueryToProvider guards the bug this package used to have:
// Exists and Kubeconfig ignored the provider argument entirely, so a cluster
// created via ProviderNerdctl was invisible to a later Exists/Kubeconfig
// call, which always queried the Docker provider (kind's default). Create's
// own duplicate-cluster guard and kubeconfig fetch depend on this being
// right — see bootstrapengine.go's Create for both call sites.
func TestExists_ScopesQueryToProvider(t *testing.T) {
	runner := &fakeRunner{outputs: []fakeOutput{{output: "test-cluster\n"}}}
	e := New(runner, nil)
	if _, err := e.Exists(context.Background(), "test-cluster", ProviderNerdctl); err != nil {
		t.Fatalf("Exists returned error: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(runner.calls))
	}
	if got := runner.calls[0].env[kindExperimentalProviderEnv]; got != "nerdctl" {
		t.Errorf("%s = %q, want %q", kindExperimentalProviderEnv, got, "nerdctl")
	}
}

func TestKubeconfig_ScopesQueryToProvider(t *testing.T) {
	runner := &fakeRunner{outputs: []fakeOutput{{output: "apiVersion: v1\n"}}}
	e := New(runner, nil)
	if _, err := e.Kubeconfig(context.Background(), "test-cluster", ProviderNerdctl); err != nil {
		t.Fatalf("Kubeconfig returned error: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(runner.calls))
	}
	if got := runner.calls[0].env[kindExperimentalProviderEnv]; got != "nerdctl" {
		t.Errorf("%s = %q, want %q", kindExperimentalProviderEnv, got, "nerdctl")
	}
}

// Kubeconfig used to write Run's output to the file, and Run merges stderr.
// kind prints "using nerdctl due to KIND_EXPERIMENTAL_PROVIDER" there under
// every non-Docker provider, which corrupted the kubeconfig. kind must write
// the path itself.
func TestKubeconfig_LetsKindWriteTheFile(t *testing.T) {
	runner := &fakeRunner{outputs: []fakeOutput{
		{output: "using nerdctl due to KIND_EXPERIMENTAL_PROVIDER\n"},
	}}
	e := New(runner, nil)

	path, err := e.Kubeconfig(context.Background(), "test-cluster", ProviderNerdctl)
	if err != nil {
		t.Fatalf("Kubeconfig returned error: %v", err)
	}

	call := runner.calls[0]
	if call.name != "kind" || joinArgs(call.args[:2]) != "export kubeconfig" {
		t.Fatalf("call = %s %s, want `kind export kubeconfig`", call.name, joinArgs(call.args))
	}
	if call.args[len(call.args)-2] != "--kubeconfig" || call.args[len(call.args)-1] != path {
		t.Errorf("args = %v, want --kubeconfig %s", call.args, path)
	}
	if body, err := os.ReadFile(path); err == nil && strings.Contains(string(body), "using nerdctl") {
		t.Errorf("kubeconfig holds kind's stderr line: %q", body)
	}
}

func TestLoadImage_UsesProviderEnv(t *testing.T) {
	runner := &fakeRunner{outputs: []fakeOutput{{output: "Image: \"my-image:local\" with ID"}}}
	e := New(runner, nil)
	if err := e.LoadImage(context.Background(), "test-cluster", ProviderDocker, "my-image:local"); err != nil {
		t.Fatalf("LoadImage returned error: %v", err)
	}
	call := runner.calls[0]
	if joinArgs(call.args) != "load docker-image my-image:local --name test-cluster" {
		t.Errorf("args = %q, want the expected `kind load docker-image` invocation", joinArgs(call.args))
	}
	if v, ok := call.env[kindExperimentalProviderEnv]; ok {
		t.Errorf("docker provider set %s=%q, want unset", kindExperimentalProviderEnv, v)
	}
}

func TestLoadImage_RequiresImage(t *testing.T) {
	e := New(&fakeRunner{}, nil)
	if err := e.LoadImage(context.Background(), "test-cluster", ProviderNerdctl, ""); err == nil {
		t.Fatal("expected an error for a missing image")
	}
}

// TestLoadImage_NerdctlSavesArchiveInstead guards the bug this package used
// to have: `kind load docker-image` always shells out to `docker` to read
// the image, regardless of provider, so it fails with "not present locally"
// for a nerdctl-pulled image even though it exists. Under nerdctl, LoadImage
// must save the image itself and use `kind load image-archive` instead,
// which has no such docker dependency.
func TestLoadImage_NerdctlSavesArchiveInstead(t *testing.T) {
	runner := &fakeRunner{
		outputs: []fakeOutput{
			// Target == manifest: a local image, nothing to flatten.
			{output: hostManifestDigest + " " + hostManifestDigest + "\n"},
			{output: ""}, // nerdctl save
			{output: ""}, // kind load image-archive
		},
	}
	e := New(runner, nil)
	if err := e.LoadImage(context.Background(), "test-cluster", ProviderNerdctl, "my-image:local"); err != nil {
		t.Fatalf("LoadImage returned error: %v", err)
	}
	if len(runner.calls) != 3 {
		t.Fatalf("expected 3 calls, got %d: %+v", len(runner.calls), runner.calls)
	}

	saveCall := runner.calls[1]
	if saveCall.name != "nerdctl" || joinArgs(saveCall.args[:1]) != "save" {
		t.Errorf("call[1] = %s %s, want a `nerdctl save` call", saveCall.name, joinArgs(saveCall.args))
	}
	if len(saveCall.args) < 3 || saveCall.args[len(saveCall.args)-1] != "my-image:local" {
		t.Errorf("call[1] args = %v, want the image as the last argument", saveCall.args)
	}

	loadCall := runner.calls[2]
	if loadCall.name != "kind" || joinArgs(loadCall.args[:2]) != "load image-archive" {
		t.Errorf("call[2] = %s %s, want a `kind load image-archive` call", loadCall.name, joinArgs(loadCall.args))
	}
	if got := loadCall.env[kindExperimentalProviderEnv]; got != "nerdctl" {
		t.Errorf("%s = %q, want %q", kindExperimentalProviderEnv, got, "nerdctl")
	}
	// The archive path load's argument must be the same file save wrote to.
	if len(loadCall.args) < 3 || loadCall.args[2] != saveCall.args[len(saveCall.args)-2] {
		t.Errorf("kind load image-archive path %v does not match nerdctl save's -o path %v", loadCall.args, saveCall.args)
	}
}

const (
	hostManifestDigest = "sha256:35c50c1e295810dbe337b06b0753d33b4fa94ccba40b8a6152630f76a057136e"
	indexDigest        = "sha256:8090c7c4538af8e717d52f5edcf26eaf4a4afae2d3e9a25c8208b0704934954e"
)

// `nerdctl save` writes a multi-arch tag's whole manifest index, and kind
// imports it on the node with `ctr images import --all-platforms`, which then
// fails on the platform nobody pulled. LoadImage must retag the image to the
// host's own manifest first, under the same name the pod spec asks for.
func TestLoadImage_NerdctlFlattensMultiArchTag(t *testing.T) {
	runner := &fakeRunner{outputs: []fakeOutput{
		// Two lines: the tag and its digest name are both store entries.
		// Target is the index, so there is something to flatten.
		{output: indexDigest + " " + hostManifestDigest + "\n" + indexDigest + " " + hostManifestDigest + "\n"},
		{output: ""}, // pull <repo>@<manifest digest>
		{output: ""}, // tag
		{output: ""}, // nerdctl save
		{output: ""}, // kind load image-archive
	}}
	e := New(runner, nil)
	const ref = "ghcr.io/nvidia/nke/controller:v0.1.1-rc.2"
	if err := e.LoadImage(context.Background(), "c", ProviderNerdctl, ref); err != nil {
		t.Fatalf("LoadImage returned error: %v", err)
	}

	inspect := runner.calls[0]
	if inspect.name != "nerdctl" || joinArgs(inspect.args[:4]) != "image inspect --mode=native --format" {
		t.Fatalf("call[0] = %s %s, want `nerdctl image inspect --mode=native`", inspect.name, joinArgs(inspect.args))
	}

	digestRef := "ghcr.io/nvidia/nke/controller@" + hostManifestDigest
	pull := runner.calls[1]
	if pull.name != "nerdctl" || joinArgs(pull.args) != "pull "+digestRef {
		t.Errorf("call[1] = %s %s, want %q", pull.name, joinArgs(pull.args), "pull "+digestRef)
	}

	tag := runner.calls[2]
	want := "tag " + digestRef + " " + ref
	if tag.name != "nerdctl" || joinArgs(tag.args) != want {
		t.Errorf("call[2] = %s %s, want %q", tag.name, joinArgs(tag.args), want)
	}
}

// A colon before the last slash is a registry port, not a tag. Splitting it
// off as one would name a repository that does not exist.
func TestRepoOfTaggedRef(t *testing.T) {
	tests := []struct {
		ref      string
		wantRepo string
		wantOK   bool
	}{
		{"ghcr.io/nvidia/nke/controller:v0.1.1-rc.2", "ghcr.io/nvidia/nke/controller", true},
		{"cwe-profiles:local", "cwe-profiles", true},
		{"localhost:5000/nke/controller:v1", "localhost:5000/nke/controller", true},
		{"localhost:5000/nke/controller", "", false},
		{"ghcr.io/nvidia/nke/controller", "", false},
		{"ghcr.io/nvidia/nke/controller@" + hostManifestDigest, "", false},
	}
	for _, tt := range tests {
		repo, ok := repoOfTaggedRef(tt.ref)
		if repo != tt.wantRepo || ok != tt.wantOK {
			t.Errorf("repoOfTaggedRef(%q) = (%q, %v), want (%q, %v)", tt.ref, repo, ok, tt.wantRepo, tt.wantOK)
		}
	}
}

func TestRenderKindConfig_IncludesMountsAndPorts(t *testing.T) {
	body, err := renderKindConfig(Config{
		ClusterName: "test-cluster",
		NodeImage:   "kindest/node:v1.32.0",
		ExtraMounts: []Mount{
			{HostPath: "/tmp/cache", ContainerPath: "/cache", ReadOnly: true},
		},
		ExtraPortMappings: []PortMapping{
			{ContainerPort: 30080, HostPort: 8080, Protocol: "TCP"},
		},
	})
	if err != nil {
		t.Fatalf("renderKindConfig returned error: %v", err)
	}
	out := string(body)
	for _, want := range []string{
		"kind: Cluster",
		"apiVersion: kind.x-k8s.io/v1alpha4",
		"role: control-plane",
		"image: kindest/node:v1.32.0",
		"hostPath: /tmp/cache",
		"containerPath: /cache",
		"readOnly: true",
		"containerPort: 30080",
		"hostPort: 8080",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered config missing %q; got:\n%s", want, out)
		}
	}
}

func TestRenderKindConfig_OmitsEmptyOptionalFields(t *testing.T) {
	body, err := renderKindConfig(Config{ClusterName: "test-cluster"})
	if err != nil {
		t.Fatalf("renderKindConfig returned error: %v", err)
	}
	out := string(body)
	for _, unwanted := range []string{"extraMounts", "extraPortMappings", "image:"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("rendered config unexpectedly contains %q; got:\n%s", unwanted, out)
		}
	}
}

func TestDefaultConfig_ProviderDefaultsToNerdctl(t *testing.T) {
	t.Setenv("NKECTL_BOOTSTRAP_PROVIDER", "")
	cfg := DefaultConfig()
	if cfg.Provider != ProviderNerdctl {
		t.Errorf("DefaultConfig().Provider = %q, want %q (docker stays available via NKECTL_BOOTSTRAP_PROVIDER)", cfg.Provider, ProviderNerdctl)
	}
}

func TestDefaultConfig_ProviderOverride(t *testing.T) {
	t.Setenv("NKECTL_BOOTSTRAP_PROVIDER", "docker")
	cfg := DefaultConfig()
	if cfg.Provider != ProviderDocker {
		t.Errorf("DefaultConfig().Provider = %q, want %q", cfg.Provider, ProviderDocker)
	}
}
