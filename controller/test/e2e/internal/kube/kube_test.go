// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"os"
	"path/filepath"
	"testing"
)

// twoContextKubeconfig writes a two-context kubeconfig, points KUBECONFIG at it
// and returns its path.
func twoContextKubeconfig(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config")
	contents := `apiVersion: v1
kind: Config
current-context: kind-other
clusters:
  - name: kind-cwe-capi
    cluster:
      server: https://127.0.0.1:16443
  - name: kind-other
    cluster:
      server: https://127.0.0.1:16444
users:
  - name: kind-cwe-capi
    user: {}
  - name: kind-other
    user: {}
contexts:
  - name: kind-cwe-capi
    context:
      cluster: kind-cwe-capi
      user: kind-cwe-capi
  - name: kind-other
    context:
      cluster: kind-other
      user: kind-other
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing kubeconfig: %v", err)
	}
	t.Setenv("KUBECONFIG", path)
	return path
}

// A context named on its own selects out of the default kubeconfig.
func TestRESTConfigSelectsAContextFromTheDefaultKubeconfig(t *testing.T) {
	twoContextKubeconfig(t)

	restConfig, err := RESTConfig("", "kind-cwe-capi")
	if err != nil {
		t.Fatalf("RESTConfig with a context and no path: %v", err)
	}
	if want := "https://127.0.0.1:16443"; restConfig.Host != want {
		t.Fatalf("host = %q, want %q", restConfig.Host, want)
	}
}

func TestRESTConfigDefaultsToTheCurrentContext(t *testing.T) {
	twoContextKubeconfig(t)

	restConfig, err := RESTConfig("", "")
	if err != nil {
		t.Fatalf("RESTConfig with neither: %v", err)
	}
	if want := "https://127.0.0.1:16444"; restConfig.Host != want {
		t.Fatalf("host = %q, want the current-context %q", restConfig.Host, want)
	}
}

func TestRESTConfigHonoursAnExplicitPathAndContext(t *testing.T) {
	path := twoContextKubeconfig(t)
	// Somewhere useless, so only the explicit path can be answering.
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "absent"))

	restConfig, err := RESTConfig(path, "kind-cwe-capi")
	if err != nil {
		t.Fatalf("RESTConfig with both: %v", err)
	}
	if want := "https://127.0.0.1:16443"; restConfig.Host != want {
		t.Fatalf("host = %q, want %q", restConfig.Host, want)
	}
}

func TestRESTConfigRejectsAnUnknownContext(t *testing.T) {
	twoContextKubeconfig(t)

	if _, err := RESTConfig("", "kind-nope"); err == nil {
		t.Fatal("RESTConfig succeeded with an unknown context, want an error")
	}
}

func TestTrustDomainFromContextName(t *testing.T) {
	twoContextKubeconfig(t)

	tests := []struct {
		name    string
		context string
		want    string
		wantErr bool
	}{
		{name: "a named kind context", context: "kind-cwe-capi", want: "cwe-capi.nke.local"},
		{name: "no context falls back to current-context", context: "", want: "other.nke.local"},
		{name: "a context that is not a kind cluster", context: "example.teleport.sh-managed-cluster", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := TrustDomain("", tc.context)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("TrustDomain(%q) = %q, want an error", tc.context, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("TrustDomain(%q): %v", tc.context, err)
			}
			if got != tc.want {
				t.Fatalf("TrustDomain(%q) = %q, want %q", tc.context, got, tc.want)
			}
		})
	}
}
