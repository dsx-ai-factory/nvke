// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bootstrapengine

import (
	"context"
	"errors"
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
