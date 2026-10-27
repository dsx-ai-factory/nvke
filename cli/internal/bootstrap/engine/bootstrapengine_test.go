// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

// TestCreate_AlwaysSetsNerdctlProviderEnv pins that every kind invocation
// carries the provider override. kind's built-in default is Docker, so an
// unset variable silently reaches for a daemon this tool no longer supports.
func TestCreate_AlwaysSetsNerdctlProviderEnv(t *testing.T) {
	runner := &fakeRunner{
		outputs: []fakeOutput{
			{output: "No kind clusters found.\n", err: errors.New("exit status 1")}, // get clusters (Exists check)
			{output: ""},                 // create cluster
			{output: "apiVersion: v1\n"}, // get kubeconfig
		},
	}
	e := New(runner, nil)

	cluster, err := e.Create(context.Background(), Config{ClusterName: "test-cluster"})
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
	if got := createCall.env[kindExperimentalProviderEnv]; got != RuntimeCLI {
		t.Errorf("%s = %q, want %q", kindExperimentalProviderEnv, got, RuntimeCLI)
	}

	// Every kind call, not just create: a cluster created through containerd
	// is invisible to a kind call that forgot the override.
	for i, call := range runner.calls {
		if call.name != "kind" {
			continue
		}
		if got := call.env[kindExperimentalProviderEnv]; got != RuntimeCLI {
			t.Errorf("call[%d] (%s %s): %s = %q, want %q",
				i, call.name, joinArgs(call.args), kindExperimentalProviderEnv, got, RuntimeCLI)
		}
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
	_, err := e.Create(context.Background(), Config{ClusterName: "test-cluster"})
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
	_, err := e.Create(context.Background(), Config{ClusterName: "test-cluster"})
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
	if err := e.Delete(context.Background(), "test-cluster"); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}
	if len(runner.calls) != 1 || runner.calls[0].args[0] != "delete" {
		t.Fatalf("unexpected call: %+v", runner.calls)
	}
	// Delete is the one path where losing the override is silent: `kind
	// delete cluster` exits 0 on a cluster it cannot see, so the teardown
	// reports success and the bootstrap cluster leaks on every run. Every
	// other method surfaces the same regression as an error.
	if got := runner.calls[0].env[kindExperimentalProviderEnv]; got != RuntimeCLI {
		t.Errorf("%s = %q, want %q", kindExperimentalProviderEnv, got, RuntimeCLI)
	}
}

