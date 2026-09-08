// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bootstrapengine

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCreate_ConfiguresDockerHubMirror(t *testing.T) {
	for _, provider := range []Provider{ProviderDocker, ProviderNerdctl} {
		t.Run(string(provider), func(t *testing.T) {
			runner := &fakeRunner{
				outputs: []fakeOutput{
					{output: "No kind clusters found.\n", err: errors.New("exit status 1")}, // get clusters
					{output: ""}, // create cluster
					{output: ""}, // exec: docker.io hosts.toml
					{output: ""}, // exec: registry-1.docker.io hosts.toml
					{output: ""}, // export kubeconfig
				},
			}
			e := New(runner, nil)

			_, err := e.Create(context.Background(), Config{
				ClusterName:     "test-cluster",
				Provider:        provider,
				DockerHubMirror: "mirror.example.com:5000",
			})
			if err != nil {
				t.Fatalf("Create returned error: %v", err)
			}
			if len(runner.calls) != 5 {
				t.Fatalf("got %d calls, want 5: %v", len(runner.calls), runner.calls)
			}

			for i, registry := range []string{"docker.io", "registry-1.docker.io"} {
				call := runner.calls[2+i]
				if call.name != string(provider) {
					t.Errorf("call[%d] binary = %q, want %q", 2+i, call.name, provider)
				}
				args := joinArgs(call.args)
				for _, want := range []string{
					"exec test-cluster-control-plane sh -c",
					"/etc/containerd/certs.d/" + registry + "/hosts.toml",
					`server = "https://mirror.example.com:5000"`,
					`[host."https://mirror.example.com:5000"]`,
				} {
					if !strings.Contains(args, want) {
						t.Errorf("call[%d] args miss %q:\n%s", 2+i, want, args)
					}
				}
			}
			// The mirror is in place before anything else touches the cluster.
			if last := runner.calls[4]; last.name != "kind" || last.args[0] != "export" {
				t.Errorf("call[4] = %s %s, want the kubeconfig export after the mirror setup", last.name, joinArgs(last.args))
			}
		})
	}
}

func TestCreate_NoMirrorMeansNoExec(t *testing.T) {
	runner := &fakeRunner{
		outputs: []fakeOutput{
			{output: "No kind clusters found.\n", err: errors.New("exit status 1")},
			{output: ""},
			{output: ""},
		},
	}
	e := New(runner, nil)
	if _, err := e.Create(context.Background(), Config{ClusterName: "test-cluster", Provider: ProviderDocker}); err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	for _, call := range runner.calls {
		if call.name != "kind" {
			t.Errorf("unexpected non-kind call without a mirror: %s %s", call.name, joinArgs(call.args))
		}
	}
}

func TestCreate_RejectsAMalformedMirror(t *testing.T) {
	runner := &fakeRunner{}
	e := New(runner, nil)
	_, err := e.Create(context.Background(), Config{
		ClusterName:     "test-cluster",
		Provider:        ProviderDocker,
		DockerHubMirror: "https://mirror.example.com/v2/",
	})
	if err == nil {
		t.Fatal("Create accepted a mirror with a scheme and a path")
	}
	if len(runner.calls) != 0 {
		t.Errorf("Create ran %d commands before rejecting the mirror, want 0", len(runner.calls))
	}
}

func TestDefaultConfig_DockerHubMirrorFromEnv(t *testing.T) {
	t.Setenv("NKECTL_BOOTSTRAP_DOCKERHUB_MIRROR", "mirror.example.com")
	if got := DefaultConfig().DockerHubMirror; got != "mirror.example.com" {
		t.Errorf("DockerHubMirror = %q, want %q", got, "mirror.example.com")
	}
	t.Setenv("NKECTL_BOOTSTRAP_DOCKERHUB_MIRROR", "")
	if got := DefaultConfig().DockerHubMirror; got != "" {
		t.Errorf("DockerHubMirror = %q, want empty when unset", got)
	}
}