func TestDelete_RequiresClusterName(t *testing.T) {
	e := New(&fakeRunner{}, nil)
	if err := e.Delete(context.Background(), ""); err == nil {
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
			got, err := e.Exists(context.Background(), "test-cluster")
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
// Exists and Kubeconfig sent no provider override, so a cluster created
// through containerd was invisible to a later Exists/Kubeconfig call, which
// queried kind's Docker default instead. Create's own duplicate-cluster guard
// and kubeconfig fetch depend on this being right — see bootstrapengine.go's
// Create for both call sites.
func TestExists_ScopesQueryToProvider(t *testing.T) {
	runner := &fakeRunner{outputs: []fakeOutput{{output: "test-cluster\n"}}}
	e := New(runner, nil)
	if _, err := e.Exists(context.Background(), "test-cluster"); err != nil {
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
	if _, err := e.Kubeconfig(context.Background(), "test-cluster"); err != nil {
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

	path, err := e.Kubeconfig(context.Background(), "test-cluster")
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
	// kind is faked, so nothing should have created the file. Asserting its
	// absence covers any self-write, not just one that echoes kind's stderr.
	if body, err := os.ReadFile(path); err == nil {
		t.Errorf("Kubeconfig wrote %s itself; kind must write it. Contents: %q", path, body)
	}
}

func TestLoadImage_RequiresImage(t *testing.T) {
	e := New(&fakeRunner{}, nil)
	if err := e.LoadImage(context.Background(), "test-cluster", ""); err == nil {
		t.Fatal("expected an error for a missing image")
	}
}

// TestLoadImage_AlwaysUsesImageArchive guards the bug this package used to
// have: `kind load docker-image` always shells out to `docker` to read the
// image, regardless of provider, so it fails with "not present locally" for a
// nerdctl-pulled image even though it exists. LoadImage must save the image
// itself and use `kind load image-archive`, which has no such dependency.
func TestLoadImage_AlwaysUsesImageArchive(t *testing.T) {
	runner := &fakeRunner{
		outputs: []fakeOutput{
			// Target == manifest: a local image, nothing to flatten.
			{output: hostManifestDigest + " " + hostManifestDigest + "\n"},
			{output: ""}, // nerdctl save
			{output: ""}, // kind load image-archive
		},
	}
	e := New(runner, nil)
	if err := e.LoadImage(context.Background(), "test-cluster", "my-image:local"); err != nil {
		t.Fatalf("LoadImage returned error: %v", err)
	}
	if len(runner.calls) != 3 {
		t.Fatalf("expected 3 calls, got %d: %+v", len(runner.calls), runner.calls)
	}

	// The deleted Docker fast path must not come back: `kind load
	// docker-image` reads from Docker's image store, which nothing here fills.
	for _, call := range runner.calls {
		if call.name == "kind" && len(call.args) >= 2 && joinArgs(call.args[:2]) == "load docker-image" {
			t.Fatalf("LoadImage used `kind load docker-image`, which requires a Docker daemon: %v", call.args)
		}
		if call.name == "docker" {
			t.Fatalf("LoadImage shelled out to docker: %v", call.args)
		}
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
	if err := e.LoadImage(context.Background(), "c", ref); err != nil {
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
		{"localhost:5000/nvke/controller:v1", "localhost:5000/nvke/controller", true},
		{"localhost:5000/nvke/controller", "", false},
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

// An image saved under a digest name alone reaches the node with no tag, and
// the node's CRI can then resolve it through the normalized form of kind's
// import name, which containerd never stored. LoadImage must save a local tag
// beside the digest name.
func TestLoadImage_TagsDigestPinnedManifest(t *testing.T) {
	runner := &fakeRunner{outputs: []fakeOutput{
		// Target == manifest: the digest names one manifest.
		{output: hostManifestDigest + " " + hostManifestDigest + "\n"},
		{output: ""}, // tag
		{output: ""}, // nerdctl save
		{output: ""}, // kind load image-archive
	}}
	e := New(runner, nil)
	ref := "nvcr.io/org/controller@" + hostManifestDigest
	if err := e.LoadImage(context.Background(), "c", ref); err != nil {
		t.Fatalf("LoadImage returned error: %v", err)
	}
	if len(runner.calls) != 4 {
		t.Fatalf("expected 4 calls, got %d: %+v", len(runner.calls), runner.calls)
	}

	localTag := "nvcr.io/org/controller:nvkectl-35c50c1e2958"
	if got, want := joinArgs(runner.calls[1].args), "tag "+ref+" "+localTag; runner.calls[1].name != "nerdctl" || got != want {
		t.Errorf("call[1] = %s %s, want nerdctl %s", runner.calls[1].name, got, want)
	}
	save := runner.calls[2].args
	if len(save) != 5 || save[0] != "save" || save[1] != "-o" || save[3] != ref || save[4] != localTag {
		t.Fatalf("call[2] args = %v, want save -o <path> %s %s", save, ref, localTag)
	}
	if load := runner.calls[3].args; len(load) < 3 || load[2] != save[2] {
		t.Errorf("kind load image-archive args %v do not use nerdctl save's -o path %v", load, save)
	}
}

// A digest can name a multi-arch index, which was pulled for this host's
// platform only, while kind imports with `ctr images import --all-platforms`.
// LoadImage must pull and save every platform the index lists.
func TestLoadImage_SavesEveryPlatformOfPinnedIndex(t *testing.T) {
	runner := &fakeRunner{outputs: []fakeOutput{
		// Target != manifest: the digest names an index.
		{output: indexDigest + " " + hostManifestDigest + "\n"},
		{output: ""}, // pull --all-platforms
		{output: ""}, // tag
		{output: ""}, // nerdctl save
		{output: ""}, // kind load image-archive
	}}
	e := New(runner, nil)
	ref := "nvcr.io/org/controller@" + indexDigest
	if err := e.LoadImage(context.Background(), "c", ref); err != nil {
		t.Fatalf("LoadImage returned error: %v", err)
	}
	if len(runner.calls) != 5 {
		t.Fatalf("expected 5 calls, got %d: %+v", len(runner.calls), runner.calls)
	}

	if got, want := joinArgs(runner.calls[1].args), "pull --all-platforms "+ref; runner.calls[1].name != "nerdctl" || got != want {
		t.Errorf("call[1] = %s %s, want nerdctl %s", runner.calls[1].name, got, want)
	}
	localTag := "nvcr.io/org/controller:nvkectl-8090c7c4538a"
	if got, want := joinArgs(runner.calls[2].args), "tag "+ref+" "+localTag; got != want {
		t.Errorf("call[2] = %s, want %s", got, want)
	}
	save := runner.calls[3].args
	if len(save) != 6 || save[0] != "save" || save[1] != "--all-platforms" || save[2] != "-o" || save[4] != ref || save[5] != localTag {
		t.Fatalf("call[3] args = %v, want save --all-platforms -o <path> %s %s", save, ref, localTag)
	}
	if load := runner.calls[4].args; len(load) < 3 || load[2] != save[3] {
		t.Errorf("kind load image-archive args %v do not use nerdctl save's -o path %v", load, save)
	}
}

func TestLocalTagForDigestRef(t *testing.T) {
	const hex12 = "35c50c1e2958"
	tests := []struct {
		name    string
		ref     string
		wantTag string
		wantOK  bool
	}{
		{"digest only", "nvcr.io/org/controller@" + hostManifestDigest, "nvcr.io/org/controller:nvkectl-" + hex12, true},
		{"registry port", "localhost:5000/nvke/controller@" + hostManifestDigest, "localhost:5000/nvke/controller:nvkectl-" + hex12, true},
		{"tag beside digest", "nvcr.io/org/controller:v1@" + hostManifestDigest, "nvcr.io/org/controller:nvkectl-" + hex12, true},
		{"tag only", "nvcr.io/org/controller:v1", "", false},
		{"bare name", "nvcr.io/org/controller", "", false},
		{"short digest", "nvcr.io/org/controller@sha256:35c50c1e", "", false},
		{"other algorithm", "nvcr.io/org/controller@sha512:" + strings.Repeat("a", 128), "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tag, ok := localTagForDigestRef(tt.ref)
			if tag != tt.wantTag || ok != tt.wantOK {
				t.Errorf("localTagForDigestRef(%q) = (%q, %v), want (%q, %v)", tt.ref, tag, ok, tt.wantTag, tt.wantOK)
			}
		})
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

// NVKECTL_BOOTSTRAP_PROVIDER selected the container runtime front end before
// the Docker provider was removed. A stale export of it must be inert rather
// than steering the runtime, so a script that still sets it does not silently
// bootstrap against a runtime this package no longer drives.
func TestDefaultConfig_IgnoresBootstrapProviderEnv(t *testing.T) {
	t.Setenv("NVKECTL_BOOTSTRAP_PROVIDER", "docker")
	t.Setenv("NVKECTL_BOOTSTRAP_CLUSTER_NAME", "from-env")

	cfg := DefaultConfig()

	// The neighbouring variable still works, so this asserts the provider
	// variable is ignored rather than that env parsing broke wholesale.
	if cfg.ClusterName != "from-env" {
		t.Fatalf("DefaultConfig().ClusterName = %q, want %q", cfg.ClusterName, "from-env")
	}

	// Through a real call, not by reading the package's own constant back:
	// what matters is the value kind is invoked with while the stale variable
	// is set, and comparing two literals could not observe that.
	runner := &fakeRunner{outputs: []fakeOutput{{output: "No kind clusters found.\n", err: errors.New("exit status 1")}}}
	if _, err := New(runner, nil).Exists(context.Background(), "test-cluster"); err != nil {
		t.Fatalf("Exists returned error: %v", err)
	}
	if got := runner.calls[0].env[kindExperimentalProviderEnv]; got != RuntimeCLI {
		t.Errorf("%s = %q, want %q (NVKECTL_BOOTSTRAP_PROVIDER must not steer this)",
			kindExperimentalProviderEnv, got, RuntimeCLI)
	}
}

func TestRenderKindConfig_WritesTheListenAddress(t *testing.T) {
	body, err := renderKindConfig(Config{
		ClusterName: "test-cluster",
		ExtraPortMappings: []PortMapping{
			{ContainerPort: 30080, HostPort: 30080, ListenAddress: "127.0.0.1"},
			{ContainerPort: 30081, HostPort: 30081},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := string(body)
	if strings.Count(out, "listenAddress:") != 1 || !strings.Contains(out, "listenAddress: 127.0.0.1") {
		t.Errorf("want listenAddress on the first mapping only; got:\n%s", out)
	}
}

func TestKindEnvAs_IsolatesTheRuntimeCNIConfig(t *testing.T) {
	for name, tc := range map[string]struct {
		euid        int
		netconfPath string
		want        map[string]string
	}{
		"root moves the configs out of the node's CNI directory": {
			euid: 0,
			want: map[string]string{kindExperimentalProviderEnv: "nerdctl", netconfPathEnv: IsolatedCNIConfDir},
		},
		"the operator's directory wins": {
			euid:        0,
			netconfPath: "/srv/cni",
			want:        map[string]string{kindExperimentalProviderEnv: "nerdctl"},
		},
		"the shared directory is replaced, however it is spelled": {
			euid:        0,
			netconfPath: "/etc/cni/net.d/",
			want:        map[string]string{kindExperimentalProviderEnv: "nerdctl", netconfPathEnv: IsolatedCNIConfDir},
		},
		"rootless keeps its per-user directory": {
			euid: 1000,
			want: map[string]string{kindExperimentalProviderEnv: "nerdctl"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := kindEnvAs(tc.euid, tc.netconfPath)
			if len(got) != len(tc.want) {
				t.Fatalf("env = %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("%s = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestNerdctlCNIConfigs(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"nerdctl-bridge.conflist", "nerdctl-kind.conflist", "10-ovn-kubernetes.conf", "other.conflist"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got, err := NerdctlCNIConfigs(dir)
	if err != nil {
		t.Fatalf("NerdctlCNIConfigs returned error: %v", err)
	}
	want := []string{filepath.Join(dir, "nerdctl-bridge.conflist"), filepath.Join(dir, "nerdctl-kind.conflist")}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("configs = %v, want %v", got, want)
	}

	if got, err := NerdctlCNIConfigs(filepath.Join(dir, "missing")); err != nil || len(got) != 0 {
		t.Errorf("a missing directory = %v, %v; want none and no error", got, err)
	}

	// A parent that is a file fails with ENOTDIR whoever runs the test, which a
	// chmod 000 directory would not for root.
	file := filepath.Join(dir, "10-ovn-kubernetes.conf")
	if _, err := NerdctlCNIConfigs(filepath.Join(file, "net.d")); err == nil {
		t.Error("an unreadable directory must be an error, not an empty one")
	}
}
